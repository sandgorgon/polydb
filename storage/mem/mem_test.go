package mem

import (
	"fmt"
	"testing"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(*testing.T) storage.Store { return New() })
}

// Small nodes make the random test split and merge constantly.
func TestConformanceSmallNodes(t *testing.T) {
	for _, size := range []int{4, 8} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			storagetest.Run(t, func(*testing.T) storage.Store { return NewWithNodeSize(size) })
		})
	}
}

// TestTreeShape checks the B+ tree invariants after heavy inserting and
// deleting: keys sorted, separators bounding their children, all leaves at
// one depth and the entry count right.
func TestTreeShape(t *testing.T) {
	for _, size := range []int{4, 8, 16, 64} {
		t.Run(fmt.Sprint(size), func(t *testing.T) { testTreeShape(t, size) })
	}
}

func testTreeShape(t *testing.T, size int) {
	s := NewWithNodeSize(size)
	checkTree(t, s, 0)
	if it := s.Iter(storage.IterOptions{Reverse: true}); it.Next() {
		t.Fatal("empty store iterated")
	}
	var b storage.Batch
	for i := range 5000 {
		b.Set(fmt.Appendf(nil, "%05d", i*7919%5000), nil)
	}
	s.Apply(&b)
	checkTree(t, s, 5000)
	// Thin out most keys so nodes shrink and merge, at every level.
	for _, keep := range []int{2, 3, 5} {
		b.Reset()
		for i := range 5000 {
			if i%keep != 0 {
				b.Delete(fmt.Appendf(nil, "%05d", i))
			}
		}
		s.Apply(&b)
	}
	checkTree(t, s, 167) // multiples of 30 below 5000
	b.Reset()
	b.DeleteRange(nil, nil)
	s.Apply(&b)
	checkTree(t, s, 0)
}

func checkTree(t *testing.T, s *Store, want int) {
	t.Helper()
	v := s.cur.Load()
	if v.t.len != want || s.Len() != want {
		t.Fatalf("len %d, want %d", v.t.len, want)
	}
	leafDepth := -1
	count := 0
	var walk func(n *node, lo, hi []byte, depth int)
	walk = func(n *node, lo, hi []byte, depth int) {
		for i, k := range n.keys {
			if i > 0 && string(n.keys[i-1]) >= string(k) {
				t.Fatalf("keys out of order at depth %d", depth)
			}
			if lo != nil && string(k) < string(lo) || hi != nil && string(k) >= string(hi) {
				t.Fatalf("key %q outside [%q, %q)", k, lo, hi)
			}
		}
		if n.leaf() {
			if leafDepth >= 0 && depth != leafDepth {
				t.Fatalf("leaves at depths %d and %d", leafDepth, depth)
			}
			leafDepth = depth
			count += len(n.keys)
			return
		}
		if len(n.keys) != len(n.children)-1 {
			t.Fatalf("%d keys for %d children", len(n.keys), len(n.children))
		}
		for i, c := range n.children {
			if c.size() == 0 {
				t.Fatal("empty child")
			}
			clo, chi := lo, hi
			if i > 0 {
				clo = n.keys[i-1]
			}
			if i < len(n.keys) {
				chi = n.keys[i]
			}
			walk(c, clo, chi, depth+1)
		}
	}
	if v.t.root != nil {
		walk(v.t.root, nil, nil, 0)
	}
	if count != want {
		t.Fatalf("counted %d entries, want %d", count, want)
	}
}

func BenchmarkSet(b *testing.B) {
	s := New()
	var batch storage.Batch
	for i := range b.N {
		batch.Reset()
		batch.Set(fmt.Appendf(nil, "%016d", i*7919), []byte("value"))
		s.Apply(&batch)
	}
}

func BenchmarkGet(b *testing.B) {
	s := New()
	var batch storage.Batch
	for i := range 100_000 {
		batch.Set(fmt.Appendf(nil, "%016d", i), []byte("value"))
	}
	s.Apply(&batch)
	b.ResetTimer()
	for i := range b.N {
		s.Get(fmt.Appendf(nil, "%016d", i%100_000))
	}
}
