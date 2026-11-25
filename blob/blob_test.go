package blob

import (
	"bytes"
	"crypto/sha256"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/storage/vfs"
)

func chunk(i int, n int) ([]byte, Hash) {
	b := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, n/2)
	return b, sha256.Sum256(b)
}

func TestPutGetRotateReopen(t *testing.T) {
	fs := vfs.NewMem()
	s, err := Open(Options{FS: fs, Dir: "/b", SegmentSize: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	locs := map[Hash]Loc{}
	for i := range 20 {
		data, h := chunk(i, 3000)
		loc, err := s.Put(h, data)
		if err != nil {
			t.Fatal(err)
		}
		locs[h] = loc
		if err := s.Sync(loc); err != nil {
			t.Fatal(err)
		}
	}
	if _, n := s.Size(); n < 6 {
		t.Fatalf("%d segments, want rotation every 3 chunks", n)
	}
	check := func(s *Store) {
		t.Helper()
		for i := range 20 {
			data, h := chunk(i, 3000)
			got, err := s.Get(h, locs[h])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("chunk %d: %v", i, err)
			}
		}
	}
	check(s)
	s.Close()

	// Everything was synced, so a crash loses nothing.
	fs2 := fs.Crash(rand.New(rand.NewPCG(1, 1)))
	s, err = Open(Options{FS: fs2, Dir: "/b", SegmentSize: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	check(s)
	// New chunks go to a new segment.
	data, h := chunk(99, 100)
	loc, err := s.Put(h, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range locs {
		if l.Seg >= loc.Seg {
			t.Fatalf("new chunk in old segment %d", loc.Seg)
		}
	}
	// A wrong hash or damaged data is an error.
	if _, err := s.Get(Hash{1}, loc); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("wrong hash: %v", err)
	}
	if _, err := s.Get(h, Loc{Seg: loc.Seg, Off: loc.Off, Len: loc.Len + 1}); err == nil {
		t.Fatal("wrong length read")
	}
}

func TestUnsyncedLostSyncedKept(t *testing.T) {
	fs := vfs.NewMem()
	s, _ := Open(Options{FS: fs, Dir: "/b"})
	a, ha := chunk(1, 1000)
	la, _ := s.Put(ha, a)
	if err := s.Sync(la); err != nil {
		t.Fatal(err)
	}
	b, hb := chunk(2, 1000)
	lb, _ := s.Put(hb, b)
	fs2 := fs.Crash(rand.New(rand.NewPCG(2, 2)))
	s2, err := Open(Options{FS: fs2, Dir: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(ha, la); err != nil {
		t.Fatalf("synced chunk lost: %v", err)
	}
	if _, err := s2.Get(hb, lb); err == nil {
		t.Fatal("unsynced chunk survived a crash that tears")
	}
}

func TestRetirePinRemove(t *testing.T) {
	fs := vfs.NewMem()
	s, _ := Open(Options{FS: fs, Dir: "/b", SegmentSize: 2000})
	var locs []Loc
	for i := range 3 {
		data, h := chunk(i, 1500)
		loc, _ := s.Put(h, data)
		locs = append(locs, loc)
	}
	sealed := s.Sealed()
	if len(sealed) != 2 {
		t.Fatalf("sealed %v", sealed)
	}
	unpin := s.Pin(locs[:1])
	s.Retire(sealed[0].Num, 10)
	s.Retire(sealed[1].Num, 10)
	if n, _ := s.Remove(9); n != 0 {
		t.Fatal("removed with an older snapshot open")
	}
	if n, _ := s.Remove(10); n != 1 {
		t.Fatalf("removed %d, want the unpinned one", n)
	}
	unpin()
	unpin()
	if n, _ := s.Remove(10); n != 1 {
		t.Fatalf("removed %d after unpin", n)
	}
	if names, _ := fs.List("/b"); len(names) != 1 {
		t.Fatalf("files left: %v", names)
	}
	if len(s.Sealed()) != 0 {
		t.Fatal("active segment listed as sealed")
	}
}

func TestPending(t *testing.T) {
	s, _ := Open(Options{FS: vfs.NewMem(), Dir: "/b", SegmentSize: 2000})
	data, h := chunk(1, 1500)
	loc, _ := s.Put(h, data)
	s.AddPending(h, loc, time.Hour)
	data2, h2 := chunk(2, 1500)
	s.Put(h2, data2) // seals the first segment
	if got, ok := s.Pending(h); !ok || got != loc {
		t.Fatal("pending chunk not found")
	}
	if len(s.Sealed()) != 0 {
		t.Fatal("segment with a pending upload offered for rewrite")
	}
	s.AddPending(h, loc, -time.Second)
	if _, ok := s.Pending(h); ok {
		t.Fatal("expired pending chunk found")
	}
	if len(s.Sealed()) != 1 {
		t.Fatal("segment not offered after its upload expired")
	}
}
