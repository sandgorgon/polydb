package storagetest

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/storage"
)

// RunReopen runs conformance tests for persistent stores: what a store
// holds when it is closed, it holds when it is opened again. open must
// open the same store (for each subtest, a new empty one the first time)
// every time it is called; the tests close each store they open.
func RunReopen(t *testing.T, newOpener func(t *testing.T) (open func() storage.Store)) {
	tests := []struct {
		name string
		fn   func(*testing.T, func() storage.Store)
	}{
		{"Persist", testPersist},
		{"Seq", testReopenSeq},
		{"Random", testReopenRandom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newOpener(t)) })
	}
}

func testPersist(t *testing.T, open func() storage.Store) {
	s := open()
	set(t, s, "a", "1", "b", "2", "c", "3", "d", "4")
	apply(t, s, func(b *storage.Batch) {
		b.Delete([]byte("b"))
		b.DeleteRange([]byte("c"), []byte("d"))
		b.Set([]byte("e"), nil) // an empty value is a value
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer s.Close()
	if got := strings.Join(scan(t, s, storage.IterOptions{}), " "); got != "a=1 d=4 e=" {
		t.Errorf("after reopen: %s", got)
	}
	wantGet(t, s, "e", "", true)
	wantGet(t, s, "b", "", false)
}

func testReopenSeq(t *testing.T, open func() storage.Store) {
	s := open()
	set(t, s, "a", "1")
	set(t, s, "a", "2")
	seq := s.Seq()
	s.Close()
	s = open()
	defer s.Close()
	if s.Seq() != seq {
		t.Errorf("Seq after reopen = %d, want %d", s.Seq(), seq)
	}
	var b storage.Batch
	b.Set([]byte("a"), []byte("3"))
	if next, err := s.Apply(&b); err != nil || next <= seq {
		t.Errorf("next batch got %d (%v), after %d", next, err, seq)
	}
	snap := s.Snapshot()
	defer snap.Release()
	if snap.Seq() <= seq {
		t.Errorf("snapshot at %d", snap.Seq())
	}
}

// testReopenRandom applies random batches across many reopens and
// compares the store with a model each time.
func testReopenRandom(t *testing.T, open func() storage.Store) {
	rng := rand.New(rand.NewPCG(3, 4))
	model := map[string]string{}
	s := open()
	defer func() { s.Close() }()
	for round := range 30 {
		for range 20 + rng.IntN(60) {
			var b storage.Batch
			for range 1 + rng.IntN(8) {
				k := fmt.Sprintf("%03d", rng.IntN(300))
				switch x := rng.IntN(20); {
				case x < 14:
					v := fmt.Sprintf("%d-%s", round, strings.Repeat("v", rng.IntN(50)))
					b.Set([]byte(k), []byte(v))
					model[k] = v
				case x < 19:
					b.Delete([]byte(k))
					delete(model, k)
				default:
					end := fmt.Sprintf("%03d", rng.IntN(300))
					b.DeleteRange([]byte(k), []byte(end))
					for mk := range model {
						if mk >= k && mk < end {
							delete(model, mk)
						}
					}
				}
			}
			if _, err := s.Apply(&b); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = open()
		var want []string
		for _, k := range slices.Sorted(maps.Keys(model)) {
			want = append(want, k+"="+model[k])
		}
		if got := scan(t, s, storage.IterOptions{}); !slices.Equal(got, want) {
			t.Fatalf("round %d, after reopen:\ngot  %.300q\nwant %.300q", round, got, want)
		}
	}
}
