package sstable

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/sandgorgon/polydb/storage/vfs"
)

type entry struct {
	key   string
	seq   uint64
	kind  uint8
	value string
}

func cmpEntry(a, b entry) int { return Compare([]byte(a.key), a.seq, []byte(b.key), b.seq) }

// randomEntries returns entries in internal key order: keys sharing
// prefixes (as PolyDB's do), several versions each.
func randomEntries(rng *rand.Rand, n int) []entry {
	seen := map[string]bool{}
	var es []entry
	seq := uint64(1)
	for len(es) < n {
		key := fmt.Sprintf("table/%03d/field/%d", rng.IntN(300), rng.IntN(5))
		if seen[key] {
			continue
		}
		seen[key] = true
		for range 1 + rng.IntN(3) {
			e := entry{key: key, seq: seq, kind: KindSet, value: fmt.Sprintf("v%d-%s", seq, bytes.Repeat([]byte("x"), rng.IntN(40)))}
			if rng.IntN(5) == 0 {
				e.kind, e.value = KindDelete, ""
			}
			seq += uint64(1 + rng.IntN(3))
			es = append(es, e)
		}
	}
	slices.SortFunc(es, cmpEntry)
	return es
}

func write(t testing.TB, fs vfs.FS, name string, es []entry, ts []Tombstone, o WriterOptions) Props {
	t.Helper()
	f, err := fs.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriter(f, o)
	for _, e := range es {
		if err := w.Add([]byte(e.key), e.seq, e.kind, []byte(e.value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tb := range ts {
		if err := w.AddTombstone(tb); err != nil {
			t.Fatal(err)
		}
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func open(t testing.TB, fs vfs.FS, name string, o ReaderOptions) *Reader {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(f, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cur(it *Iter) entry {
	return entry{string(it.Key()), it.Seq(), it.Kind(), string(it.Value())}
}

// checkTable compares a table with the model: full scans both ways, gets
// at random snapshots, and random walks of seeks, Next and Prev.
func checkTable(t *testing.T, r *Reader, es []entry, rng *rand.Rand) {
	t.Helper()
	it := r.NewIter()
	defer it.Close()
	var got []entry
	for it.First(); it.Valid(); it.Next() {
		got = append(got, cur(it))
	}
	if it.Err() != nil || !slices.Equal(got, es) {
		t.Fatalf("forward scan: %d entries, want %d (%v)", len(got), len(es), it.Err())
	}
	got = got[:0]
	for it.Last(); it.Valid(); it.Prev() {
		got = append(got, cur(it))
	}
	slices.Reverse(got)
	if it.Err() != nil || !slices.Equal(got, es) {
		t.Fatalf("reverse scan: %d entries (%v)", len(got), it.Err())
	}

	maxSeq := es[len(es)-1].seq
	for _, e := range es {
		maxSeq = max(maxSeq, e.seq)
	}
	for range 2000 {
		key := es[rng.IntN(len(es))].key
		if rng.IntN(4) == 0 {
			key += "~" // absent
		}
		snap := rng.Uint64N(maxSeq + 2)
		var want *entry
		for i := range es {
			if es[i].key == key && es[i].seq <= snap {
				want = &es[i]
				break
			}
		}
		v, seq, kind, ok, err := r.Get([]byte(key), snap)
		if err != nil {
			t.Fatal(err)
		}
		if (want == nil) != !ok || (ok && (want.seq != seq || want.kind != kind || want.value != string(v))) {
			t.Fatalf("Get(%q, %d) = %q %d %d %v, want %v", key, snap, v, seq, kind, ok, want)
		}
	}

	for range 300 {
		probe := es[rng.IntN(len(es))].key
		if rng.IntN(3) == 0 {
			probe = probe[:rng.IntN(len(probe)+1)]
		}
		var pos int
		if rng.IntN(2) == 0 {
			it.SeekGE([]byte(probe))
			pos, _ = slices.BinarySearchFunc(es, probe, func(e entry, k string) int {
				if e.key < k {
					return -1
				}
				return 1
			})
		} else {
			it.SeekLT([]byte(probe))
			p, _ := slices.BinarySearchFunc(es, probe, func(e entry, k string) int {
				if e.key < k {
					return -1
				}
				return 1
			})
			pos = p - 1
		}
		for step := range 20 {
			if pos < 0 || pos >= len(es) {
				if it.Valid() {
					t.Fatalf("walk from %q, step %d: at %v, want the end", probe, step, cur(it))
				}
				break
			}
			if !it.Valid() || cur(it) != es[pos] {
				t.Fatalf("walk from %q, step %d: valid %v, want %v (%v)", probe, step, it.Valid(), es[pos], it.Err())
			}
			if rng.IntN(2) == 0 {
				it.Next()
				pos++
			} else {
				it.Prev()
				pos--
			}
		}
	}
}

func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	tombs := []Tombstone{{Start: []byte("b"), End: []byte("c"), Seq: 5}, {Start: []byte("a"), Seq: 9}}
	for _, o := range []WriterOptions{{}, {BlockSize: 64, RestartInterval: 1}, {BlockSize: 300, RestartInterval: 3}} {
		es := randomEntries(rng, 500)
		mem := vfs.NewMem()
		mem.MkdirAll("/t")
		p := write(t, mem, "/t/1.sst", es, tombs, o)
		if p.Entries != uint64(len(es)) || p.Tombstones != 2 || p.Smallest == nil ||
			string(p.Smallest) != es[0].key || string(p.Largest) != es[len(es)-1].key || p.DataBlocks < 2 {
			t.Errorf("props %+v", p)
		}
		dir := t.TempDir()
		write(t, vfs.OS, dir+"/1.sst", es, tombs, o)
		readers := map[string]*Reader{
			"memory, cached": open(t, mem, "/t/1.sst", ReaderOptions{ID: 1, Cache: NewCache(1 << 20)}),
			"memory":         open(t, mem, "/t/1.sst", ReaderOptions{}),
			"OS, mapped":     open(t, vfs.OS, dir+"/1.sst", ReaderOptions{}),
			"OS, cached":     open(t, vfs.OS, dir+"/1.sst", ReaderOptions{ID: 2, NoMmap: true, Cache: NewCache(4096)}),
		}
		if readers["OS, mapped"].data == nil {
			t.Error("the OS reader didn't map the file")
		}
		for name, r := range readers {
			t.Run(fmt.Sprintf("%s/%d", name, o.BlockSize), func(t *testing.T) {
				checkTable(t, r, es, rng)
				if got := r.Tombstones(); len(got) != 2 || string(got[0].Start) != "a" || got[0].End != nil || string(got[1].End) != "c" {
					t.Errorf("tombstones %v", got)
				}
				if fmt.Sprint(r.Props()) != fmt.Sprint(p) {
					t.Errorf("props differ")
				}
			})
			r.Close()
		}
	}
}

func TestEmptyAndTombstoneOnly(t *testing.T) {
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	p := write(t, mem, "/t/e.sst", nil, []Tombstone{{Start: []byte("k"), End: []byte("m"), Seq: 3}}, WriterOptions{})
	if p.Entries != 0 || p.MinSeq != 3 || p.MaxSeq != 3 || p.Smallest != nil {
		t.Errorf("props %+v", p)
	}
	r := open(t, mem, "/t/e.sst", ReaderOptions{})
	defer r.Close()
	it := r.NewIter()
	defer it.Close()
	for _, f := range []func(){it.First, it.Last, func() { it.SeekGE([]byte("k")) }, func() { it.SeekLT([]byte("z")) }} {
		if f(); it.Valid() || it.Err() != nil {
			t.Error("an empty table has an entry")
		}
	}
	if _, _, _, ok, err := r.Get([]byte("k"), 10); ok || err != nil {
		t.Error("Get on an empty table")
	}
}

func TestWriterChecks(t *testing.T) {
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	f, _ := mem.Create("/t/x.sst")
	w := NewWriter(f, WriterOptions{})
	if err := w.Add([]byte("b"), 5, KindSet, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []entry{{"a", 9, KindSet, ""}, {"b", 5, KindSet, ""}, {"b", 6, KindSet, ""}, {"c", 1, 7, ""}, {"c", MaxSeq + 1, KindSet, ""}, {"c", 1, KindDelete, "v"}} {
		if err := w.Add([]byte(bad.key), bad.seq, bad.kind, []byte(bad.value)); err == nil {
			t.Errorf("Add(%v) succeeded", bad)
		}
	}
	if err := w.Add([]byte("b"), 4, KindDelete, nil); err != nil {
		t.Error("an older version of the same key was refused")
	}
	if err := w.AddTombstone(Tombstone{Start: []byte("z"), End: []byte("a")}); err == nil {
		t.Error("empty tombstone accepted")
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Finish(); err == nil {
		t.Error("second Finish")
	}
}

func TestBloomFilter(t *testing.T) {
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	var es []entry
	for i := range 10000 {
		es = append(es, entry{key: fmt.Sprintf("key%06d", i), seq: 1, kind: KindSet, value: "v"})
	}
	write(t, mem, "/t/b.sst", es, nil, WriterOptions{})
	r := open(t, mem, "/t/b.sst", ReaderOptions{})
	defer r.Close()
	for _, e := range es {
		if !r.MayContain([]byte(e.key)) {
			t.Fatalf("false negative for %s", e.key)
		}
	}
	fp := 0
	for i := range 10000 {
		if r.MayContain(fmt.Appendf(nil, "absent%06d", i)) {
			fp++
		}
	}
	t.Logf("false positives: %d of 10000", fp)
	if fp > 300 {
		t.Errorf("false-positive rate %.1f%%", float64(fp)/100)
	}
}

// TestCorruption flips every byte of a table in turn: each flip must be
// reported, never returned as data.
func TestCorruption(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 2))
	es := randomEntries(rng, 40)
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	write(t, mem, "/t/c.sst", es, []Tombstone{{Start: []byte("q"), Seq: 1}}, WriterOptions{BlockSize: 256, RestartInterval: 4})
	f, _ := mem.Open("/t/c.sst")
	size, _ := f.Size()
	orig := make([]byte, size)
	f.ReadAt(orig, 0)
	f.Close()
	for i := range orig {
		bad := bytes.Clone(orig)
		bad[i] ^= 1 << rng.IntN(8)
		wf, _ := mem.Create("/t/bad.sst")
		wf.Write(bad)
		wf.Close()
		rf, _ := mem.Open("/t/bad.sst")
		r, err := Open(rf, ReaderOptions{})
		if err != nil {
			continue
		}
		it := r.NewIter()
		n := 0
		for it.First(); it.Valid(); it.Next() {
			n++
		}
		err = it.Err()
		it.Close()
		r.Close()
		if err == nil {
			t.Fatalf("flipping byte %d of %d went unnoticed (%d entries read)", i, len(orig), n)
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("byte %d: %v is not ErrCorrupt", i, err)
		}
	}
}

func TestCache(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	es := randomEntries(rng, 400)
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	p := write(t, mem, "/t/1.sst", es, nil, WriterOptions{BlockSize: 512})
	c := NewCache(1 << 20)
	r := open(t, mem, "/t/1.sst", ReaderOptions{ID: 7, Cache: c})
	defer r.Close()
	scan := func() {
		it := r.NewIter()
		for it.First(); it.Valid(); it.Next() {
		}
		it.Close()
	}
	scan()
	_, misses, used := c.Stats()
	if misses != int64(p.DataBlocks) || used == 0 {
		t.Errorf("first scan: %d misses for %d blocks, %d bytes", misses, p.DataBlocks, used)
	}
	scan()
	hits, misses2, _ := c.Stats()
	if misses2 != misses || hits < int64(p.DataBlocks) {
		t.Errorf("second scan: %d hits, %d misses", hits, misses2)
	}
	c.EvictTable(7)
	if _, _, used := c.Stats(); used != 0 {
		t.Errorf("%d bytes after eviction", used)
	}
	small := NewCache(2048)
	r2 := open(t, mem, "/t/1.sst", ReaderOptions{ID: 8, Cache: small})
	defer r2.Close()
	checkTable(t, r2, es, rng)
	// Each of the 16 shards may keep one block over its share.
	if _, _, used := small.Stats(); used > 2048+16*600 {
		t.Errorf("small cache holds %d bytes", used)
	}
}

// TestReaderLifetime checks that the file stays open while an iterator
// is open, even after the reader is closed.
func TestReaderLifetime(t *testing.T) {
	dir := t.TempDir()
	es := randomEntries(rand.New(rand.NewPCG(4, 4)), 50)
	write(t, vfs.OS, dir+"/1.sst", es, nil, WriterOptions{})
	r := open(t, vfs.OS, dir+"/1.sst", ReaderOptions{})
	it := r.NewIter()
	r.Close()
	n := 0
	for it.First(); it.Valid(); it.Next() {
		n++
	}
	if n != len(es) {
		t.Errorf("%d entries after the reader was closed", n)
	}
	it.Close()
	if r.data != nil {
		t.Error("the mapping outlived the reader")
	}
	if it.First(); it.Valid() {
		t.Error("closed iterator is valid")
	}
}

// FuzzBlock checks that block parsing survives arbitrary bytes.
func FuzzBlock(f *testing.F) {
	var w blockWriter
	w.interval = 2
	for i := range 5 {
		w.add(appendInternal(nil, fmt.Appendf(nil, "key%d", i), 9, KindSet), []byte("value"))
	}
	f.Add(slices.Clone(w.finish()), []byte("key2"))
	f.Fuzz(func(t *testing.T, b []byte, key []byte) {
		var it blockIter
		if it.init(b) != nil {
			return
		}
		for it.first(); it.valid; it.nextEntry() {
		}
		for it.last(); it.valid; it.prev() {
		}
		it.seekGE(key, seekSeq)
		it.seekLT(key, 5)
	})
}

// FuzzOpen checks that opening and reading arbitrary files never panics.
func FuzzOpen(f *testing.F) {
	mem := vfs.NewMem()
	mem.MkdirAll("/t")
	write(f, mem, "/t/s.sst", randomEntries(rand.New(rand.NewPCG(5, 5)), 10), []Tombstone{{Start: []byte("a"), Seq: 2}}, WriterOptions{BlockSize: 64})
	sf, _ := mem.Open("/t/s.sst")
	size, _ := sf.Size()
	seed := make([]byte, size)
	sf.ReadAt(seed, 0)
	f.Add(seed)
	f.Fuzz(func(t *testing.T, b []byte) {
		m := vfs.NewMem()
		m.MkdirAll("/t")
		w, _ := m.Create("/t/f.sst")
		w.Write(b)
		w.Close()
		rf, _ := m.Open("/t/f.sst")
		r, err := Open(rf, ReaderOptions{Cache: NewCache(1 << 16)})
		if err != nil {
			return
		}
		it := r.NewIter()
		for it.First(); it.Valid(); it.Next() {
		}
		for it.Last(); it.Valid(); it.Prev() {
		}
		it.Close()
		r.Get([]byte("table"), 100)
		r.Close()
	})
}
