package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/sandgorgon/polydb/blob"
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
	"golang.org/x/text/unicode/norm"
)

// Large values (spec/types.md §13). A text, bytes or image field value
// longer than the blob threshold is stored in the database's blob store
// (package blob): its content is split into chunks, and the field record
// (or history version) holds a value.BlobRef instead. Reads replace the
// reference with a value.Lazy that reads the content when something
// needs it (Tx.lazy), so nothing above the engine sees a reference, and
// a value passed along (shown, stored elsewhere) is never loaded.
//
// What the key-value store records (keys/blob.go):
//
//   - a blob record per distinct content, listing its chunks, and a
//     location record per distinct chunk: identical content is stored
//     once;
//   - a use record for each entity that references a blob, from a field
//     or a history version. Writers only add use records, each for its
//     own entity, so writers of the same popular content never conflict;
//   - a dirty mark on an entity that may have dropped a reference (a
//     field overwritten or removed, the entity deleted, history pruned).
//
// The collector (CollectBlobs, run with the history sweep) checks dirty
// entities, removes the use records nothing backs any more, deletes the
// blobs left without uses, deletes chunks no blob lists, and rewrites
// segments that are mostly garbage. Physical deletion waits until no open
// snapshot and no download lease can read the old segment.
//
// Concurrency rests on the transaction manager's conflict checks:
//
//   - A writer reusing an existing blob guards its record; the collector
//     deleting it guards the blob's use records, which the writer adds
//     to. Whichever commits second fails.
//   - A writer reusing an existing chunk for a new blob writes the
//     chunk's location record again, so it conflicts with the collector
//     deleting or moving that chunk.
//   - A writer using a chunk uploaded earlier (blob.Store.Pending) writes
//     its segment's retire mark, which the collector writes too when it
//     empties the segment.
//   - Chunks a writer appends go to the active segment. The collector
//     only rewrites a segment once every transaction open when a chunk
//     was last appended to it has ended (DB.touchSeg), so it never misses
//     chunks whose records are still to commit.

const (
	// DefaultBlobThreshold is the length above which values go to the
	// blob store.
	DefaultBlobThreshold = 16 << 10
	// minBlobThreshold is the least Options.BlobThreshold can be.
	minBlobThreshold = 4 << 10
	// rewriteLive is the share of a sealed segment that must still be live
	// for it to be kept as it is.
	rewriteLive = 0.5
	// DefaultMaxLoad is Options.MaxLoad's default.
	DefaultMaxLoad = 256 << 20
)

// blobSegKey is the retire mark of a blob segment: the collector sets it
// in the transaction that empties the segment.
func blobSegKey(seg uint32) []byte { return keys.Meta(fmt.Sprintf("blobseg/%d", seg)) }

func openBlobs(e *Engine, dir string) (*blob.Store, error) {
	o := blob.Options{FS: e.opts.FS, Dir: dir, SegmentSize: e.opts.BlobSegmentSize}
	if dir == "" {
		o.FS, o.Dir, o.NoSync = vfs.NewMem(), "/blobs", true
	}
	s, err := blob.Open(o)
	return s, wrap(CodeInternal, err)
}

func (db *DB) blobThreshold() int { return db.eng.opts.BlobThreshold }

// storeValue returns what a stored record of entity's field holds for v:
// v itself, or a reference after putting v's content in the blob store.
// It records the entity's use of the blob. A lazy value's content is
// copied a chunk at a time, or not at all when this store has it.
func (tx *Tx) storeValue(entity value.ID, v value.Value) (value.Value, error) {
	var ref value.BlobRef
	switch x := v.(type) {
	case value.BlobRef:
		return nil, errorf(CodeType, "a blob reference is not a value")
	case value.Lazy:
		if x.Ref.Len <= int64(tx.db.blobThreshold()) {
			lv, err := x.Load()
			if err != nil {
				return nil, wrap(CodeInternal, err)
			}
			return tx.storeValue(entity, lv)
		}
		if err := tx.putBlob(x.Ref.Hash, x.Ref.Len, lazyChunks(x)); err != nil {
			return nil, err
		}
		ref = x.Ref
	default:
		content, media, ok := value.Large(v)
		if !ok || len(content) <= tx.db.blobThreshold() {
			return v, nil
		}
		h := sha256.Sum256(content)
		if err := tx.putBlob(h, int64(len(content)), contentChunks(content)); err != nil {
			return nil, err
		}
		ref = value.BlobRef{Of: v.Code(), Hash: h, Len: int64(len(content)), Media: media}
	}
	tx.t.Set(keys.BlobUse(ref.Hash, entity), nil)
	tx.t.Set(keys.BlobEnt(entity, ref.Hash), nil)
	return ref, nil
}

// chunkFunc returns a blob's chunk i: its hash, and a function reading
// its data, called only if the store doesn't have the chunk.
type chunkFunc func(i int) (h [32]byte, data func() ([]byte, error), err error)

// contentChunks splits content in memory into chunks.
func contentChunks(content []byte) chunkFunc {
	return func(i int) ([32]byte, func() ([]byte, error), error) {
		data := content[i*blob.ChunkSize : min((i+1)*blob.ChunkSize, len(content))]
		return sha256.Sum256(data), func() ([]byte, error) { return data, nil }, nil
	}
}

// lazyChunks reads a lazy value's chunks. Content in a blob store comes
// with its chunk hashes, so a chunk this store has isn't read at all;
// other content is read a chunk at a time.
func lazyChunks(l value.Lazy) chunkFunc {
	if r, ok := l.Src.(*BlobReader); ok {
		return func(i int) ([32]byte, func() ([]byte, error), error) {
			h := r.rec.Chunks[i]
			return h, func() ([]byte, error) { return r.db.blobs.Get(h, r.locs[i]) }, nil
		}
	}
	buf := make([]byte, blob.ChunkSize)
	return func(i int) ([32]byte, func() ([]byte, error), error) {
		off := int64(i) * blob.ChunkSize
		data := buf[:min(int64(blob.ChunkSize), l.Ref.Len-off)]
		if _, err := l.Src.ReadAt(data, off); err != nil {
			return [32]byte{}, nil, err
		}
		return sha256.Sum256(data), func() ([]byte, error) { return data, nil }, nil
	}
}

// putBlob makes sure blob h, n bytes in the chunks chunk returns, is
// stored.
func (tx *Tx) putBlob(h [32]byte, n int64, chunk chunkFunc) error {
	if tx.blobsPut[h] {
		return nil
	}
	bk := keys.Blob(h)
	if _, ok, err := tx.get(bk); err != nil {
		return err
	} else if ok {
		tx.t.GuardKey(bk) // against the collector deleting it
		tx.markBlobPut(h)
		return nil
	}
	store := tx.db.blobs
	rec := keys.BlobRecord{Len: n}
	for i := 0; int64(i)*blob.ChunkSize < n; i++ {
		ch, data, err := chunk(i)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		rec.Chunks = append(rec.Chunks, ch)
		ck := keys.Chunk(ch)
		cur, ok, err := tx.get(ck)
		if err != nil {
			return err
		}
		if ok {
			// Writing the location again makes a collector deleting or
			// moving the chunk conflict with this transaction.
			tx.t.Set(ck, cur)
			continue
		}
		loc, uploaded := store.Pending(ch)
		if uploaded {
			tx.t.Set(blobSegKey(loc.Seg), nil)
		} else {
			d, err := data()
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if loc, err = tx.db.putChunk(ch, d); err != nil {
				return tx.fail(err)
			}
			tx.blobSync = append(tx.blobSync, loc)
		}
		tx.t.Set(ck, keys.EncodeChunk(keys.ChunkLoc(loc)))
	}
	tx.t.Set(bk, keys.EncodeBlob(rec))
	tx.markBlobPut(h)
	return nil
}

// putChunk appends a chunk to the active segment, noting when.
func (db *DB) putChunk(h [32]byte, data []byte) (blob.Loc, error) {
	loc, err := db.blobs.Put(h, data)
	if err != nil {
		return loc, wrap(CodeInternal, err)
	}
	db.touchSeg(loc.Seg)
	return loc, nil
}

// touchSeg records that a transaction open now may have appended to seg.
func (db *DB) touchSeg(seg uint32) {
	seq := db.mgr.Store().Seq()
	db.touchMu.Lock()
	defer db.touchMu.Unlock()
	if db.touched == nil {
		db.touched = map[uint32]uint64{}
	}
	db.touched[seg] = max(db.touched[seg], seq)
}

// settled reports whether every transaction that may have appended to
// seg has ended: every open snapshot is newer than its last append.
func (db *DB) settled(seg uint32, oldest uint64) bool {
	db.touchMu.Lock()
	defer db.touchMu.Unlock()
	t, ok := db.touched[seg]
	if ok && t < oldest {
		delete(db.touched, seg)
		return true
	}
	return !ok
}

func (tx *Tx) markBlobPut(h [32]byte) {
	if tx.blobsPut == nil {
		tx.blobsPut = map[[32]byte]bool{}
	}
	tx.blobsPut[h] = true
}

// syncBlobs makes the chunks the transaction wrote durable. Commit calls
// it before committing the records that reference them.
func (tx *Tx) syncBlobs() error {
	for _, loc := range tx.blobSync {
		if err := tx.db.blobs.Sync(loc); err != nil {
			return wrap(CodeInternal, err)
		}
	}
	return nil
}

// lazy replaces a blob reference with a lazy value reading the blob's
// content from the locations this transaction sees.
func (tx *Tx) lazy(v value.Value) (value.Value, error) {
	r, ok := v.(value.BlobRef)
	if !ok {
		return v, nil
	}
	locs, rec, err := tx.blobLocs(r.Hash)
	if err != nil {
		return nil, err
	}
	return value.Lazy{Ref: r, Src: tx.db.newReader(rec, locs)}, nil
}

// blobLocs returns a blob's record and the locations of its chunks.
func (tx *Tx) blobLocs(h [32]byte) ([]blob.Loc, keys.BlobRecord, error) {
	b, ok, err := tx.get(keys.Blob(h))
	if err != nil {
		return nil, keys.BlobRecord{}, err
	}
	if !ok {
		return nil, keys.BlobRecord{}, errorf(CodeInternal, "no blob %x", h[:8])
	}
	rec, err := keys.DecodeBlob(b)
	if err != nil {
		return nil, rec, wrap(CodeInternal, err)
	}
	locs := make([]blob.Loc, len(rec.Chunks))
	for i, ch := range rec.Chunks {
		b, ok, err := tx.get(keys.Chunk(ch))
		if err != nil {
			return nil, rec, err
		}
		if !ok {
			return nil, rec, errorf(CodeInternal, "blob %x: no chunk %x", h[:8], ch[:8])
		}
		l, err := keys.DecodeChunk(b)
		if err != nil {
			return nil, rec, wrap(CodeInternal, err)
		}
		locs[i] = blob.Loc(l)
	}
	return locs, rec, nil
}

// markBlobsDirty marks an entity for the collector if it has any blob
// use records.
func (tx *Tx) markBlobsDirty(id value.ID) error {
	has := false
	stop := errorf(CodeInternal, "stop")
	err := tx.scan(keys.BlobEnts(id), false, func(_, _ []byte) error {
		has = true
		return stop
	})
	if err != nil && err != stop {
		return err
	}
	if has {
		tx.t.Set(keys.BlobDirty(id), nil)
	}
	return nil
}

// droppedBlobs marks st's entity for the collector if saving st may drop
// a blob reference: a field that held one changes or goes, or the entity
// is deleted (its versions may go with it, now or later).
func (tx *Tx) droppedBlobs(st *entState) error {
	if st.isNew {
		return nil
	}
	if st.deleted {
		return tx.markBlobsDirty(st.e.ID)
	}
	for name := range st.blobs {
		if f, ok := st.fields[name]; !ok || f != st.orig[name] {
			tx.t.Set(keys.BlobDirty(st.e.ID), nil)
			return nil
		}
	}
	return nil
}

// --- reading blobs outside a transaction ---

// BlobReader reads a large value's content from the blob store, also
// after the transaction that found it has ended: the content of a
// value.Lazy, and of download leases. It keeps the segments it reads
// from until Close, or until it is garbage.
type BlobReader struct {
	Len   int64
	db    *DB
	rec   keys.BlobRecord
	locs  []blob.Loc
	unpin func()

	// The chunk read last, so that reads smaller than a chunk, one after
	// another, read and check each chunk once.
	mu       sync.Mutex
	lastNum  int
	lastData []byte
}

// newReader returns a reader of the chunks at locs. The caller must hold
// a snapshot that can read them, or another reader of them.
func (db *DB) newReader(rec keys.BlobRecord, locs []blob.Loc) *BlobReader {
	r := &BlobReader{Len: rec.Len, db: db, rec: rec, locs: locs, unpin: db.blobs.Pin(locs)}
	runtime.AddCleanup(r, func(unpin func()) { unpin() }, r.unpin)
	return r
}

// OpenBlob returns a reader of the content with SHA-256 h if the blob
// store holds it, so a lease can keep the reader instead of the content.
func (db *DB) OpenBlob(h [32]byte) (*BlobReader, bool, error) {
	var r *BlobReader
	err := db.View(func(tx *Tx) error {
		if _, ok, err := tx.get(keys.Blob(h)); err != nil || !ok {
			return err
		}
		locs, rec, err := tx.blobLocs(h)
		if err != nil {
			return err
		}
		r = db.newReader(rec, locs)
		return nil
	})
	return r, r != nil, err
}

// ReaderOf returns the blob store reader behind a lazy value, if it has
// one, holding the segments for the caller until its Close.
func ReaderOf(l value.Lazy) (*BlobReader, bool) {
	r, ok := l.Src.(*BlobReader)
	if !ok {
		return nil, false
	}
	return r.Retain(), true
}

// Retain returns another reader of the same content, holding the
// segments until its own Close.
func (r *BlobReader) Retain() *BlobReader { return r.db.newReader(r.rec, r.locs) }

// Read reads n bytes from offset off.
func (r *BlobReader) Read(off, n int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("blob: negative length %d", n)
	}
	out := make([]byte, n)
	return out, r.readInto(off, out)
}

// readInto fills p from offset off, through the last chunk read.
func (r *BlobReader) readInto(off int64, p []byte) error {
	n := int64(len(p))
	if off < 0 || off+n > r.Len {
		return fmt.Errorf("blob range %d+%d outside %d bytes", off, n, r.Len)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	done := int64(0)
	for i := int(off / blob.ChunkSize); done < n; i++ {
		if r.lastData == nil || r.lastNum != i {
			data, err := r.db.blobs.Get(r.rec.Chunks[i], r.locs[i])
			if err != nil {
				return err
			}
			r.lastNum, r.lastData = i, data
		}
		start := max(off+done-int64(i)*blob.ChunkSize, 0)
		done += int64(copy(p[done:], r.lastData[start:]))
	}
	return nil
}

// ReadAt implements io.ReaderAt.
func (r *BlobReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("blob: negative offset %d", off)
	}
	if off >= r.Len {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.Len-off)
	if err := r.readInto(off, p[:n]); err != nil {
		return 0, err
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

// Close lets the collector remove the segments the reader used.
func (r *BlobReader) Close() { r.unpin() }

// --- uploads ---

// Upload is content streamed into the blob store ahead of the statement
// that stores it (spec/protocol.md §7).
type Upload struct {
	Hash    [32]byte
	Len     int64
	of      value.Code
	invalid error // why the content isn't a valid value of its type
	chunks  []blob.Loc
	hashes  [][32]byte
	r       *BlobReader
}

// StoreUpload writes r's content to the blob store as chunks no record
// references yet. A write of the same content within ttl uses them as
// they are; after that they are garbage. It reads at most limit bytes.
// Text is normalized to NFC as it arrives and checked to be UTF-8, and
// an image's header is checked: an invalid upload is stored, and fails
// when a statement uses it (UploadValue).
func (db *DB) StoreUpload(r io.Reader, of value.Code, limit int64, ttl time.Duration) (*Upload, error) {
	u := &Upload{of: of}
	var text *value.TextChecker
	if of == value.CodeText {
		text = &value.TextChecker{}
		r = norm.NFC.Reader(r)
	}
	whole := sha256.New()
	buf := make([]byte, blob.ChunkSize)
	var last blob.Loc
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if u.Len += int64(n); u.Len > limit {
				return nil, errorf(CodeConstraint, "upload longer than %d bytes", limit)
			}
			data := buf[:n]
			whole.Write(data)
			if text != nil {
				text.Write(data)
			}
			ch := sha256.Sum256(data)
			loc, perr := db.putChunk(ch, data)
			if perr != nil {
				return nil, perr
			}
			db.blobs.AddPending(ch, loc, ttl)
			u.chunks, u.hashes, last = append(u.chunks, loc), append(u.hashes, ch), loc
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(u.chunks) > 0 {
		if err := db.blobs.Sync(last); err != nil {
			return nil, wrap(CodeInternal, err)
		}
	}
	copy(u.Hash[:], whole.Sum(nil))
	u.r = db.newReader(keys.BlobRecord{Len: u.Len, Chunks: u.hashes}, u.chunks)
	switch of {
	case value.CodeText:
		u.invalid = text.Err()
	case value.CodeImage:
		_, u.invalid = value.ImageFormat(io.NewSectionReader(u.r, 0, u.Len))
	}
	return u, nil
}

// ReadUpload reads n bytes from offset off of an upload's content.
func (db *DB) ReadUpload(u *Upload, off, n int64) ([]byte, error) {
	out, err := u.r.Read(off, n)
	return out, wrap(CodeInternal, err)
}

// UploadValue returns an upload as a lazy value to pass to a statement,
// or why its content isn't a valid value of its type.
func (db *DB) UploadValue(u *Upload, mediaType string) (value.Value, error) {
	if u.invalid != nil {
		return nil, u.invalid
	}
	ref := value.BlobRef{Of: u.of, Hash: u.Hash, Len: u.Len}
	if u.of == value.CodeBytes {
		ref.Media = mediaType
	}
	return value.Lazy{Ref: ref, Src: u.r.Retain()}, nil
}

// --- the collector ---

// BlobResult counts what a collection removed.
type BlobResult struct {
	Uses     int // use records nothing backed
	Blobs    int // blobs left without uses
	Chunks   int // chunks no blob listed
	Moved    int // live chunks moved out of rewritten segments
	Segments int // segment files deleted
}

// CollectBlobs removes what no stored value references any more (see
// the top of this file) and reports what it removed.
func (db *DB) CollectBlobs(ctx context.Context) (BlobResult, error) {
	var res BlobResult
	if err := db.collectDirty(ctx, &res); err != nil {
		return res, err
	}
	if err := db.collectUnused(ctx, &res); err != nil {
		return res, err
	}
	if err := db.collectChunks(ctx, &res); err != nil {
		return res, err
	}
	if err := db.rewriteSegments(ctx, &res); err != nil {
		return res, err
	}
	n, err := db.blobs.Remove(db.mgr.Oldest())
	res.Segments += n
	return res, wrap(CodeInternal, err)
}

// batches runs fn over the keys under prefix in transactions of up to
// sweepBatch keys, halving the batch on conflicts like the history sweep.
// fn sees each key once; keys a batch leaves in place are not revisited.
func (db *DB) batches(ctx context.Context, prefix []byte, fn func(tx *Tx, k []byte) error) error {
	var after []byte
	batch := sweepBatch
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var next []byte
		err := db.Update(func(tx *Tx) error {
			opts := keys.Range(prefix)
			if after != nil {
				opts.Lower = storage.PrefixEnd(after)
			}
			var ks [][]byte
			it := tx.t.Iter(opts)
			for len(ks) < batch && it.Next() {
				ks = append(ks, append([]byte(nil), it.Key()...))
			}
			err := it.Err()
			it.Close()
			if err != nil {
				return wrap(CodeInternal, err)
			}
			for _, k := range ks {
				if err := fn(tx, k); err != nil {
					return err
				}
			}
			if len(ks) > 0 {
				next = ks[len(ks)-1]
			}
			return nil
		})
		switch {
		case IsConflict(err) && batch > minSweepBatch:
			batch = max(batch/2, minSweepBatch)
			continue
		case IsConflict(err):
			// Busy: left for the next collection.
		case err != nil:
			return err
		default:
			batch = min(batch*2, sweepBatch)
		}
		if next == nil {
			return nil
		}
		after = next
	}
}

// collectDirty checks each dirty entity's use records against the
// references it still holds.
func (db *DB) collectDirty(ctx context.Context, res *BlobResult) error {
	return db.batches(ctx, keys.Prefix(keys.PrefixBlobDirty), func(tx *Tx, k []byte) error {
		id, err := keys.KeyEntity(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		// Any save of the entity writes its header, and a delete its
		// life record or header: a writer changing it conflicts.
		tx.t.GuardKey(keys.Entity(id))
		tx.t.GuardKey(keys.Life(id))
		live, err := tx.blobRefs(id)
		if err != nil {
			return err
		}
		err = tx.scan(keys.BlobEnts(id), false, func(k, _ []byte) error {
			h, err := keys.KeyHash(k)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if live[h] {
				delete(live, h)
				return nil
			}
			tx.t.Delete(append([]byte(nil), k...))
			tx.t.Delete(keys.BlobUse(h, id))
			tx.t.Set(keys.BlobCheck(h), nil)
			res.Uses++
			return nil
		})
		if err != nil {
			return err
		}
		for h := range live { // missing use records: restore them
			tx.t.Set(keys.BlobUse(h, id), nil)
			tx.t.Set(keys.BlobEnt(id, h), nil)
		}
		tx.t.Delete(k)
		return nil
	})
}

// blobRefs returns the blobs an entity's fields and versions reference.
func (tx *Tx) blobRefs(id value.ID) (map[[32]byte]bool, error) {
	out := map[[32]byte]bool{}
	cat := tx.Catalog()
	err := tx.scan(keys.Fields(id), false, func(k, v []byte) error {
		_, name, err := keys.ParseField(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		f, err := keys.DecodeField(id, name, v, cat)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if r, ok := f.Value.(value.BlobRef); ok {
			out[r.Hash] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = tx.scan(keys.HistoryEntity(id), false, func(_, v []byte) error {
		val, err := keys.DecodeVersion(v)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if r, ok := val.(value.BlobRef); ok {
			out[r.Hash] = true
		}
		return nil
	})
	return out, err
}

// collectUnused deletes the blobs marked for checking that have no uses.
func (db *DB) collectUnused(ctx context.Context, res *BlobResult) error {
	return db.batches(ctx, keys.Prefix(keys.PrefixBlobCheck), func(tx *Tx, k []byte) error {
		h, err := keys.KeyHash(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		used := false
		stop := errorf(CodeInternal, "stop")
		// Guarded: a writer adding a use conflicts with this deletion.
		err = tx.scan(keys.BlobUses(h), true, func(_, _ []byte) error {
			used = true
			return stop
		})
		if err != nil && err != stop {
			return err
		}
		if !used {
			if _, ok, err := tx.get(keys.Blob(h)); err != nil {
				return err
			} else if ok {
				tx.t.Delete(keys.Blob(h))
				res.Blobs++
			}
		}
		tx.t.Delete(k)
		return nil
	})
}

// collectChunks deletes the location records of chunks no blob lists,
// in one transaction so that the blobs it reads and the chunks it
// deletes are from one snapshot. A writer that reuses one of the chunks
// meanwhile writes its record too, so one of the two fails.
func (db *DB) collectChunks(ctx context.Context, res *BlobResult) error {
	return db.Update(func(tx *Tx) error {
		live, err := tx.liveChunks(ctx)
		if err != nil {
			return err
		}
		n := 0
		err = tx.scan(keys.Prefix(keys.PrefixChunk), false, func(k, _ []byte) error {
			h, err := keys.KeyHash(k)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if !live[h] {
				tx.t.Delete(append([]byte(nil), k...))
				n++
			}
			return nil
		})
		res.Chunks += n
		return err
	})
}

// liveChunks returns the chunks some blob lists.
func (tx *Tx) liveChunks(ctx context.Context) (map[[32]byte]bool, error) {
	live := map[[32]byte]bool{}
	err := tx.scan(keys.Prefix(keys.PrefixBlob), false, func(_, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := keys.DecodeBlob(v)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		for _, ch := range rec.Chunks {
			live[ch] = true
		}
		return nil
	})
	return live, err
}

// rewriteSegments moves the live chunks out of sealed segments that are
// mostly garbage, and retires those segments.
func (db *DB) rewriteSegments(ctx context.Context, res *BlobResult) error {
	sealed := db.blobs.Sealed()
	if len(sealed) == 0 {
		return nil
	}
	size := map[uint32]int64{}
	for _, s := range sealed {
		size[s.Num] = s.Size
	}
	// How much of each sealed segment is live.
	live := map[uint32]int64{}
	err := db.View(func(tx *Tx) error {
		return tx.scan(keys.Prefix(keys.PrefixChunk), false, func(_, v []byte) error {
			l, err := keys.DecodeChunk(v)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if _, ok := size[l.Seg]; ok {
				live[l.Seg] += int64(l.Len)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	oldest := db.mgr.Oldest()
	for _, s := range sealed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if float64(live[s.Num]) >= rewriteLive*float64(s.Size) || !db.settled(s.Num, oldest) {
			continue
		}
		var moved int
		var seq uint64
		err := db.Update(func(tx *Tx) error {
			moved = 0
			if err := tx.moveChunks(s.Num, &moved); err != nil {
				return err
			}
			tx.t.Set(blobSegKey(s.Num), nil)
			tx.commitSeq = &seq
			return nil
		})
		if IsConflict(err) {
			continue // a writer used it meanwhile; next time
		}
		if err != nil {
			return err
		}
		res.Moved += moved
		db.blobs.Retire(s.Num, seq)
	}
	return nil
}

// moveChunks copies the chunks located in segment seg to the active
// segment and points their records there.
func (tx *Tx) moveChunks(seg uint32, moved *int) error {
	type entry struct {
		k   []byte
		loc keys.ChunkLoc
	}
	var es []entry
	err := tx.scan(keys.Prefix(keys.PrefixChunk), false, func(k, v []byte) error {
		l, err := keys.DecodeChunk(v)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if l.Seg == seg {
			es = append(es, entry{append([]byte(nil), k...), l})
		}
		return nil
	})
	if err != nil {
		return err
	}
	store := tx.db.blobs
	for _, e := range es {
		h, err := keys.KeyHash(e.k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		data, err := store.Get(h, blob.Loc(e.loc))
		if err != nil {
			return wrap(CodeInternal, err)
		}
		loc, err := tx.db.putChunk(h, data)
		if err != nil {
			return tx.fail(err)
		}
		tx.blobSync = append(tx.blobSync, loc)
		tx.t.Set(e.k, keys.EncodeChunk(keys.ChunkLoc(loc)))
		*moved++
	}
	return nil
}

// BlobStats describes a database's blob store.
type BlobStats struct {
	Blobs, Chunks int   // records
	Bytes         int64 // content bytes of the blobs
	FileBytes     int64 // bytes in segment files
	Segments      int
}

// BlobStats counts the blob store's records and files.
func (tx *Tx) BlobStats() (BlobStats, error) {
	var st BlobStats
	err := tx.scan(keys.Prefix(keys.PrefixBlob), false, func(_, v []byte) error {
		rec, err := keys.DecodeBlob(v)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		st.Blobs++
		st.Bytes += rec.Len
		return nil
	})
	if err != nil {
		return st, err
	}
	if st.Chunks, err = tx.count(keys.Prefix(keys.PrefixChunk)); err != nil {
		return st, err
	}
	st.FileBytes, st.Segments = tx.db.blobs.Size()
	return st, nil
}
