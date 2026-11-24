package lsm

import (
	"fmt"
	"testing"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
)

// A built store opens with every entry, in several tables, and takes
// writes like any other.
func TestBuild(t *testing.T) {
	fs := vfs.NewMem()
	const n = 20000
	key := func(i int) []byte { return fmt.Appendf(nil, "k%06d", i) }
	val := func(i int) []byte { return fmt.Appendf(nil, "value %d %0200d", i, i) }
	i := 0
	err := Build(Options{FS: fs, Dir: "/b", TableSize: 256 << 10}, func() ([]byte, []byte, bool, error) {
		if i == n {
			return nil, nil, false, nil
		}
		i++
		return key(i - 1), val(i - 1), true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{FS: fs, Dir: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if m := s.Metrics(); m.Levels[numLevels-1].Tables < 2 {
		t.Errorf("one table for %d entries: %+v", n, m.Levels)
	}
	it := s.Iter(storage.IterOptions{})
	got := 0
	for it.Next() {
		if string(it.Key()) != string(key(got)) || string(it.Value()) != string(val(got)) {
			t.Fatalf("entry %d: %q", got, it.Key())
		}
		got++
	}
	it.Close()
	if got != n {
		t.Fatalf("%d entries, want %d", got, n)
	}
	var b storage.Batch
	b.Set(key(5), []byte("new"))
	b.Delete(key(6))
	if seq, err := s.Apply(&b); err != nil || seq != 2 {
		t.Fatalf("write after build: seq %d, %v", seq, err)
	}
	s.Close()
	s, err = Open(Options{FS: fs, Dir: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Get(key(5)); string(v) != "new" {
		t.Errorf("after reopen: %q", v)
	}
	if _, ok, _ := s.Get(key(6)); ok {
		t.Error("deleted key back after reopen")
	}
	s.Close()

	// Keys out of order, and a directory in use, are refused.
	keys := [][]byte{[]byte("b"), []byte("a")}
	j := 0
	err = Build(Options{FS: fs, Dir: "/c"}, func() ([]byte, []byte, bool, error) {
		if j == len(keys) {
			return nil, nil, false, nil
		}
		j++
		return keys[j-1], nil, true, nil
	})
	if err == nil {
		t.Error("keys out of order accepted")
	}
	if err := Build(Options{FS: fs, Dir: "/b"}, func() ([]byte, []byte, bool, error) { return nil, nil, false, nil }); err == nil {
		t.Error("built over a store")
	}
	// An empty store builds and opens.
	if err := Build(Options{FS: fs, Dir: "/e"}, func() ([]byte, []byte, bool, error) { return nil, nil, false, nil }); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Options{FS: fs, Dir: "/e"}); err != nil {
		t.Fatal(err)
	} else {
		s.Close()
	}
}
