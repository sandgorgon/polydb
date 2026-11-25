package lsm

import (
	"bytes"
	"container/heap"

	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/internal/storage/sstable"
)

// internalIter walks entries in internal key order: user key ascending,
// then newest version first. Memtables and table files provide them.
type internalIter interface {
	SeekGE(key []byte) // first entry with user key ≥ key
	SeekLT(key []byte) // last entry with user key < key
	First()
	Last()
	Next()
	Prev()
	Valid() bool
	Key() []byte
	Seq() uint64
	Kind() uint8
	Value() []byte
	Err() error
	Close() error
}

var _ internalIter = (*sstable.Iter)(nil)

// mergeIter merges several internal iterators in one direction.
type mergeIter struct {
	iters   []internalIter
	h       iterHeap
	reverse bool
}

type iterHeap struct {
	its     []internalIter
	reverse bool
}

func (h iterHeap) Len() int { return len(h.its) }
func (h iterHeap) Less(i, j int) bool {
	a, b := h.its[i], h.its[j]
	c := compare(a.Key(), a.Seq(), b.Key(), b.Seq())
	if h.reverse {
		return c > 0
	}
	return c < 0
}
func (h iterHeap) Swap(i, j int) { h.its[i], h.its[j] = h.its[j], h.its[i] }
func (h *iterHeap) Push(x any)   { h.its = append(h.its, x.(internalIter)) }
func (h *iterHeap) Pop() any {
	x := h.its[len(h.its)-1]
	h.its = h.its[:len(h.its)-1]
	return x
}

func newMergeIter(iters []internalIter, reverse bool) *mergeIter {
	return &mergeIter{iters: iters, reverse: reverse}
}

// start positions every source and builds the heap: at the first entry
// at or after lower (forward), or the last before upper (reverse).
func (m *mergeIter) start(lower, upper []byte) {
	m.h = iterHeap{reverse: m.reverse}
	for _, it := range m.iters {
		switch {
		case !m.reverse && lower != nil:
			it.SeekGE(lower)
		case !m.reverse:
			it.First()
		case upper != nil:
			it.SeekLT(upper)
		default:
			it.Last()
		}
		if it.Valid() {
			m.h.its = append(m.h.its, it)
		}
	}
	heap.Init(&m.h)
}

func (m *mergeIter) valid() bool       { return len(m.h.its) > 0 }
func (m *mergeIter) top() internalIter { return m.h.its[0] }

func (m *mergeIter) next() {
	it := m.top()
	if m.reverse {
		it.Prev()
	} else {
		it.Next()
	}
	if it.Valid() {
		heap.Fix(&m.h, 0)
	} else {
		heap.Pop(&m.h)
	}
}

func (m *mergeIter) err() error {
	for _, it := range m.iters {
		if err := it.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (m *mergeIter) close() {
	for _, it := range m.iters {
		it.Close()
	}
}

// userIter is the storage.Iterator over a snapshot: for each user key in
// the bounds it yields the newest version at or below the snapshot,
// unless that version is a deletion or a range tombstone covers it.
type userIter struct {
	m          *mergeIter
	snap       uint64
	lower      []byte
	upper      []byte
	reverse    bool
	tombstones []tombstone
	started    bool
	closed     bool
	key, value []byte
	err        error
	release    func()
}

func (it *userIter) deleted(key []byte, seq uint64) bool {
	for _, t := range it.tombstones {
		if t.covers(key, seq, it.snap) {
			return true
		}
	}
	return false
}

func (it *userIter) inBounds(key []byte) bool {
	return storage.InRange(key, it.lower, it.upper)
}

func (it *userIter) Next() bool {
	if it.err != nil || it.closed {
		it.key, it.value = nil, nil
		return false
	}
	if !it.started {
		it.started = true
		it.m.start(it.lower, it.upper)
	}
	for it.m.valid() {
		top := it.m.top()
		if !it.inBounds(top.Key()) {
			break
		}
		// Sources may reuse their buffers when they move on, so keep
		// copies.
		key := bytes.Clone(top.Key())
		// Gather this key's versions and pick the newest visible one. In
		// reverse they arrive oldest first, so all are looked at.
		var bestSeq uint64
		var bestKind uint8
		var bestValue []byte
		found := false
		for it.m.valid() && bytes.Equal(it.m.top().Key(), key) {
			e := it.m.top()
			if seq := e.Seq(); seq <= it.snap && (!found || seq > bestSeq) {
				bestSeq, bestKind, bestValue, found = seq, e.Kind(), bytes.Clone(e.Value()), true
			}
			it.m.next()
		}
		if !found || bestKind == kindDelete || it.deleted(key, bestSeq) {
			continue
		}
		it.key, it.value = key, bestValue
		return true
	}
	if err := it.m.err(); err != nil {
		it.err = err
	}
	it.key, it.value = nil, nil
	return false
}

func (it *userIter) Key() []byte   { return it.key }
func (it *userIter) Value() []byte { return it.value }
func (it *userIter) Err() error    { return it.err }

func (it *userIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.m.close()
	if it.release != nil {
		it.release()
		it.release = nil
	}
	return nil
}

// errIter is an iterator that only reports an error.
type errIter struct{ err error }

func (errIter) Next() bool    { return false }
func (errIter) Key() []byte   { return nil }
func (errIter) Value() []byte { return nil }
func (e errIter) Err() error  { return e.err }
func (errIter) Close() error  { return nil }
