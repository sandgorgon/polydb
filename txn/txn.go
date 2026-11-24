// Package txn implements snapshot-isolation transactions over a
// storage.Store (spec/rhizome.md §9).
//
// A transaction reads from the snapshot taken when it began, plus its own
// buffered writes. At commit it is checked against every transaction that
// committed after it began:
//
//   - Write-write conflicts: if both wrote the same key, the later commit
//     fails with ErrConflict ("first committer wins").
//   - Guarded reads: keys and ranges registered with Guard must not have
//     been written by those transactions. Callers guard the reads that a
//     constraint check depends on (a uniqueness lookup, a scan for
//     relations before a delete), which prevents the write skew snapshot
//     isolation would otherwise allow.
//
// Commits are serialized by the Manager and applied as one storage batch.
package txn

import (
	"bytes"
	"errors"
	"maps"
	"math"
	"slices"
	"sync"

	"github.com/sandgorgon/polydb/storage"
)

// Errors returned by transactions.
var (
	// ErrConflict means another transaction committed a conflicting change
	// first. The transaction can be retried from the start.
	ErrConflict = errors.New("txn: conflict with a concurrent transaction")
	ErrReadOnly = errors.New("txn: transaction is read-only")
	ErrDone     = errors.New("txn: transaction already committed or rolled back")
)

// span is a key range [lo, hi); hi nil means unbounded.
type span struct{ lo, hi []byte }

func (s span) has(k []byte) bool { return storage.InRange(k, s.lo, s.hi) }

func (s span) overlaps(o span) bool {
	return (s.hi == nil || bytes.Compare(o.lo, s.hi) < 0) && (o.hi == nil || bytes.Compare(s.lo, o.hi) < 0)
}

// commitRecord is what a committed transaction wrote.
type commitRecord struct {
	seq    uint64
	keys   [][]byte
	ranges []span
}

// Manager runs transactions on one store. It is safe for concurrent use.
type Manager struct {
	store storage.Store

	mu     sync.Mutex
	log    []commitRecord  // commits that active transactions may conflict with
	active map[*Txn]uint64 // read-write transactions and their start seq
	open   map[*Txn]uint64 // every open transaction and its snapshot's seq
	hook   func(seq uint64, keys [][]byte)
}

// NewManager returns a transaction manager for s.
func NewManager(s storage.Store) *Manager {
	return &Manager{store: s, active: map[*Txn]uint64{}, open: map[*Txn]uint64{}}
}

// Store returns the underlying store.
func (m *Manager) Store() storage.Store { return m.store }

// OnCommit sets a function called after each commit with the keys it
// wrote (not including range deletions). It is called outside the
// manager's lock, in commit order per key but possibly concurrently.
func (m *Manager) OnCommit(fn func(seq uint64, keys [][]byte)) {
	m.mu.Lock()
	m.hook = fn
	m.mu.Unlock()
}

// Begin starts a transaction.
func (m *Manager) Begin(readOnly bool) *Txn {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &Txn{m: m, snap: m.store.Snapshot(), readOnly: readOnly, writes: map[string]*entry{}}
	if !readOnly {
		m.active[t] = t.snap.Seq()
	}
	m.open[t] = t.snap.Seq()
	return t
}

// Oldest returns the sequence number of the oldest snapshot an open
// transaction reads, or math.MaxUint64 if none is open. Data that only
// snapshots older than this could read can be removed.
func (m *Manager) Oldest() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldest := uint64(math.MaxUint64)
	for _, s := range m.open {
		oldest = min(oldest, s)
	}
	return oldest
}

// finish removes t from the active set and drops log records no active
// transaction can conflict with. m.mu must be held.
func (m *Manager) finish(t *Txn) {
	delete(m.active, t)
	delete(m.open, t)
	oldest := m.store.Seq()
	for _, s := range m.active {
		oldest = min(oldest, s)
	}
	i := 0
	for i < len(m.log) && m.log[i].seq <= oldest {
		i++
	}
	m.log = slices.Delete(m.log, 0, i)
}

// entry is a buffered write.
type entry struct {
	val []byte
	del bool
}

// Txn is one transaction. It is not safe for concurrent use.
type Txn struct {
	m        *Manager
	snap     storage.Snapshot
	readOnly bool
	done     bool
	err      error // first error from a write call, returned by Commit

	writes map[string]*entry
	ranges []span // range deletions
	sorted []string
	guards []span              // guarded ranges
	points map[string]struct{} // guarded single keys
}

// Seq returns the sequence number of the snapshot the transaction reads.
func (t *Txn) Seq() uint64 { return t.snap.Seq() }

// ReadOnly reports whether the transaction is read-only.
func (t *Txn) ReadOnly() bool { return t.readOnly }

// Get reads key, seeing the transaction's own writes.
func (t *Txn) Get(key []byte) ([]byte, bool, error) {
	if t.done {
		return nil, false, ErrDone
	}
	if e, ok := t.writes[string(key)]; ok {
		if e.del {
			return nil, false, nil
		}
		return e.val, true, nil
	}
	if t.rangeDeleted(key) {
		return nil, false, nil
	}
	return t.snap.Get(key)
}

func (t *Txn) rangeDeleted(k []byte) bool {
	for _, r := range t.ranges {
		if r.has(k) {
			return true
		}
	}
	return false
}

func (t *Txn) writable() bool {
	switch {
	case t.done:
		t.fail(ErrDone)
	case t.readOnly:
		t.fail(ErrReadOnly)
	default:
		return true
	}
	return false
}

func (t *Txn) fail(err error) {
	if t.err == nil {
		t.err = err
	}
}

func (t *Txn) put(key []byte, e *entry) {
	k := string(key)
	if _, ok := t.writes[k]; !ok {
		t.sorted = nil
	}
	t.writes[k] = e
}

// Set buffers a write of value to key. Errors (a read-only or finished
// transaction) are reported by Commit.
func (t *Txn) Set(key, value []byte) {
	if t.writable() {
		t.put(key, &entry{val: bytes.Clone(nonNil(value))})
	}
}

// Delete buffers a deletion of key.
func (t *Txn) Delete(key []byte) {
	if t.writable() {
		t.put(key, &entry{del: true})
	}
}

// DeleteRange buffers a deletion of every key in [lo, hi); nil hi is
// unbounded.
func (t *Txn) DeleteRange(lo, hi []byte) {
	if !t.writable() {
		return
	}
	s := span{bytes.Clone(nonNil(lo)), bytes.Clone(hi)}
	t.ranges = append(t.ranges, s)
	for k, e := range t.writes {
		if s.has([]byte(k)) {
			e.val, e.del = nil, true
		}
	}
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// Guard registers a read of [lo, hi) whose result the transaction relies
// on: the commit fails with ErrConflict if a transaction that committed
// after this one began wrote any key in the range. nil hi is unbounded.
func (t *Txn) Guard(lo, hi []byte) {
	if !t.readOnly {
		t.guards = append(t.guards, span{bytes.Clone(nonNil(lo)), bytes.Clone(hi)})
	}
}

// GuardKey guards a single key.
func (t *Txn) GuardKey(key []byte) {
	if !t.readOnly {
		if t.points == nil {
			t.points = map[string]struct{}{}
		}
		t.points[string(key)] = struct{}{}
	}
}

// Rollback discards the transaction. It is safe to call after Commit.
func (t *Txn) Rollback() {
	if t.done {
		return
	}
	t.done = true
	t.snap.Release()
	t.m.mu.Lock()
	t.m.finish(t)
	t.m.mu.Unlock()
}

// Commit applies the transaction's writes atomically and returns the
// commit's sequence number. A read-only transaction, or one with no writes,
// commits without writing and returns its snapshot's sequence number.
func (t *Txn) Commit() (uint64, error) {
	if t.done {
		return 0, ErrDone
	}
	if t.err != nil {
		t.Rollback()
		return 0, t.err
	}
	if t.readOnly || (len(t.writes) == 0 && len(t.ranges) == 0) {
		seq := t.snap.Seq()
		t.Rollback()
		return seq, nil
	}
	t.done = true
	defer t.snap.Release()

	var b storage.Batch
	for _, r := range t.ranges {
		b.DeleteRange(r.lo, r.hi)
	}
	keys := make([][]byte, 0, len(t.writes))
	for _, k := range t.sortedKeys() {
		e := t.writes[k]
		if e.del {
			b.Delete([]byte(k))
		} else {
			b.Set([]byte(k), e.val)
		}
		keys = append(keys, []byte(k))
	}

	m := t.m
	m.mu.Lock()
	start := m.active[t]
	for _, rec := range m.log {
		if rec.seq > start && t.conflicts(rec) {
			m.finish(t)
			m.mu.Unlock()
			return 0, ErrConflict
		}
	}
	// With a Preparer, the order and the conflict record are settled under
	// the lock, and the wait for durability happens outside it, so
	// concurrent commits can share a sync.
	var seq uint64
	var err error
	commit := func() error { return nil }
	if p, ok := m.store.(storage.Preparer); ok {
		seq, commit, err = p.Prepare(&b)
	} else {
		seq, err = m.store.Apply(&b)
	}
	if err != nil {
		m.finish(t)
		m.mu.Unlock()
		return 0, err
	}
	m.log = append(m.log, commitRecord{seq: seq, keys: keys, ranges: t.ranges})
	m.finish(t)
	hook := m.hook
	m.mu.Unlock()
	if err := commit(); err != nil {
		return 0, err
	}
	if hook != nil {
		hook(seq, keys)
	}
	return seq, nil
}

// conflicts reports whether rec wrote anything t wrote or guarded. Its
// cost is linear in what rec wrote, plus a binary search per range, so
// big transactions committing side by side stay cheap.
func (t *Txn) conflicts(rec commitRecord) bool {
	for _, k := range rec.keys {
		if _, ok := t.writes[string(k)]; ok {
			return true
		}
		if _, ok := t.points[string(k)]; ok {
			return true
		}
	}
	// rec.keys are sorted: find the first at or after each range's start.
	for _, s := range slices.Concat(t.guards, t.ranges) {
		i, _ := slices.BinarySearchFunc(rec.keys, s.lo, bytes.Compare)
		if i < len(rec.keys) && s.has(rec.keys[i]) {
			return true
		}
		for _, r := range rec.ranges {
			if s.overlaps(r) {
				return true
			}
		}
	}
	if len(rec.ranges) > 0 {
		writes := t.sortedKeys()
		points := slices.Sorted(maps.Keys(t.points))
		for _, r := range rec.ranges {
			for _, ks := range [][]string{writes, points} {
				i, _ := slices.BinarySearchFunc(ks, r.lo, func(k string, lo []byte) int { return bytes.Compare([]byte(k), lo) })
				if i < len(ks) && r.has([]byte(ks[i])) {
					return true
				}
			}
		}
	}
	return false
}

func (t *Txn) sortedKeys() []string {
	if t.sorted == nil {
		t.sorted = make([]string, 0, len(t.writes))
		for k := range t.writes {
			t.sorted = append(t.sorted, k)
		}
		slices.Sort(t.sorted)
	}
	return t.sorted
}
