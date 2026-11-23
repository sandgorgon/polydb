package lsm

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/storagetest"
	"github.com/sandgorgon/polydb/storage/vfs"
)

// tiny makes every memtable flush and compaction happen at test sizes.
func tiny(fs vfs.FS) Options {
	return Options{FS: fs, Dir: "/db", MemtableSize: 2 << 10, TableSize: 2 << 10, LevelBaseSize: 8 << 10,
		L0CompactionTrigger: 2, L0StopWrites: 6, BlockSize: 256, LogSegmentSize: 4 << 10}
}

func openOpts(t *testing.T, o Options) *Store {
	t.Helper()
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformanceTiny(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store { return openOpts(t, tiny(vfs.NewMem())) })
}

func TestConformanceOS(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		o := tiny(vfs.OS)
		o.Dir = t.TempDir()
		return openOpts(t, o)
	})
}

type model map[string]string

func (m model) apply(b *storage.Batch) {
	for _, op := range b.Ops() {
		switch op.Kind {
		case storage.OpSet:
			m[string(op.Key)] = string(op.Value)
		case storage.OpDelete:
			delete(m, string(op.Key))
		case storage.OpDeleteRange:
			for k := range m {
				if storage.InRange([]byte(k), op.Key, op.End) {
					delete(m, k)
				}
			}
		}
	}
}

func (m model) scan(lo, hi string, reverse bool) string {
	var keys []string
	for k := range m {
		if k >= lo && (hi == "" || k < hi) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if reverse {
		slices.Reverse(keys)
	}
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, " ")
}

func scanRange(t *testing.T, r storage.Reader, lo, hi string, reverse bool) string {
	t.Helper()
	o := storage.IterOptions{Reverse: reverse}
	if lo != "" {
		o.Lower = []byte(lo)
	}
	if hi != "" {
		o.Upper = []byte(hi)
	}
	it := r.Iter(o)
	defer it.Close()
	var out []string
	for it.Next() {
		out = append(out, string(it.Key())+"="+string(it.Value()))
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " ")
}

func check(t *testing.T, r storage.Reader, m model, rng *rand.Rand, what string) {
	t.Helper()
	if got, want := scanRange(t, r, "", "", false), m.scan("", "", false); got != want {
		t.Fatalf("%s: scan\n got %.300s\nwant %.300s", what, got, want)
	}
	if got, want := scanRange(t, r, "", "", true), m.scan("", "", true); got != want {
		t.Fatalf("%s: reverse scan differs", what)
	}
	for range 20 {
		lo, hi := fmt.Sprintf("k%03d", rng.IntN(200)), fmt.Sprintf("k%03d", rng.IntN(200))
		if lo > hi {
			lo, hi = hi, lo
		}
		rev := rng.IntN(2) == 0
		if got, want := scanRange(t, r, lo, hi, rev), m.scan(lo, hi, rev); got != want {
			t.Fatalf("%s: scan [%s, %s) reverse %v\n got %s\nwant %s", what, lo, hi, rev, got, want)
		}
	}
	for i := range 200 {
		k := fmt.Sprintf("k%03d", i)
		want, ok := m[k]
		if !ok {
			want = "<none>"
		}
		if got := get(t, r, k); got != want {
			t.Fatalf("%s: Get(%s) = %s, want %s", what, k, got, want)
		}
	}
}

// TestModel runs random batches (sets, deletes, range deletions) through
// flushes, compactions and reopens, holding snapshots, and compares every
// view with a model.
func TestModel(t *testing.T) {
	for _, fsName := range []string{"mem", "os"} {
		t.Run(fsName, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(7, 7))
			var fs vfs.FS = vfs.NewMem()
			o := tiny(fs)
			if fsName == "os" {
				o = tiny(vfs.OS)
				o.Dir = t.TempDir()
			}
			s := openOpts(t, o)
			defer func() { s.Close() }()
			m := model{}
			type held struct {
				snap storage.Snapshot
				m    model
			}
			var snaps []held
			for round := range 3000 {
				var b storage.Batch
				for range 1 + rng.IntN(5) {
					k := fmt.Sprintf("k%03d", rng.IntN(200))
					switch x := rng.IntN(100); {
					case x < 75:
						b.Set([]byte(k), fmt.Appendf(nil, "%d-%s", round, strings.Repeat("v", rng.IntN(30))))
					case x < 97:
						b.Delete([]byte(k))
					default:
						end := fmt.Sprintf("k%03d", rng.IntN(200))
						if end <= k {
							b.DeleteRange([]byte(k), nil)
						} else {
							b.DeleteRange([]byte(k), []byte(end))
						}
					}
				}
				if _, err := s.Apply(&b); err != nil {
					t.Fatal(err)
				}
				m.apply(&b)
				switch rng.IntN(200) {
				case 0:
					if len(snaps) < 4 {
						snaps = append(snaps, held{s.Snapshot(), maps.Clone(m)})
					}
				case 1:
					if len(snaps) > 0 {
						i := rng.IntN(len(snaps))
						check(t, snaps[i].snap, snaps[i].m, rng, fmt.Sprintf("round %d, snapshot %d", round, snaps[i].snap.Seq()))
						snaps[i].snap.Release()
						snaps = slices.Delete(snaps, i, i+1)
					}
				case 2:
					if err := s.CompactAll(); err != nil {
						t.Fatal(err)
					}
				case 3:
					for _, h := range snaps {
						h.snap.Release()
					}
					snaps = nil
					s.Close()
					s = openOpts(t, o)
					check(t, s, m, rng, fmt.Sprintf("round %d, after reopen", round))
				}
				if round%250 == 0 {
					check(t, s, m, rng, fmt.Sprintf("round %d", round))
				}
			}
			for _, h := range snaps {
				check(t, h.snap, h.m, rng, "final snapshot")
				h.snap.Release()
			}
			check(t, s, m, rng, "final")
			met := s.Metrics()
			deep := 0
			for l := 1; l < numLevels; l++ {
				deep += met.Levels[l].Tables
			}
			if deep == 0 {
				t.Errorf("nothing was compacted below level 0: %+v", met)
			}
			t.Logf("%+v", met)
		})
	}
}

// TestGarbageCollection checks that compaction drops old versions and
// deletions, but keeps what a live snapshot can see.
func TestGarbageCollection(t *testing.T) {
	fs := vfs.NewMem()
	s := openOpts(t, tiny(fs))
	defer s.Close()
	for i := range 50 {
		set(t, s, "key", fmt.Sprint(i), "other", fmt.Sprint(i))
	}
	snap := s.Snapshot()
	set(t, s, "key", "after")
	var b storage.Batch
	b.Delete([]byte("other"))
	b.DeleteRange([]byte("a"), []byte("j"))
	s.Apply(&b)
	if err := s.CompactAll(); err != nil {
		t.Fatal(err)
	}
	versions := func() map[string]int {
		n := map[string]int{}
		rs, _ := s.acquire()
		defer rs.v.unref()
		for l, level := range rs.v.levels {
			for _, tb := range level {
				if l != numLevels-1 {
					t.Errorf("table at level %d after CompactAll", l)
				}
				it := tb.r.NewIter()
				for it.First(); it.Valid(); it.Next() {
					n[fmt.Sprintf("%s/%d", it.Key(), it.Kind())]++
				}
				it.Close()
				n["tombstones"] += len(tb.tombs)
			}
		}
		return n
	}
	// The snapshot sees key=49 and other=49; now sees key=after and no
	// other. So: two versions of key, other's set and deletion, and the
	// range tombstone (the snapshot predates it).
	if got := versions(); got["key/1"] != 2 || got["other/1"] != 1 || got["other/2"] != 1 || got["tombstones"] != 1 {
		t.Errorf("with a snapshot: %v", got)
	}
	if get(t, snap, "key") != "49" || get(t, snap, "other") != "49" || get(t, s, "key") != "after" || get(t, s, "other") != "<none>" {
		t.Error("reads after compaction")
	}
	snap.Release()
	if err := s.CompactAll(); err != nil {
		t.Fatal(err)
	}
	if got := versions(); got["key/1"] != 1 || got["other/1"] != 0 || got["other/2"] != 0 || got["tombstones"] != 0 {
		t.Errorf("without snapshots: %v", got)
	}
	// Obsolete tables are deleted.
	names, _ := fs.List("/db")
	ssts := 0
	for _, n := range names {
		if strings.HasSuffix(n, ".sst") {
			ssts++
		}
	}
	if met := s.Metrics(); ssts != met.Levels[numLevels-1].Tables {
		t.Errorf("%d table files for %d tables", ssts, met.Levels[numLevels-1].Tables)
	}
}

// TestIteratorOutlivesCompaction checks that an open iterator keeps
// reading the tables a compaction replaced.
func TestIteratorOutlivesCompaction(t *testing.T) {
	fs := vfs.NewMem()
	s := openOpts(t, tiny(fs))
	defer s.Close()
	for i := range 300 {
		set(t, s, fmt.Sprintf("k%03d", i), strings.Repeat("x", 20))
	}
	s.Flush()
	it := s.Iter(storage.IterOptions{})
	it.Next()
	for i := range 300 {
		var b storage.Batch
		b.Delete(fmt.Appendf(nil, "k%03d", i))
		s.Apply(&b)
	}
	if err := s.CompactAll(); err != nil {
		t.Fatal(err)
	}
	n := 1
	for it.Next() {
		n++
	}
	if it.Err() != nil || n != 300 {
		t.Errorf("iterator saw %d keys (%v)", n, it.Err())
	}
	it.Close()
	if got := scan(t, s, false); got != "" {
		t.Errorf("after deleting everything: %.100s", got)
	}
}

// TestCrashDuringBackgroundWork crashes while flushes and compactions run
// under concurrent writers.
func TestCrashDuringBackgroundWork(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	for iter := range 40 {
		fs := vfs.NewMem()
		s := openOpts(t, tiny(fs))
		var mu sync.Mutex
		acked := map[string]bool{}
		var wg sync.WaitGroup
		for w := range 4 {
			n := 20 + rng.IntN(60)
			wg.Go(func() {
				for i := range n {
					tag := fmt.Sprintf("w%d-%03d", w, i)
					var b storage.Batch
					for k := range 3 {
						b.Set(fmt.Appendf(nil, "%s/%d", tag, k), []byte(tag+strings.Repeat("-", 30)))
					}
					if _, err := s.Apply(&b); err != nil {
						return
					}
					mu.Lock()
					acked[tag] = true
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		crashed := fs.Crash(rng)
		s.Close()
		s2, err := Open(tiny(crashed))
		if err != nil {
			t.Fatalf("iteration %d: reopen: %v", iter, err)
		}
		counts := map[string]int{}
		it := s2.Iter(storage.IterOptions{})
		for it.Next() {
			counts[strings.TrimRight(string(it.Value()), "-")]++
		}
		if it.Err() != nil {
			t.Fatalf("iteration %d: %v", iter, it.Err())
		}
		it.Close()
		for tag := range acked {
			if counts[tag] != 3 {
				t.Fatalf("iteration %d: acknowledged batch %s has %d of 3 keys", iter, tag, counts[tag])
			}
		}
		for tag, n := range counts {
			if n != 3 {
				t.Fatalf("iteration %d: batch %s is partial", iter, tag)
			}
		}
		// The recovered store keeps working, and survives a clean reopen.
		set(t, s2, "after", "crash")
		s2.Close()
		s3 := openOpts(t, tiny(crashed))
		if get(t, s3, "after") != "crash" {
			t.Fatalf("iteration %d: write after recovery lost", iter)
		}
		s3.Close()
	}
}

func TestManifestEdit(t *testing.T) {
	e := &edit{snapshot: true, nextFile: 9, flushedSeq: 100, logNum: 4,
		deleted: []deletedTable{{1, 3}},
		added: []addedTable{{level: 2, num: 8, size: 4096, minSeq: 5, maxSeq: 90, b: bounds{lo: []byte("a"), hi: []byte("m"), hiExcl: true}},
			{level: 0, num: 7, b: bounds{lo: []byte("x"), hiInf: true}}}}
	rec := e.encode()
	d, err := decodeEdit(rec)
	if err != nil || fmt.Sprint(d) != fmt.Sprint(e) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, d, e)
	}
	for i := range rec {
		if _, err := decodeEdit(rec[:i]); err == nil {
			t.Errorf("truncated edit at %d decoded", i)
		}
	}
}

func TestBounds(t *testing.T) {
	b := func(lo, hi string, excl, inf bool) bounds {
		return bounds{lo: []byte(lo), hi: []byte(hi), hiExcl: excl, hiInf: inf}
	}
	for _, tc := range []struct {
		a, b bounds
		want bool
	}{
		{b("a", "c", false, false), b("c", "d", false, false), true},
		{b("a", "c", true, false), b("c", "d", false, false), false},
		{b("a", "", false, true), b("x", "y", false, false), true},
		{b("d", "e", false, false), b("a", "c", false, false), false},
	} {
		if overlaps(tc.a, tc.b) != tc.want || overlaps(tc.b, tc.a) != tc.want {
			t.Errorf("overlaps(%v, %v) != %v", tc.a, tc.b, tc.want)
		}
	}
	u := union(b("a", "c", true, false), b("b", "c", false, false))
	if string(u.lo) != "a" || string(u.hi) != "c" || u.hiExcl {
		t.Errorf("union %+v", u)
	}
}

// checkLevels checks the version's invariants: tables below level 0 are
// sorted and disjoint.
func checkLevels(t *testing.T, s *Store) {
	t.Helper()
	rs, _ := s.acquire()
	defer rs.v.unref()
	for l := 1; l < numLevels; l++ {
		files := rs.v.levels[l]
		for i := 1; i < len(files); i++ {
			if overlaps(files[i-1].b, files[i].b) {
				t.Fatalf("level %d: tables %d and %d overlap: %+v %+v", l, files[i-1].num, files[i].num, files[i-1].b, files[i].b)
			}
		}
	}
}

// TestGrowth writes enough distinct keys that background compaction
// alone fills several levels.
func TestGrowth(t *testing.T) {
	fs := vfs.NewMem()
	s := openOpts(t, tiny(fs))
	defer func() { s.Close() }()
	rng := rand.New(rand.NewPCG(11, 11))
	m := map[string]string{}
	for i := range 20000 {
		k := fmt.Sprintf("key%07d", rng.IntN(1_000_000))
		v := fmt.Sprintf("v%d", i)
		set(t, s, k, v)
		m[k] = v
		if i%5000 == 0 {
			checkLevels(t, s)
		}
	}
	checkLevels(t, s)
	met := s.Metrics()
	t.Logf("%+v", met)
	used := 0
	for l := 1; l < numLevels; l++ {
		if met.Levels[l].Tables > 0 {
			used++
		}
	}
	if used < 3 {
		t.Errorf("only %d levels below 0 in use", used)
	}
	// Writes stall at L0StopWrites, but memtables already waiting still
	// flush.
	if met.Levels[0].Tables > s.o.L0StopWrites+s.o.MaxImmutable {
		t.Errorf("level 0 has %d tables", met.Levels[0].Tables)
	}
	verify := func(what string) {
		n := 0
		it := s.Iter(storage.IterOptions{})
		for it.Next() {
			if m[string(it.Key())] != string(it.Value()) {
				t.Fatalf("%s: %s = %s, want %s", what, it.Key(), it.Value(), m[string(it.Key())])
			}
			n++
		}
		it.Close()
		if n != len(m) {
			t.Fatalf("%s: %d keys, want %d", what, n, len(m))
		}
		for k, v := range m {
			if got := get(t, s, k); got != v {
				t.Fatalf("%s: Get(%s) = %s, want %s", what, k, got, v)
			}
		}
	}
	verify("before reopen")
	s.Close()
	s = openOpts(t, tiny(fs))
	verify("after reopen")
	checkLevels(t, s)
}

// failFS fails table creation once armed.
type failFS struct {
	vfs.FS
	armed atomic.Bool
}

func (f *failFS) Create(name string) (vfs.File, error) {
	if f.armed.Load() && strings.HasSuffix(name, ".sst") {
		return nil, fmt.Errorf("injected failure")
	}
	return f.FS.Create(name)
}

// TestBackgroundFailure checks that a failed flush stops writes with an
// error instead of hanging them, and that reopening recovers.
func TestBackgroundFailure(t *testing.T) {
	ffs := &failFS{FS: vfs.NewMem()}
	s := openOpts(t, tiny(ffs))
	for i := range 50 {
		set(t, s, fmt.Sprintf("k%03d", i), strings.Repeat("x", 40))
	}
	ffs.armed.Store(true)
	var err error
	for i := 0; i < 1000 && err == nil; i++ {
		var b storage.Batch
		b.Set(fmt.Appendf(nil, "z%04d", i), []byte(strings.Repeat("y", 40)))
		_, err = s.Apply(&b)
	}
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("writes kept succeeding: %v", err)
	}
	if get(t, s, "k007") != strings.Repeat("x", 40) {
		t.Error("reads stopped working")
	}
	s.Close()
	ffs.armed.Store(false)
	s = openOpts(t, tiny(ffs))
	defer s.Close()
	if get(t, s, "k049") != strings.Repeat("x", 40) {
		t.Error("data lost after a failed flush")
	}
}

// FuzzDecodeEdit checks that decoding arbitrary manifest records never
// panics.
func FuzzDecodeEdit(f *testing.F) {
	e := &edit{nextFile: 3, deleted: []deletedTable{{0, 1}}, added: []addedTable{{level: 1, num: 2, b: bounds{lo: []byte("a"), hi: []byte("b")}}}}
	f.Add(e.encode())
	f.Fuzz(func(t *testing.T, rec []byte) {
		if e, err := decodeEdit(rec); err == nil {
			if _, err := decodeEdit(e.encode()); err != nil {
				t.Fatal(err)
			}
		}
	})
}
