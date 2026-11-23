package lsm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/sandgorgon/polydb/storage/sstable"
)

// numLevels is the number of levels. Level 0 holds flushed memtables,
// which may overlap; each deeper level holds tables with disjoint key
// ranges, about ten times more data than the level above.
const numLevels = 7

// bounds is the range of user keys a table may hold, counting its range
// tombstones: [lo, hi], or [lo, hi) when hiExcl, or [lo, ∞) when hiInf.
type bounds struct {
	lo, hi []byte
	hiExcl bool
	hiInf  bool
}

// beforeEnd reports whether key is below the upper bound.
func (b bounds) beforeEnd(key []byte) bool {
	if b.hiInf {
		return true
	}
	c := bytes.Compare(key, b.hi)
	return c < 0 || (c == 0 && !b.hiExcl)
}

func (b bounds) contains(key []byte) bool {
	return bytes.Compare(key, b.lo) >= 0 && b.beforeEnd(key)
}

func overlaps(a, b bounds) bool { return a.beforeEnd(b.lo) && b.beforeEnd(a.lo) }

func union(a, b bounds) bounds {
	u := a
	if bytes.Compare(b.lo, a.lo) < 0 {
		u.lo = b.lo
	}
	switch {
	case a.hiInf || b.hiInf:
		u.hiInf, u.hi, u.hiExcl = true, nil, false
	default:
		switch c := bytes.Compare(b.hi, a.hi); {
		case c > 0:
			u.hi, u.hiExcl = b.hi, b.hiExcl
		case c == 0:
			u.hiExcl = a.hiExcl && b.hiExcl
		}
	}
	return u
}

// tableMeta describes one table file.
type tableMeta struct {
	num            uint64
	size           uint64
	b              bounds
	minSeq, maxSeq uint64

	r     *sstable.Reader
	tombs []tombstone
	// refs counts the versions holding the table. When it drops to zero
	// the reader is closed, and the file is deleted if an edit removed it.
	refs     atomic.Int32
	obsolete atomic.Bool
}

// version is an immutable set of tables. Readers hold a reference while
// they use its tables.
type version struct {
	s          *Store
	levels     [numLevels][]*tableMeta
	tombstones []tombstone // of every table
	refs       atomic.Int32
}

func (v *version) ref() { v.refs.Add(1) }

func (v *version) unref() {
	if v.refs.Add(-1) != 0 {
		return
	}
	for _, level := range v.levels {
		for _, t := range level {
			if t.refs.Add(-1) == 0 {
				v.s.releaseTable(t)
			}
		}
	}
}

// newVersion builds a version from levels, taking a reference on each
// table. Level 0 is ordered newest first, the others by key.
func (s *Store) newVersion(levels [numLevels][]*tableMeta) *version {
	v := &version{s: s, levels: levels}
	v.refs.Store(1)
	slices.SortFunc(v.levels[0], func(a, b *tableMeta) int {
		if a.maxSeq != b.maxSeq {
			return -cmpU64(a.maxSeq, b.maxSeq)
		}
		return -cmpU64(a.num, b.num)
	})
	for l := 1; l < numLevels; l++ {
		slices.SortFunc(v.levels[l], func(a, b *tableMeta) int { return bytes.Compare(a.b.lo, b.b.lo) })
	}
	for _, level := range v.levels {
		for _, t := range level {
			t.refs.Add(1)
			v.tombstones = append(v.tombstones, t.tombs...)
		}
	}
	return v
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// apply returns the version that results from an edit.
func (v *version) apply(e *edit, added []*tableMeta) *version {
	var levels [numLevels][]*tableMeta
	gone := map[uint64]bool{}
	for _, d := range e.deleted {
		gone[d.num] = true
	}
	for l, level := range v.levels {
		for _, t := range level {
			if !gone[t.num] {
				levels[l] = append(levels[l], t)
			}
		}
	}
	for i, a := range e.added {
		levels[a.level] = append(levels[a.level], added[i])
	}
	return v.s.newVersion(levels)
}

// overlapping returns the tables of a level that overlap b.
func (v *version) overlapping(level int, b bounds) []*tableMeta {
	var out []*tableMeta
	for _, t := range v.levels[level] {
		if overlaps(t.b, b) {
			out = append(out, t)
		}
	}
	return out
}

func (v *version) levelSize(level int) uint64 {
	var n uint64
	for _, t := range v.levels[level] {
		n += t.size
	}
	return n
}

// An edit is one manifest record: tables added and removed, and the
// store's counters. A snapshot edit replaces everything before it.
type edit struct {
	snapshot   bool
	nextFile   uint64 // table numbers below this have been used
	flushedSeq uint64 // every batch up to here is in a table
	logNum     uint64 // log segments below this are no longer needed
	deleted    []deletedTable
	added      []addedTable
}

type deletedTable struct {
	level int
	num   uint64
}

type addedTable struct {
	level          int
	num, size      uint64
	minSeq, maxSeq uint64
	b              bounds
}

func (a addedTable) meta() *tableMeta {
	return &tableMeta{num: a.num, size: a.size, b: a.b, minSeq: a.minSeq, maxSeq: a.maxSeq}
}

func describe(level int, t *tableMeta) addedTable {
	return addedTable{level: level, num: t.num, size: t.size, minSeq: t.minSeq, maxSeq: t.maxSeq, b: t.b}
}

func (e *edit) encode() []byte {
	var b []byte
	if e.snapshot {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	for _, v := range []uint64{e.nextFile, e.flushedSeq, e.logNum, uint64(len(e.deleted))} {
		b = binary.AppendUvarint(b, v)
	}
	for _, d := range e.deleted {
		b = binary.AppendUvarint(binary.AppendUvarint(b, uint64(d.level)), d.num)
	}
	b = binary.AppendUvarint(b, uint64(len(e.added)))
	for _, a := range e.added {
		for _, v := range []uint64{uint64(a.level), a.num, a.size, a.minSeq, a.maxSeq} {
			b = binary.AppendUvarint(b, v)
		}
		b = appendStr(appendStr(b, a.b.lo), a.b.hi)
		var flags byte
		if a.b.hiExcl {
			flags |= 1
		}
		if a.b.hiInf {
			flags |= 2
		}
		b = append(b, flags)
	}
	return b
}

func decodeEdit(rec []byte) (*edit, error) {
	bad := fmt.Errorf("lsm: malformed manifest record")
	r := &reader{b: rec}
	e := &edit{}
	switch r.byte() {
	case 0:
	case 1:
		e.snapshot = true
	default:
		return nil, bad
	}
	e.nextFile, e.flushedSeq, e.logNum = r.uvarint(), r.uvarint(), r.uvarint()
	n := r.uvarint()
	if r.err != nil || n > uint64(len(r.b)) {
		return nil, bad
	}
	for range n {
		d := deletedTable{level: int(r.uvarint()), num: r.uvarint()}
		if d.level >= numLevels {
			return nil, bad
		}
		e.deleted = append(e.deleted, d)
	}
	n = r.uvarint()
	if r.err != nil || n > uint64(len(r.b)) {
		return nil, bad
	}
	for range n {
		a := addedTable{level: int(r.uvarint()), num: r.uvarint(), size: r.uvarint(), minSeq: r.uvarint(), maxSeq: r.uvarint()}
		a.b.lo, a.b.hi = bytes.Clone(r.str()), bytes.Clone(r.str())
		flags := r.byte()
		a.b.hiExcl, a.b.hiInf = flags&1 != 0, flags&2 != 0
		if a.level >= numLevels || flags > 3 {
			return nil, bad
		}
		if a.b.hiInf {
			a.b.hi = nil
		}
		e.added = append(e.added, a)
	}
	if r.err != nil || len(r.b) != 0 {
		return nil, bad
	}
	return e, nil
}

// builder replays manifest edits.
type builder struct {
	levels     [numLevels]map[uint64]addedTable
	nextFile   uint64
	flushedSeq uint64
	logNum     uint64
}

func newBuilder() *builder {
	b := &builder{}
	for i := range b.levels {
		b.levels[i] = map[uint64]addedTable{}
	}
	return b
}

func (b *builder) apply(e *edit) {
	if e.snapshot {
		*b = *newBuilder()
	}
	b.nextFile = max(b.nextFile, e.nextFile)
	b.flushedSeq = max(b.flushedSeq, e.flushedSeq)
	b.logNum = max(b.logNum, e.logNum)
	for _, d := range e.deleted {
		delete(b.levels[d.level], d.num)
	}
	for _, a := range e.added {
		b.levels[a.level][a.num] = a
	}
}
