package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/blob"
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// content returns n bytes that differ by seed.
func content(seed, n int) []byte {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func blobBytes(seed, n int) value.Bytes {
	return must(value.NewBytes(content(seed, n), "application/octet-stream"))
}

func defineFiles(t *testing.T, db *DB, history bool) {
	t.Helper()
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "File", History: model.History{On: history},
			Fields: []model.FieldDef{fd("name", "text"), fd("data", "bytes?"), fd("body", "text?"), fd("pic", "image?")}})
		return err
	})
}

func fieldValue(t *testing.T, db *DB, id value.ID, name string) value.Value {
	t.Helper()
	var v value.Value
	err := db.View(func(tx *Tx) error {
		var err error
		_, v, _, err = tx.EntityField(id, name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rawField(t *testing.T, db *DB, id value.ID, name string) value.Value {
	t.Helper()
	var v value.Value
	err := db.View(func(tx *Tx) error {
		b, _, err := tx.get(keys.Field(id, name))
		if err != nil {
			return err
		}
		f, err := keys.DecodeField(id, name, b, tx.Catalog())
		v = f.Value
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func blobStats(t *testing.T, db *DB) BlobStats {
	t.Helper()
	var st BlobStats
	if err := db.View(func(tx *Tx) (err error) { st, err = tx.BlobStats(); return }); err != nil {
		t.Fatal(err)
	}
	return st
}

func collect(t *testing.T, db *DB) BlobResult {
	t.Helper()
	r, err := db.CollectBlobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// loaded loads a lazy value.
func loaded(t *testing.T, v value.Value) value.Value {
	t.Helper()
	v, err := value.Load(v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// sameVal compares values, loading got if it is lazy.
func sameVal(t *testing.T, got, want value.Value) {
	t.Helper()
	got = loaded(t, got)
	if !value.Equal(got, want) || got.Code() != want.Code() {
		t.Fatalf("got %.60s, want %.60s", got, want)
	}
}

// pngOf returns a valid PNG of at least n bytes.
func pngOf(n int) value.Image {
	// A minimal 1×1 PNG; trailing bytes after IEND are ignored by
	// decoders, so padding makes it as large as wanted.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
		"\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa7\x35\x81\x84\x00\x00\x00\x00IEND\xaeB`\x82")
	return must(value.NewImage(append(png, content(99, n)...)))
}

func TestBlobRoundTrip(t *testing.T) {
	db := newDB(t)
	defineFiles(t, db, false)
	data := blobBytes(1, 3<<20+123) // four chunks, the last partial
	body := txt(strings.Repeat("ünïcode ", 4000))
	pic := pngOf(40 << 10)
	small := blobBytes(2, 100)
	a := create(t, db, "File", Fields{"name": txt("a"), "data": data, "body": body, "pic": pic})
	b := create(t, db, "File", Fields{"name": txt("b"), "data": small})

	sameVal(t, fieldValue(t, db, a, "data"), data)
	sameVal(t, fieldValue(t, db, a, "body"), body)
	sameVal(t, fieldValue(t, db, a, "pic"), pic)
	sameVal(t, fieldValue(t, db, b, "data"), small)
	// Large values read back lazily; small ones as they are.
	lz, ok := fieldValue(t, db, a, "data").(value.Lazy)
	if !ok || lz.Len() != int64(data.Len()) || lz.Code() != value.CodeBytes {
		t.Fatalf("large value read as %T", fieldValue(t, db, a, "data"))
	}
	if _, ok := fieldValue(t, db, b, "data").(value.Bytes); !ok {
		t.Fatal("small value read lazily")
	}
	if got := loaded(t, fieldValue(t, db, a, "data")).(value.Bytes).MediaType(); got != "application/octet-stream" {
		t.Fatalf("media type %q", got)
	}
	if n, err := fieldValue(t, db, a, "body").(value.Lazy).RuneCount(); err != nil || n != int64(body.Len()) {
		t.Fatalf("rune count %d, %v; want %d", n, err, body.Len())
	}
	e := get(t, db, a)
	f, _ := e.Field("pic")
	if img := loaded(t, f.Value).(value.Image); img.Format() != "png" || img.Width() != 1 {
		t.Fatalf("image %s %d", img.Format(), img.Width())
	}

	// Large values are references in storage, small ones inline.
	for _, name := range []string{"data", "body", "pic"} {
		if r, ok := rawField(t, db, a, name).(value.BlobRef); !ok || r.Len != int64(value.LargeLen(loaded(t, fieldValue(t, db, a, name)))) {
			t.Fatalf("%s stored as %.40s", name, rawField(t, db, a, name))
		}
	}
	if _, ok := rawField(t, db, b, "data").(value.Bytes); !ok {
		t.Fatal("small value not inline")
	}
	st := blobStats(t, db)
	if st.Blobs != 3 || st.Chunks != 6 {
		t.Fatalf("stats %+v, want 3 blobs and 6 chunks", st)
	}

	// The same content again is stored once.
	before := st.FileBytes
	create(t, db, "File", Fields{"name": txt("c"), "data": data})
	if st := blobStats(t, db); st.Blobs != 3 || st.FileBytes != before {
		t.Fatalf("duplicate content stored again: %+v", st)
	}
	// Content sharing chunks with another blob stores only the new chunk.
	ext := must(value.NewBytes(append(data.Go(), 'x'), ""))
	create(t, db, "File", Fields{"name": txt("d"), "data": ext})
	if st := blobStats(t, db); st.Blobs != 4 || st.Chunks != 7 {
		t.Fatalf("extended content: %+v, want 4 blobs, 7 chunks", st)
	}
}

func TestBlobCollect(t *testing.T) {
	db := newDB(t)
	defineFiles(t, db, false)
	shared := blobBytes(1, 50<<10)
	a := create(t, db, "File", Fields{"name": txt("a"), "data": shared})
	b := create(t, db, "File", Fields{"name": txt("b"), "data": shared})
	c := create(t, db, "File", Fields{"name": txt("c"), "data": blobBytes(2, 50<<10)})
	if r := collect(t, db); r != (BlobResult{}) {
		t.Fatalf("collected %+v with everything in use", r)
	}

	// Overwriting c's value frees its blob; changing an unrelated field
	// frees nothing.
	update(t, db, func(tx *Tx) error { return tx.Set(c, Fields{"data": blobBytes(3, 50<<10)}) })
	update(t, db, func(tx *Tx) error { return tx.Set(a, Fields{"name": txt("a2")}) })
	if r := collect(t, db); r.Blobs != 1 || r.Chunks != 1 || r.Uses != 1 {
		t.Fatalf("after overwrite: %+v", r)
	}
	// Deleting one of two users of a blob keeps it.
	update(t, db, func(tx *Tx) error { return tx.Delete(a) })
	if r := collect(t, db); r.Blobs != 0 {
		t.Fatalf("shared blob collected: %+v", r)
	}
	sameVal(t, fieldValue(t, db, b, "data"), shared)
	// Unsetting the last use frees it.
	update(t, db, func(tx *Tx) error { return tx.Unset(b, "data") })
	if r := collect(t, db); r.Blobs != 1 {
		t.Fatalf("after unset: %+v", r)
	}
	update(t, db, func(tx *Tx) error { return tx.Delete(c) })
	collect(t, db)
	if st := blobStats(t, db); st.Blobs != 0 || st.Chunks != 0 {
		t.Fatalf("left %+v", st)
	}
	// Nothing is left marked.
	err := db.View(func(tx *Tx) error {
		for _, p := range []byte{keys.PrefixBlobUse, keys.PrefixBlobEnt, keys.PrefixBlobDirty, keys.PrefixBlobCheck} {
			if n, err := tx.count(keys.Prefix(p)); err != nil || n != 0 {
				return fmt.Errorf("prefix %#x: %d records, %v", p, n, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBlobHistory(t *testing.T) {
	var now atomic.Int64
	now.Store(clock.UnixNano())
	eng := must(Open(Options{Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }, HistorySweep: -1}))
	defer eng.Close()
	db := must(eng.CreateDatabase("test"))
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "File", History: model.History{On: true, Keep: time.Hour},
			Fields: []model.FieldDef{fd("data", "bytes?")}})
		return err
	})
	v1, v2 := blobBytes(1, 40<<10), blobBytes(2, 40<<10)
	id := create(t, db, "File", Fields{"data": v1})
	now.Add(int64(time.Minute))
	update(t, db, func(tx *Tx) error { return tx.Set(id, Fields{"data": v2}) })
	if r := collect(t, db); r.Blobs != 0 {
		t.Fatalf("collected a blob history still holds: %+v", r)
	}
	var vs []Version
	if err := db.View(func(tx *Tx) (err error) { vs, err = tx.FieldHistory(id, "data"); return }); err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("%d versions", len(vs))
	}
	sameVal(t, vs[0].Value, v1)
	sameVal(t, vs[1].Value, v2)
	err := db.View(func(tx *Tx) error {
		v, ok, err := tx.AsOf(must(value.NewTime(clock))).Field(id, "data")
		if err != nil || !ok {
			return fmt.Errorf("as of: %v %v", ok, err)
		}
		sameVal(t, v, v1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Past retention the sweep drops the old version, and the blob goes.
	now.Add(int64(2 * time.Hour))
	if _, err := db.SweepHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := collect(t, db); r.Blobs != 1 {
		t.Fatalf("after sweep: %+v", r)
	}
	sameVal(t, fieldValue(t, db, id, "data"), v2)
	// Deleting the entity keeps its history, and so the blob, until that
	// is past retention too.
	update(t, db, func(tx *Tx) error { return tx.Delete(id) })
	if r := collect(t, db); r.Blobs != 0 {
		t.Fatalf("collected a deleted entity's history: %+v", r)
	}
	now.Add(int64(2 * time.Hour))
	if _, err := db.SweepHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := collect(t, db); r.Blobs != 1 {
		t.Fatalf("after the deleted entity's sweep: %+v", r)
	}
}

func TestBlobRewriteAndSnapshots(t *testing.T) {
	eng := must(Open(Options{Now: func() time.Time { return clock }, HistorySweep: -1, BlobSegmentSize: 1 << 20}))
	defer eng.Close()
	db := must(eng.CreateDatabase("test"))
	defineFiles(t, db, false)
	var ids []value.ID
	for i := range 40 {
		ids = append(ids, create(t, db, "File", Fields{"name": txt("f"), "data": blobBytes(i, 100<<10)}))
	}
	full := blobStats(t, db)
	if full.Segments < 4 {
		t.Fatalf("%d segments", full.Segments)
	}
	// A reader that began before the deletions keeps seeing everything.
	old := db.Begin(TxOptions{ReadOnly: true})
	for i, id := range ids {
		if i%5 != 0 {
			update(t, db, func(tx *Tx) error { return tx.Delete(id) })
		}
	}
	r := collect(t, db)
	if r.Blobs != 32 || r.Moved == 0 || r.Segments != 0 {
		t.Fatalf("collected %+v; want 32 blobs, moves, no segment deleted yet", r)
	}
	for i, id := range ids {
		_, v, _, err := old.EntityField(id, "data")
		if err != nil {
			t.Fatalf("old snapshot, file %d: %v", i, err)
		}
		sameVal(t, v, blobBytes(i, 100<<10))
	}
	old.Rollback()
	if r := collectUnpinned(t, db); r.Segments == 0 {
		t.Fatalf("no segment deleted once the reader ended: %+v", r)
	}
	st := blobStats(t, db)
	if st.FileBytes > full.FileBytes/2 {
		t.Fatalf("files still %d bytes of %d", st.FileBytes, full.FileBytes)
	}
	for i, id := range ids {
		if i%5 == 0 {
			sameVal(t, fieldValue(t, db, id, "data"), blobBytes(i, 100<<10))
		}
	}
}

func TestBlobReaderPins(t *testing.T) {
	eng := must(Open(Options{Now: func() time.Time { return clock }, HistorySweep: -1, BlobSegmentSize: 1 << 20}))
	defer eng.Close()
	db := must(eng.CreateDatabase("test"))
	defineFiles(t, db, false)
	data := blobBytes(1, 1<<20+300<<10)
	id := create(t, db, "File", Fields{"name": txt("a"), "data": data})
	for i := range 10 { // fill and seal the segment
		create(t, db, "File", Fields{"name": txt("x"), "data": blobBytes(i+10, 300<<10)})
	}
	r, ok, err := db.OpenBlob(sha256.Sum256(data.Go()))
	if err != nil || !ok || r.Len != int64(data.Len()) {
		t.Fatalf("open: %v %v", ok, err)
	}
	if _, ok, _ := db.OpenBlob(sha256.Sum256(content(5, 100<<10))); ok {
		t.Fatal("opened content the store doesn't hold")
	}
	update(t, db, func(tx *Tx) error { return tx.Delete(id) })
	collect(t, db)
	collect(t, db)
	part, err := r.Read(1<<20-10, 20)
	if err != nil {
		t.Fatalf("pinned read: %v", err)
	}
	if !bytes.Equal(part, data.Go()[1<<20-10:1<<20+10]) {
		t.Fatal("pinned read returned the wrong bytes")
	}
	r.Close()
	if r := collectUnpinned(t, db); r.Segments == 0 {
		t.Fatalf("segment not deleted after unpin: %+v", r)
	}
}

// collectUnpinned collects after letting the garbage collector release
// the segments that lazy values read in earlier transactions pinned,
// trying a few times since cleanups run in the background.
func collectUnpinned(t *testing.T, db *DB) BlobResult {
	t.Helper()
	var total BlobResult
	for range 20 {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
		r := collect(t, db)
		total.Uses += r.Uses
		total.Blobs += r.Blobs
		total.Chunks += r.Chunks
		total.Moved += r.Moved
		if total.Segments += r.Segments; r.Segments > 0 {
			break
		}
	}
	return total
}

func TestBlobUpload(t *testing.T) {
	db := newDB(t)
	defineFiles(t, db, false)
	data := content(1, 2<<20+5)
	u, err := db.StoreUpload(bytes.NewReader(data), value.CodeBytes, 10<<20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StoreUpload(bytes.NewReader(data), value.CodeBytes, 1<<20, time.Minute); err == nil {
		t.Fatal("upload over the limit accepted")
	}
	got, err := db.ReadUpload(u, 0, u.Len)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("upload content: %v", err)
	}
	before := blobStats(t, db).FileBytes
	id := create(t, db, "File", Fields{"name": txt("a"), "data": must(value.NewBytes(got, ""))})
	if after := blobStats(t, db).FileBytes; after != before {
		t.Fatalf("stored uploaded content again: %d → %d bytes", before, after)
	}
	sameVal(t, fieldValue(t, db, id, "data"), must(value.NewBytes(data, "")))

	// As a lazy value, an upload is stored without being read.
	u2, err := db.StoreUpload(bytes.NewReader(content(2, 3<<20)), value.CodeBytes, 10<<20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lv, err := db.UploadValue(u2, "application/x-test")
	if err != nil {
		t.Fatal(err)
	}
	before = blobStats(t, db).FileBytes
	id2 := create(t, db, "File", Fields{"name": txt("b"), "data": lv})
	if after := blobStats(t, db).FileBytes; after != before {
		t.Fatalf("stored a lazy upload again: %d → %d bytes", before, after)
	}
	got2 := loaded(t, fieldValue(t, db, id2, "data")).(value.Bytes)
	if !bytes.Equal(got2.Go(), content(2, 3<<20)) || got2.MediaType() != "application/x-test" {
		t.Fatal("lazy upload stored wrong")
	}
}

// Uploaded text is normalized to NFC and checked to be UTF-8, also
// across chunk boundaries; uploaded images have their header checked.
func TestBlobUploadChecks(t *testing.T) {
	db := newDB(t)
	defineFiles(t, db, false)
	upload := func(data []byte, of value.Code) (value.Value, error) {
		t.Helper()
		u, err := db.StoreUpload(bytes.NewReader(data), of, 10<<20, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return db.UploadValue(u, "")
	}
	// "é" as e + combining acute, with a two-byte character straddling
	// the first chunk boundary.
	decomposed := strings.Repeat("e\u0301", 1<<19)
	straddle := strings.Repeat("a", blob.ChunkSize-1) + "ü" + strings.Repeat("b", 30<<10)
	for _, s := range []string{decomposed, straddle} {
		v, err := upload([]byte(s), value.CodeText)
		if err != nil {
			t.Fatal(err)
		}
		id := create(t, db, "File", Fields{"name": txt("t"), "body": v})
		sameVal(t, fieldValue(t, db, id, "body"), txt(s)) // txt normalizes too
	}
	bad := []byte(strings.Repeat("a", blob.ChunkSize-1) + "\xc3" + strings.Repeat("b", 10))
	if _, err := upload(bad, value.CodeText); err == nil {
		t.Fatal("invalid UTF-8 across chunks accepted")
	}
	if _, err := upload([]byte(strings.Repeat("a", 30<<10)+"\xe2\x82"), value.CodeText); err == nil {
		t.Fatal("text ending in a partial character accepted")
	}
	if _, err := upload(content(3, 40<<10), value.CodeImage); err == nil {
		t.Fatal("non-image accepted as an image")
	}
	img := pngOf(40 << 10)
	v, err := upload(img.Data(), value.CodeImage)
	if err != nil {
		t.Fatal(err)
	}
	id := create(t, db, "File", Fields{"name": txt("i"), "pic": v})
	if got := loaded(t, fieldValue(t, db, id, "pic")).(value.Image); got.Format() != "png" {
		t.Fatalf("image format %q", got.Format())
	}
}

// A lazy value stored in another database is copied there a chunk at a
// time; one whose blob the collector removed after it was read is still
// readable from the segments it pins, and is stored again.
func TestBlobLazyCopies(t *testing.T) {
	eng := must(Open(Options{Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
	defer eng.Close()
	a, b := must(eng.CreateDatabase("a")), must(eng.CreateDatabase("b"))
	defineFiles(t, a, false)
	defineFiles(t, b, false)
	data := blobBytes(1, 2<<20+77)
	ida := create(t, a, "File", Fields{"name": txt("x"), "data": data})
	lv := fieldValue(t, a, ida, "data")
	idb := create(t, b, "File", Fields{"name": txt("y"), "data": lv})
	sameVal(t, fieldValue(t, b, idb, "data"), data)
	if st := blobStats(t, b); st.Blobs != 1 || st.Chunks != 3 {
		t.Fatalf("copy into b: %+v", st)
	}

	// Delete the only use in a, collect, then store the old lazy value.
	update(t, a, func(tx *Tx) error { return tx.Delete(ida) })
	collect(t, a)
	if st := blobStats(t, a); st.Blobs != 0 {
		t.Fatalf("blob not collected: %+v", st)
	}
	id := create(t, a, "File", Fields{"name": txt("again"), "data": lv})
	sameVal(t, fieldValue(t, a, id, "data"), data)
}

func TestBlobDurable(t *testing.T) {
	fs := vfs.NewMem()
	o := Options{Dir: "/data", FS: fs, Now: func() time.Time { return clock }, HistorySweep: -1}
	eng := must(Open(o))
	db := must(eng.CreateDatabase("test"))
	defineFiles(t, db, false)
	data := blobBytes(1, 2<<20)
	id := create(t, db, "File", Fields{"name": txt("a"), "data": data})
	// A transaction that appends chunks but never commits leaves only
	// garbage.
	tx := db.Begin(TxOptions{})
	if _, err := tx.Create("File", Fields{"name": txt("b"), "data": blobBytes(2, 1<<20)}); err != nil {
		t.Fatal(err)
	}
	crashed := fs.Crash(rand.New(rand.NewPCG(1, 1)))
	tx.Rollback()
	eng.Close()
	o.FS = crashed
	eng = must(Open(o))
	defer eng.Close()
	db = must(eng.Database("test"))
	sameVal(t, fieldValue(t, db, id, "data"), data)
	if st := blobStats(t, db); st.Blobs != 1 {
		t.Fatalf("after crash: %+v", st)
	}
}

// TestBlobConcurrent runs writers that store, share, replace and delete
// large values while the collector runs nonstop, then checks every
// remaining value, and that removing them all leaves nothing behind.
func TestBlobConcurrent(t *testing.T) {
	eng := must(Open(Options{Now: func() time.Time { return clock }, HistorySweep: -1, BlobSegmentSize: 512 << 10}))
	defer eng.Close()
	db := must(eng.CreateDatabase("test"))
	defineFiles(t, db, false)
	const files, pool = 60, 12
	ids := make([]value.ID, files)
	var mu sync.Mutex
	want := make([]int, files) // content seed per file, -1 deleted
	for i := range files {
		ids[i] = create(t, db, "File", Fields{"name": txt("f"), "data": blobBytes(i%pool, 20<<10+i%pool*7000)})
		want[i] = i % pool
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 9))
			for !stop.Load() {
				i := rng.IntN(files)
				seed := rng.IntN(pool)
				var err error
				mu.Lock()
				deleted := want[i] < 0
				mu.Unlock()
				switch {
				case deleted:
					var id value.ID
					err = db.Update(func(tx *Tx) (err error) {
						id, err = tx.Create("File", Fields{"name": txt("f"), "data": blobBytes(seed, 20<<10+seed*7000)})
						return err
					})
					if err == nil {
						mu.Lock()
						ids[i], want[i] = id, seed
						mu.Unlock()
					}
				case rng.IntN(5) == 0:
					err = db.Update(func(tx *Tx) error { return tx.Delete(ids[i]) })
					if err == nil {
						mu.Lock()
						want[i] = -1
						mu.Unlock()
					}
				default:
					err = db.Update(func(tx *Tx) error {
						return tx.Set(ids[i], Fields{"data": blobBytes(seed, 20<<10+seed*7000)})
					})
					if err == nil {
						mu.Lock()
						want[i] = seed
						mu.Unlock()
					}
				}
				if err != nil && !IsConflict(err) && CodeOf(err) != CodeNotFound {
					t.Error(err)
					return
				}
			}
		})
	}
	var total BlobResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := collect(t, db)
		total.Blobs += r.Blobs
		total.Moved += r.Moved
		total.Segments += r.Segments
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("collector during the run: %+v", total)
	collect(t, db)
	err := db.View(func(tx *Tx) error {
		return tx.Entities("File", func(id value.ID) bool {
			_, v, _, err := tx.EntityField(id, "data")
			if err != nil {
				t.Errorf("entity %s: %v", id, err)
				return false
			}
			v = loaded(t, v)
			n := value.LargeLen(v)
			seed := (n - 20<<10) / 7000
			sameVal(t, v, blobBytes(seed, n))
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error {
		return tx.Entities("File", func(id value.ID) bool { return tx.Delete(id) == nil })
	})
	for range 2 {
		r := collect(t, db)
		total.Blobs += r.Blobs
		total.Moved += r.Moved
		total.Segments += r.Segments
	}
	if st := blobStats(t, db); st.Blobs != 0 || st.Chunks != 0 {
		t.Fatalf("left %+v", st)
	}
	// Under a slow run the collector may get few rounds in while writers
	// run, but over the whole test it must have freed and deleted files.
	if total.Blobs == 0 || total.Segments == 0 {
		t.Errorf("collector removed little: %+v", total)
	}
}

// BenchmarkBlobs writes and reads 1 MiB values durably on disk (set
// TMPDIR to a real disk: /tmp is often in memory).
func BenchmarkBlobs(b *testing.B) {
	eng := must(Open(Options{Dir: b.TempDir(), Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
	defer eng.Close()
	db := must(eng.CreateDatabase("bench"))
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "File", Fields: []model.FieldDef{fd("data", "bytes")}})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	const size = 1 << 20
	base := content(1, size)
	var ids []value.ID
	b.Run("write", func(b *testing.B) {
		b.SetBytes(size)
		for i := 0; b.Loop(); i++ {
			data := append([]byte(nil), base...)
			data[0], data[1], data[2] = byte(i), byte(i>>8), byte(i>>16) // distinct content
			err := db.Update(func(tx *Tx) error {
				id, err := tx.Create("File", Fields{"data": must(value.NewBytes(data, ""))})
				ids = append(ids, id)
				return err
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read", func(b *testing.B) {
		b.SetBytes(size)
		for i := 0; b.Loop(); i++ {
			if err := db.View(func(tx *Tx) error {
				_, _, _, err := tx.EntityField(ids[i%len(ids)], "data")
				return err
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("inline-write", func(b *testing.B) {
		// The same values stored inline in the key-value store, for
		// comparison.
		eng2 := must(Open(Options{Dir: b.TempDir(), Now: func() time.Time { return clock }, HistorySweep: -1,
			StatsRefresh: -1, BlobThreshold: 1 << 30}))
		defer eng2.Close()
		db2 := must(eng2.CreateDatabase("bench"))
		db2.Update(func(tx *Tx) error {
			_, err := tx.DefineKind(model.Kind{Name: "File", Fields: []model.FieldDef{fd("data", "bytes")}})
			return err
		})
		b.SetBytes(size)
		for i := 0; b.Loop(); i++ {
			data := append([]byte(nil), base...)
			data[0], data[1], data[2] = byte(i), byte(i>>8), byte(i>>16)
			if err := db2.Update(func(tx *Tx) error {
				_, err := tx.Create("File", Fields{"data": must(value.NewBytes(data, ""))})
				return err
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
