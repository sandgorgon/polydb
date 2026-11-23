package lsm

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/sstable"
	"github.com/sandgorgon/polydb/storage/storagetest"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/txn"
)

func openMem(t *testing.T, fs vfs.FS) *Store {
	t.Helper()
	s, err := Open(Options{FS: fs, Dir: "/db"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store { return openMem(t, vfs.NewMem()) })
}

func set(t *testing.T, s storage.Store, kv ...string) uint64 {
	t.Helper()
	var b storage.Batch
	for i := 0; i < len(kv); i += 2 {
		b.Set([]byte(kv[i]), []byte(kv[i+1]))
	}
	seq, err := s.Apply(&b)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func get(t *testing.T, r storage.Reader, key string) string {
	t.Helper()
	v, ok, err := r.Get([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return "<none>"
	}
	return string(v)
}

func scan(t *testing.T, r storage.Reader, reverse bool) string {
	t.Helper()
	it := r.Iter(storage.IterOptions{Reverse: reverse})
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

func TestReopen(t *testing.T) {
	fs := vfs.NewMem()
	s := openMem(t, fs)
	set(t, s, "a", "1", "b", "2")
	set(t, s, "a", "3")
	var b storage.Batch
	b.Delete([]byte("b"))
	b.DeleteRange([]byte("x"), nil)
	s.Apply(&b)
	seq := set(t, s, "c", "4")
	if _, err := Open(Options{FS: fs, Dir: "/db"}); err == nil {
		t.Error("a second store opened the same directory")
	}
	s.Close()
	s = openMem(t, fs)
	defer s.Close()
	if got := scan(t, s, false); got != "a=3 c=4" {
		t.Errorf("after reopen: %s", got)
	}
	if s.Seq() != seq {
		t.Errorf("seq %d, want %d", s.Seq(), seq)
	}
	if next := set(t, s, "d", "5"); next != seq+1 {
		t.Errorf("next seq %d", next)
	}
}

func TestVersionsAndRanges(t *testing.T) {
	s := openMem(t, vfs.NewMem())
	defer s.Close()
	set(t, s, "a", "1", "b", "1", "c", "1")
	old := s.Snapshot()
	defer old.Release()
	set(t, s, "a", "2")
	var b storage.Batch
	b.Set([]byte("b"), []byte("before"))
	b.DeleteRange([]byte("b"), []byte("c")) // cancels the set before it
	b.Set([]byte("bb"), []byte("after"))    // survives: it comes after
	s.Apply(&b)
	if got := scan(t, s, false); got != "a=2 bb=after c=1" {
		t.Errorf("now: %s", got)
	}
	if got := scan(t, s, true); got != "c=1 bb=after a=2" {
		t.Errorf("reverse: %s", got)
	}
	if got := scan(t, old, false); got != "a=1 b=1 c=1" {
		t.Errorf("old snapshot: %s", got)
	}
	if got := scan(t, old, true); got != "c=1 b=1 a=1" {
		t.Errorf("old snapshot reversed: %s", got)
	}
	if get(t, s, "b") != "<none>" || get(t, old, "b") != "1" {
		t.Error("range deletion")
	}
	it := s.Iter(storage.IterOptions{Lower: []byte("b"), Upper: []byte("c")})
	if !it.Next() || string(it.Key()) != "bb" || it.Next() {
		t.Error("bounded scan")
	}
	it.Close()
}

// TestPublishOrder checks that a batch committed out of order stays
// invisible until the batches before it are visible.
func TestPublishOrder(t *testing.T) {
	s := openMem(t, vfs.NewMem())
	defer s.Close()
	var b1, b2 storage.Batch
	b1.Set([]byte("k"), []byte("1"))
	b2.Set([]byte("k"), []byte("2"))
	seq1, c1, _ := s.Prepare(&b1)
	seq2, c2, _ := s.Prepare(&b2)
	if seq2 != seq1+1 {
		t.Fatalf("seqs %d %d", seq1, seq2)
	}
	done := make(chan error)
	go func() { done <- c2() }()
	select {
	case <-done:
		t.Fatal("batch 2 became visible before batch 1")
	case <-time.After(50 * time.Millisecond):
	}
	if get(t, s, "k") != "<none>" {
		t.Error("a prepared batch is visible")
	}
	if err := c1(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if get(t, s, "k") != "2" || s.Seq() != seq2 {
		t.Errorf("after both: %s, seq %d", get(t, s, "k"), s.Seq())
	}
}

// TestCrash crashes during concurrent writes: every acknowledged batch
// survives, and every batch is all or nothing.
func TestCrash(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for iter := range 100 {
		fs := vfs.NewMem()
		s, err := Open(Options{FS: fs, Dir: "/db", LogSegmentSize: int64(200 + rng.IntN(2000))})
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		acked := map[string]bool{}
		var wg sync.WaitGroup
		for w := range 4 {
			n := 5 + rng.IntN(20)
			wg.Go(func() {
				for i := range n {
					tag := fmt.Sprintf("w%d-%d", w, i)
					var b storage.Batch
					for k := range 3 {
						b.Set([]byte(fmt.Sprintf("%s/%d", tag, k)), []byte(tag))
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
		s2, err := Open(Options{FS: crashed, Dir: "/db"})
		if err != nil {
			t.Fatalf("iteration %d: reopen: %v", iter, err)
		}
		counts := map[string]int{}
		it := s2.Iter(storage.IterOptions{})
		for it.Next() {
			counts[string(it.Value())]++
		}
		it.Close()
		for tag := range acked {
			if counts[tag] != 3 {
				t.Fatalf("iteration %d: acknowledged batch %s has %d of 3 keys", iter, tag, counts[tag])
			}
		}
		for tag, n := range counts {
			if n != 3 {
				t.Fatalf("iteration %d: batch %s is partial (%d of 3 keys)", iter, tag, n)
			}
		}
		s2.Close()
	}
}

// countingFS counts syncs.
type countingFS struct {
	vfs.FS
	syncs atomic.Int64
}

type countingFile struct {
	vfs.File
	n *atomic.Int64
}

// Sync takes a little time, as on a real disk; that's what lets
// concurrent commits pile up behind one sync.
func (f countingFile) Sync() error {
	f.n.Add(1)
	time.Sleep(2 * time.Millisecond)
	return f.File.Sync()
}

func (c *countingFS) Create(name string) (vfs.File, error) {
	f, err := c.FS.Create(name)
	return countingFile{f, &c.syncs}, err
}

func (c *countingFS) OpenAppend(name string) (vfs.File, error) {
	f, err := c.FS.OpenAppend(name)
	return countingFile{f, &c.syncs}, err
}

// TestGroupCommitThroughTransactions checks that concurrent transactions
// share syncs: the transaction manager prepares under its lock and waits
// for durability outside it.
func TestGroupCommitThroughTransactions(t *testing.T) {
	fs := &countingFS{FS: vfs.NewMem()}
	s := openMem(t, fs)
	defer s.Close()
	m := txn.NewManager(s)
	before := fs.syncs.Load()
	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			tx := m.Begin(false)
			tx.Set([]byte(fmt.Sprintf("key%03d", i)), []byte("v"))
			if _, err := tx.Commit(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	syncs := fs.syncs.Load() - before
	t.Logf("%d transactions, %d syncs", n, syncs)
	if syncs >= n {
		t.Errorf("no grouping: %d syncs for %d commits", syncs, n)
	}
	keys := 0
	it := s.Iter(storage.IterOptions{})
	for it.Next() {
		keys++
	}
	it.Close()
	if keys != n {
		t.Errorf("%d keys", keys)
	}
}

func TestNormalize(t *testing.T) {
	var b storage.Batch
	b.Set([]byte("a"), []byte("1"))
	b.Set([]byte("a"), []byte("2"))
	b.Delete([]byte("c"))
	b.Set([]byte("m"), []byte("x"))
	b.DeleteRange([]byte("l"), []byte("n"))
	b.Set([]byte("z"), []byte("9"))
	points, ranges := normalize(&b)
	var got []string
	for _, p := range points {
		got = append(got, fmt.Sprintf("%s:%d:%s", p.key, p.kind, p.value))
	}
	if want := []string{"a:1:2", "c:2:", "z:1:9"}; !slices.Equal(got, want) {
		t.Errorf("points %v", got)
	}
	if len(ranges) != 1 || string(ranges[0].start) != "l" {
		t.Errorf("ranges %v", ranges)
	}
	rec := encodeRecord(7, points, ranges)
	seq, p2, r2, err := decodeRecord(rec)
	if err != nil || seq != 7 || len(p2) != 3 || len(r2) != 1 || string(r2[0].end) != "n" || r2[0].seq != 7 {
		t.Errorf("round trip: %d %v %v %v", seq, p2, r2, err)
	}
	for i := range rec {
		if _, _, _, err := decodeRecord(rec[:i]); err == nil {
			t.Errorf("truncated record at %d decoded", i)
		}
	}
}

// FuzzDecodeRecord checks that decoding arbitrary log records never
// panics, and that what decodes re-encodes to a record that decodes the
// same. (Non-minimal varints are accepted: records are only written by
// this package and are checksummed by the log.)
func FuzzDecodeRecord(f *testing.F) {
	var b storage.Batch
	b.Set([]byte("k"), []byte("v"))
	b.Delete([]byte("d"))
	b.DeleteRange([]byte("a"), []byte("b"))
	b.DeleteRange([]byte("x"), nil)
	p, r := normalize(&b)
	f.Add(encodeRecord(9, p, r))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0})
	f.Fuzz(func(t *testing.T, rec []byte) {
		seq, points, ranges, err := decodeRecord(rec)
		if err != nil {
			return
		}
		again := encodeRecord(seq, points, ranges)
		seq2, p2, r2, err := decodeRecord(again)
		if err != nil || seq2 != seq || fmt.Sprint(p2) != fmt.Sprint(points) || fmt.Sprint(r2) != fmt.Sprint(ranges) {
			t.Fatalf("re-encoded record decodes differently: %v", err)
		}
	})
}

// TestMergeWithTable checks that table iterators merge with a memtable:
// newer memtable versions and tombstones hide the table's, and older
// snapshots still see the table's versions.
func TestMergeWithTable(t *testing.T) {
	fs := vfs.NewMem()
	fs.MkdirAll("/t")
	f, _ := fs.Create("/t/1.sst")
	w := sstable.NewWriter(f, sstable.WriterOptions{BlockSize: 64})
	for i := range 20 {
		w.Add(fmt.Appendf(nil, "k%02d", i), 2, kindSet, []byte("table"))
		w.Add(fmt.Appendf(nil, "k%02d", i), 1, kindSet, []byte("older"))
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	rf, _ := fs.Open("/t/1.sst")
	r, err := sstable.Open(rf, sstable.ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mem := newMemtable()
	mem.add(3, []point{{key: []byte("k05"), kind: kindSet, value: []byte("mem")}, {key: []byte("k06"), kind: kindDelete}, {key: []byte("k99"), kind: kindSet, value: []byte("new")}}, nil)
	mem.add(4, nil, []tombstone{{start: []byte("k10"), end: []byte("k15")}})
	for _, tc := range []struct {
		snap    uint64
		reverse bool
		want    string
	}{
		{4, false, "k00=table k01=table k02=table k03=table k04=table k05=mem k07=table k08=table k09=table k15=table k16=table k17=table k18=table k19=table k99=new"},
		{4, true, "k99=new k19=table k18=table k17=table k16=table k15=table k09=table k08=table k07=table k05=mem k04=table k03=table k02=table k01=table k00=table"},
		{1, false, "k00=older k01=older k02=older k03=older k04=older k05=older k06=older k07=older k08=older k09=older k10=older k11=older k12=older k13=older k14=older k15=older k16=older k17=older k18=older k19=older"},
	} {
		it := &userIter{m: newMergeIter([]internalIter{mem.iter(), r.NewIter()}, tc.reverse), snap: tc.snap,
			reverse: tc.reverse, tombstones: mem.rangeDels()}
		var out []string
		for it.Next() {
			out = append(out, string(it.Key())+"="+string(it.Value()))
		}
		it.Close()
		if got := strings.Join(out, " "); got != tc.want {
			t.Errorf("snap %d reverse %v:\n got %s\nwant %s", tc.snap, tc.reverse, got, tc.want)
		}
	}
}
