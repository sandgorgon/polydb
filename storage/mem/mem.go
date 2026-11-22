// Package mem is an in-memory storage.Store, built on a copy-on-write B+
// tree. Snapshots cost one pointer copy and never block writers; old
// versions are freed by the garbage collector once no snapshot uses them.
//
// It is used for tests, for embedded databases that don't need
// persistence, and as the store behind the server until the on-disk engine
// arrives.
package mem

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sandgorgon/polydb/storage"
)

// DefaultNodeSize is the number of entries per tree node before it splits.
const DefaultNodeSize = 64

// version is one published state of the store.
type version struct {
	t   tree
	seq uint64
}

// Store is an in-memory storage.Store. The zero value is not usable; call
// New.
type Store struct {
	cur      atomic.Pointer[version]
	nodeSize int

	mu     sync.Mutex // serializes writers
	gen    uint64
	closed atomic.Bool
}

// New returns an empty store.
func New() *Store { return NewWithNodeSize(DefaultNodeSize) }

// NewWithNodeSize returns an empty store whose tree nodes split above n
// entries (minimum 4). Small sizes are useful in tests to exercise
// splitting and merging.
func NewWithNodeSize(n int) *Store {
	if n < 4 {
		panic(fmt.Sprintf("mem: node size %d below 4", n))
	}
	s := &Store{nodeSize: n}
	s.cur.Store(&version{})
	return s
}

var _ storage.Store = (*Store)(nil)

func (s *Store) Get(key []byte) ([]byte, bool, error) {
	if s.closed.Load() {
		return nil, false, storage.ErrClosed
	}
	v, ok := get(s.cur.Load().t.root, key)
	return v, ok, nil
}

func (s *Store) Iter(opts storage.IterOptions) storage.Iterator {
	if s.closed.Load() {
		return errIterator{storage.ErrClosed}
	}
	return newIterator(s.cur.Load().t.root, opts.Lower, opts.Upper, opts.Reverse)
}

func (s *Store) Seq() uint64 { return s.cur.Load().seq }

// Len returns the number of keys.
func (s *Store) Len() int { return s.cur.Load().t.len }

func (s *Store) Snapshot() storage.Snapshot {
	return &snapshot{v: s.cur.Load(), s: s}
}

func (s *Store) Apply(b *storage.Batch) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return 0, storage.ErrClosed
	}
	old := s.cur.Load()
	s.gen++
	w := &writer{gen: s.gen, maxSize: s.nodeSize, t: old.t}
	for _, op := range b.Ops() {
		switch op.Kind {
		case storage.OpSet:
			w.set(op.Key, op.Value)
		case storage.OpDelete:
			w.delete(op.Key)
		case storage.OpDeleteRange:
			// Collect first: the tree can't change under its own iterator.
			var keys [][]byte
			it := newIterator(w.t.root, op.Key, op.End, false)
			for it.Next() {
				keys = append(keys, it.Key())
			}
			for _, k := range keys {
				w.delete(k)
			}
		default:
			return 0, fmt.Errorf("mem: unknown batch operation %d", op.Kind)
		}
	}
	v := &version{t: w.t, seq: old.seq + 1}
	s.cur.Store(v)
	return v.seq, nil
}

// Close makes further operations fail with storage.ErrClosed. Snapshots
// taken earlier stay readable.
func (s *Store) Close() error {
	s.closed.Store(true)
	return nil
}

type snapshot struct {
	v        *version
	s        *Store
	released atomic.Bool
}

func (sn *snapshot) Get(key []byte) ([]byte, bool, error) {
	if sn.released.Load() {
		return nil, false, storage.ErrReleased
	}
	v, ok := get(sn.v.t.root, key)
	return v, ok, nil
}

func (sn *snapshot) Iter(opts storage.IterOptions) storage.Iterator {
	if sn.released.Load() {
		return errIterator{storage.ErrReleased}
	}
	return newIterator(sn.v.t.root, opts.Lower, opts.Upper, opts.Reverse)
}

func (sn *snapshot) Seq() uint64 { return sn.v.seq }
func (sn *snapshot) Release()    { sn.released.Store(true) }

type errIterator struct{ err error }

func (errIterator) Next() bool     { return false }
func (errIterator) Key() []byte    { return nil }
func (errIterator) Value() []byte  { return nil }
func (e errIterator) Err() error   { return e.err }
func (e errIterator) Close() error { return nil }
