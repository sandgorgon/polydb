package lsm

import (
	"sync"
	"sync/atomic"
)

// memtable holds recent writes in memory: point entries in a skiplist,
// and range tombstones in a list (they are rare: dropping an index is the
// only user).
type memtable struct {
	list *skiplist
	size atomic.Int64 // approximate bytes

	mu         sync.RWMutex
	tombstones []tombstone
}

func newMemtable() *memtable { return &memtable{list: newSkiplist()} }

// add inserts a batch's entries. Callers serialize adds.
func (m *memtable) add(seq uint64, points []point, ranges []tombstone) {
	for _, p := range points {
		m.list.insert(p.key, seq, p.kind, p.value)
		m.size.Add(int64(len(p.key) + len(p.value) + 48))
	}
	if len(ranges) > 0 {
		m.mu.Lock()
		for _, r := range ranges {
			r.seq = seq
			m.tombstones = append(m.tombstones, r)
			m.size.Add(int64(len(r.start) + len(r.end) + 32))
		}
		m.mu.Unlock()
	}
}

// get returns the newest version of key at or below snap.
func (m *memtable) get(key []byte, snap uint64) (n *node, ok bool) {
	n = m.list.seekGE(key, snap)
	if n == nil || string(n.key) != string(key) {
		return nil, false
	}
	return n, true
}

// rangeDels returns the memtable's range tombstones.
func (m *memtable) rangeDels() []tombstone {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tombstones[:len(m.tombstones):len(m.tombstones)]
}

func (m *memtable) iter() internalIter { return &skipIter{s: m.list} }
