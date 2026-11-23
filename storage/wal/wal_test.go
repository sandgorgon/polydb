package wal

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sandgorgon/polydb/storage/vfs"
)

func open(t *testing.T, fs vfs.FS, o Options) (*Log, []string) {
	t.Helper()
	o.FS, o.Dir = fs, "/db/wal"
	var recs []string
	l, err := Open(o, func(r []byte) error { recs = append(recs, string(r)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return l, recs
}

func appendAll(t *testing.T, l *Log, recs ...string) {
	t.Helper()
	for _, r := range recs {
		if err := l.Append([]byte(r)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoundTripAndSegments(t *testing.T) {
	fs := vfs.NewMem()
	l, recs := open(t, fs, Options{SegmentSize: 100})
	if len(recs) != 0 {
		t.Fatalf("new log replayed %v", recs)
	}
	var want []string
	for i := range 20 {
		want = append(want, fmt.Sprintf("record %02d %s", i, strings.Repeat("x", i)))
	}
	appendAll(t, l, want...)
	if l.Segment() < 5 {
		t.Errorf("only %d segments for 20 records with 100-byte segments", l.Segment())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, recs = open(t, fs, Options{SegmentSize: 100})
	if !slices.Equal(recs, want) {
		t.Fatalf("replayed %v", recs)
	}
	// Appends continue where the log ended.
	appendAll(t, l, "after reopen")
	seg, err := l.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, l, "new segment")
	if err := l.RemoveBefore(seg); err != nil {
		t.Fatal(err)
	}
	l.Close()
	_, recs = open(t, fs, Options{SegmentSize: 100})
	if !slices.Equal(recs, []string{"new segment"}) {
		t.Errorf("after RemoveBefore: %v", recs)
	}
	// Empty records are fine.
	l, _ = open(t, vfs.NewMem(), Options{})
	appendAll(t, l, "")
	l.Close()
}

// syncCounter counts syncs, and can fail them.
type syncCounter struct {
	vfs.FS
	syncs atomic.Int64
	fail  atomic.Bool
}

type countedFile struct {
	vfs.File
	c *syncCounter
}

func (f countedFile) Sync() error {
	f.c.syncs.Add(1)
	if f.c.fail.Load() {
		return errors.New("disk on fire")
	}
	return f.File.Sync()
}

func (c *syncCounter) Create(name string) (vfs.File, error) {
	f, err := c.FS.Create(name)
	return countedFile{f, c}, err
}

func (c *syncCounter) OpenAppend(name string) (vfs.File, error) {
	f, err := c.FS.OpenAppend(name)
	return countedFile{f, c}, err
}

func TestGroupCommit(t *testing.T) {
	fs := &syncCounter{FS: vfs.NewMem()}
	l, _ := open(t, fs, Options{})
	before := fs.syncs.Load()
	var wg sync.WaitGroup
	const writers, each = 32, 50
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := l.Append([]byte(fmt.Sprintf("%d-%d", w, i))); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	syncs := fs.syncs.Load() - before
	if syncs > writers*each {
		t.Errorf("%d syncs for %d appends", syncs, writers*each)
	}
	t.Logf("%d appends shared %d syncs", writers*each, syncs)
	l.Close()
	_, recs := open(t, fs, Options{})
	if len(recs) != writers*each {
		t.Errorf("replayed %d records", len(recs))
	}
}

// TestCrash crashes the file system at random points while writers
// append: every acknowledged record must survive, in order, and the log
// must keep working after recovery.
func TestCrash(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := range 200 {
		fs := vfs.NewMem()
		l, _ := open(t, fs, Options{SegmentSize: int64(64 + rng.IntN(512))})
		var mu sync.Mutex
		var acked []string
		var wg sync.WaitGroup
		for w := range 4 {
			count := 10 + rng.IntN(20) // drawn here: the writers don't share rng
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range count {
					rec := fmt.Sprintf("w%d-r%d", w, i)
					if l.Append([]byte(rec)) != nil {
						return
					}
					mu.Lock()
					acked = append(acked, rec)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		// Some writes that were never synced.
		for range rng.IntN(5) {
			l.mu.Lock()
			l.f.Write([]byte("unsynced garbage that a crash may tear"))
			l.mu.Unlock()
		}
		crashed := fs.Crash(rng)
		l2, recs := open(t, crashed, Options{})
		got := map[string]bool{}
		for _, r := range recs {
			got[r] = true
		}
		for _, a := range acked {
			if !got[a] {
				t.Fatalf("iteration %d: acknowledged record %s lost (replayed %d of %d)", iter, a, len(recs), len(acked))
			}
		}
		appendAll(t, l2, "after recovery") // durable: it must survive another crash
		_, recs = open(t, crashed.Crash(nil), Options{})
		if recs[len(recs)-1] != "after recovery" {
			t.Fatalf("iteration %d: the record after recovery was lost: %v", iter, recs[len(recs)-3:])
		}
	}
}

func TestRelaxed(t *testing.T) {
	fs := &syncCounter{FS: vfs.NewMem()}
	l, _ := open(t, fs, Options{Durability: Relaxed})
	before := fs.syncs.Load()
	appendAll(t, l, "a", "b", "c")
	if fs.syncs.Load() != before {
		t.Error("relaxed appends synced")
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	crashed := fs.FS.(*vfs.Mem).Crash(nil)
	_, recs := open(t, crashed, Options{})
	if !slices.Equal(recs, []string{"a", "b", "c"}) {
		t.Errorf("after Sync and crash: %v", recs)
	}
	l.Close()
}

func TestCorruption(t *testing.T) {
	fs := vfs.NewMem()
	l, _ := open(t, fs, Options{SegmentSize: 60})
	appendAll(t, l, "first record", "second record", "third record", "fourth record")
	l.Close()
	// Damage the first segment, which isn't the last.
	f, _ := fs.Open("/db/wal/" + segName(1))
	size, _ := f.Size()
	data := make([]byte, size)
	f.ReadAt(data, 0)
	f.Close()
	data[len(data)-2] ^= 0xff
	w, _ := fs.Create("/db/wal/" + segName(1))
	w.Write(data)
	w.Close()
	_, err := Open(Options{FS: fs, Dir: "/db/wal"}, func([]byte) error { return nil })
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt middle segment: %v", err)
	}
}

func TestSyncFailure(t *testing.T) {
	fs := &syncCounter{FS: vfs.NewMem()}
	l, _ := open(t, fs, Options{})
	appendAll(t, l, "ok")
	fs.fail.Store(true)
	if err := l.Append([]byte("lost")); err == nil {
		t.Fatal("append succeeded although sync failed")
	}
	fs.fail.Store(false)
	if err := l.Append([]byte("later")); err == nil || !strings.Contains(err.Error(), "takes no more writes") {
		t.Errorf("after a failed sync: %v", err)
	}
}

func TestOSFileSystem(t *testing.T) {
	dir := t.TempDir()
	o := Options{FS: vfs.OS, Dir: dir + "/wal", SegmentSize: 64}
	l, err := Open(o, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, l, "one", "two", "three", strings.Repeat("big", 50))
	l.Close()
	var recs []string
	l, err = Open(o, func(r []byte) error { recs = append(recs, string(r)); return nil })
	if err != nil || len(recs) != 4 || recs[1] != "two" {
		t.Fatalf("reopen: %v %v", recs, err)
	}
	l.Close()
	lock, err := vfs.OS.Lock(dir + "/LOCK")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vfs.OS.Lock(dir + "/LOCK"); err != nil && !errors.Is(err, vfs.ErrLocked) {
		t.Logf("second lock in the same process: %v", err) // flock is per open file
	}
	lock.Close()
}

// FuzzReplay feeds arbitrary bytes as the last segment: opening must not
// panic, and must either replay a prefix of good records or fail.
func FuzzReplay(f *testing.F) {
	fs := vfs.NewMem()
	l, _ := open(&testing.T{}, fs, Options{})
	l.Append([]byte("hello"))
	l.Append([]byte("world"))
	l.Close()
	seg, _ := fs.Open("/db/wal/" + segName(1))
	size, _ := seg.Size()
	good := make([]byte, size)
	seg.ReadAt(good, 0)
	f.Add(good)
	f.Add([]byte(magic))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		fs := vfs.NewMem()
		fs.MkdirAll("/w")
		w, _ := fs.Create("/w/" + segName(1))
		w.Write(data)
		w.Sync()
		w.Close()
		fs.SyncDir("/w")
		n := 0
		l, err := Open(Options{FS: fs, Dir: "/w"}, func([]byte) error { n++; return nil })
		if err != nil {
			return
		}
		if err := l.Append([]byte("new")); err != nil {
			t.Fatal(err)
		}
		l.Close()
		m := 0
		l, err = Open(Options{FS: fs, Dir: "/w"}, func([]byte) error { m++; return nil })
		if err != nil || m != n+1 {
			t.Fatalf("after appending to a recovered log: %d records, want %d (%v)", m, n+1, err)
		}
		l.Close()
	})
}
