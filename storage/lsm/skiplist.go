package lsm

import (
	"math/rand/v2"
	"sync/atomic"
)

// skiplist holds a memtable's entries in internal key order (user key,
// then newest first). One writer inserts at a time; readers never lock:
// links are atomic pointers, and a node is fully built before it is
// linked in.
type skiplist struct {
	head   *node
	height atomic.Int32
	rnd    *rand.Rand // writer only
}

const maxHeight = 12

type node struct {
	key   []byte
	seq   uint64
	kind  uint8
	value []byte
	next  []atomic.Pointer[node]
}

func newSkiplist() *skiplist {
	s := &skiplist{head: &node{next: make([]atomic.Pointer[node], maxHeight)}, rnd: rand.New(rand.NewPCG(1, 2))}
	s.height.Store(1)
	return s
}

func (s *skiplist) randomHeight() int {
	h := 1
	for h < maxHeight && s.rnd.IntN(4) == 0 {
		h++
	}
	return h
}

// before reports whether n sorts before (key, seq).
func before(n *node, key []byte, seq uint64) bool {
	return n != nil && compare(n.key, n.seq, key, seq) < 0
}

// insert adds an entry. Callers serialize inserts.
func (s *skiplist) insert(key []byte, seq uint64, kind uint8, value []byte) {
	var prev [maxHeight]*node
	x := s.head
	for level := int(s.height.Load()) - 1; level >= 0; level-- {
		for next := x.next[level].Load(); before(next, key, seq); next = x.next[level].Load() {
			x = next
		}
		prev[level] = x
	}
	h := s.randomHeight()
	if cur := int(s.height.Load()); h > cur {
		for level := cur; level < h; level++ {
			prev[level] = s.head
		}
		s.height.Store(int32(h))
	}
	n := &node{key: key, seq: seq, kind: kind, value: value, next: make([]atomic.Pointer[node], h)}
	for level := range h {
		n.next[level].Store(prev[level].next[level].Load())
		prev[level].next[level].Store(n)
	}
}

// seekGE returns the first node at or after (key, seq).
func (s *skiplist) seekGE(key []byte, seq uint64) *node {
	x := s.head
	for level := int(s.height.Load()) - 1; level >= 0; level-- {
		for next := x.next[level].Load(); before(next, key, seq); next = x.next[level].Load() {
			x = next
		}
	}
	return x.next[0].Load()
}

// seekLT returns the last node before (key, seq), or nil.
func (s *skiplist) seekLT(key []byte, seq uint64) *node {
	x := s.head
	for level := int(s.height.Load()) - 1; level >= 0; level-- {
		for next := x.next[level].Load(); before(next, key, seq); next = x.next[level].Load() {
			x = next
		}
	}
	if x == s.head {
		return nil
	}
	return x
}

// last returns the last node, or nil.
func (s *skiplist) last() *node {
	x := s.head
	for level := int(s.height.Load()) - 1; level >= 0; level-- {
		for next := x.next[level].Load(); next != nil; next = x.next[level].Load() {
			x = next
		}
	}
	if x == s.head {
		return nil
	}
	return x
}

func (s *skiplist) first() *node { return s.head.next[0].Load() }

// skipIter walks a skiplist; it implements internalIter.
type skipIter struct {
	s *skiplist
	n *node
}

func (it *skipIter) SeekGE(key []byte) { it.n = it.s.seekGE(key, maxSeq) }
func (it *skipIter) SeekLT(key []byte) { it.n = it.s.seekLT(key, maxSeq) }
func (it *skipIter) First()            { it.n = it.s.first() }
func (it *skipIter) Last()             { it.n = it.s.last() }
func (it *skipIter) Next()             { it.n = it.n.next[0].Load() }
func (it *skipIter) Prev()             { it.n = it.s.seekLT(it.n.key, it.n.seq) }
func (it *skipIter) Valid() bool       { return it.n != nil }
func (it *skipIter) Key() []byte       { return it.n.key }
func (it *skipIter) Seq() uint64       { return it.n.seq }
func (it *skipIter) Kind() uint8       { return it.n.kind }
func (it *skipIter) Value() []byte     { return it.n.value }
func (it *skipIter) Err() error        { return nil }
func (it *skipIter) Close() error      { return nil }
