package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/sandgorgon/polydb/storage/sstable"
	"github.com/sandgorgon/polydb/storage/vfs"
)

var errClosing = errors.New("lsm: store is closing")

func tableName(num uint64) string { return fmt.Sprintf("%06d.sst", num) }

// outputs writes the tables a flush or compaction produces. It cuts a
// new table when one reaches the target size, never between versions of
// one user key, and gives each table the parts of the range tombstones
// between its first key and the next table's first key, so that the
// tables' ranges stay disjoint.
type outputs struct {
	s        *Store
	split    bool
	tombs    []tombstone
	w        *sstable.Writer
	num      uint64
	lower    []byte // first key of the current table; nil for the first
	last     []byte // last key added
	cut      bool   // the current table is full: cut at the next new key
	made     []*tableMeta
	pointLo  []byte
	nEntries int
}

func (o *outputs) start() error {
	o.num = o.s.newFileNum()
	f, err := o.s.o.FS.Create(vfs.Join(o.s.o.Dir, tableName(o.num)))
	if err != nil {
		return err
	}
	o.w = sstable.NewWriter(f, sstable.WriterOptions{BlockSize: o.s.o.BlockSize})
	o.nEntries, o.pointLo = 0, nil
	return nil
}

func (o *outputs) add(key []byte, seq uint64, kind uint8, value []byte) error {
	if o.cut && !bytes.Equal(key, o.last) {
		if err := o.finish(key); err != nil {
			return err
		}
		o.lower = bytes.Clone(key)
		o.cut = false
	}
	if o.w == nil {
		if err := o.start(); err != nil {
			return err
		}
	}
	if err := o.w.Add(key, seq, kind, value); err != nil {
		return err
	}
	if o.nEntries == 0 {
		o.pointLo = bytes.Clone(key)
	}
	o.nEntries++
	o.last = append(o.last[:0], key...)
	if o.split && o.w.EstimatedSize() >= uint64(o.s.o.TableSize) {
		o.cut = true
	}
	return nil
}

// finish completes the current table, whose keys end before upper (nil:
// unbounded).
func (o *outputs) finish(upper []byte) error {
	var mine []tombstone
	for _, t := range o.tombs {
		if o.lower != nil && bytes.Compare(t.start, o.lower) < 0 {
			t.start = o.lower
		}
		if upper != nil && (t.end == nil || bytes.Compare(t.end, upper) > 0) {
			t.end = upper
		}
		if t.end != nil && bytes.Compare(t.start, t.end) >= 0 {
			continue
		}
		mine = append(mine, t)
	}
	if o.w == nil {
		if len(mine) == 0 {
			return nil
		}
		if err := o.start(); err != nil {
			return err
		}
	}
	for _, t := range mine {
		if err := o.w.AddTombstone(sstable.Tombstone{Start: t.start, End: t.end, Seq: t.seq}); err != nil {
			return err
		}
	}
	p, err := o.w.Finish()
	o.w = nil
	if err != nil {
		return err
	}
	m := &tableMeta{num: o.num, size: p.Size, minSeq: p.MinSeq, maxSeq: p.MaxSeq}
	first := true
	grow := func(b bounds) {
		if first {
			m.b, first = b, false
		} else {
			m.b = union(m.b, b)
		}
	}
	if p.Entries > 0 {
		grow(bounds{lo: p.Smallest, hi: p.Largest})
	}
	for _, t := range mine {
		grow(bounds{lo: bytes.Clone(t.start), hi: bytes.Clone(t.end), hiExcl: t.end != nil, hiInf: t.end == nil})
	}
	o.made = append(o.made, m)
	return nil
}

func (o *outputs) close() error {
	if o.w != nil || len(o.tombs) > 0 {
		return o.finish(nil)
	}
	return nil
}

// abandon removes whatever was written.
func (o *outputs) abandon() {
	if o.w != nil {
		o.w.Finish()
		o.s.o.FS.Remove(vfs.Join(o.s.o.Dir, tableName(o.num)))
	}
	for _, m := range o.made {
		if m.r != nil {
			m.r.Close()
		}
		o.s.o.FS.Remove(vfs.Join(o.s.o.Dir, tableName(m.num)))
	}
}

// open makes the new tables durable and opens them.
func (o *outputs) open() error {
	if len(o.made) == 0 {
		return nil
	}
	if err := o.s.o.FS.SyncDir(o.s.o.Dir); err != nil {
		return err
	}
	for _, m := range o.made {
		if err := o.s.openTable(m); err != nil {
			return err
		}
	}
	return nil
}

// flush writes the oldest immutable memtable to a level-0 table.
func (s *Store) flush(imm *memtable) error {
	// Rotating the log when the memtable was handed over already synced
	// its batches. Waiting until they are published as well means that a
	// batch whose commit failed (the store has then failed) never reaches
	// a table.
	if err := s.waitPublished(imm.maxSeq); err != nil {
		return err
	}
	o := &outputs{s: s, tombs: imm.rangeDels()}
	for n := imm.list.first(); n != nil; n = n.next[0].Load() {
		if err := o.add(n.key, n.seq, n.kind, n.value); err != nil {
			o.abandon()
			return err
		}
	}
	err := o.close()
	if err == nil {
		err = o.open()
	}
	if err != nil {
		o.abandon()
		return err
	}
	e := &edit{flushedSeq: imm.maxSeq, logNum: imm.logSeg}
	for _, m := range o.made {
		e.added = append(e.added, describe(0, m))
	}
	if err := s.logAndApply(e, o.made, imm); err != nil {
		o.abandon()
		return err
	}
	for _, m := range o.made {
		s.flushBytes.Add(m.size)
	}
	return s.log.RemoveBefore(imm.logSeg)
}

// compaction merges tables of one level into the next.
type compaction struct {
	level  int // inputs from level and level+1; outputs to level+1
	inputs [2][]*tableMeta
	v      *version
	force  bool // rewrite even a single table, to collect garbage
}

// out is the output level: the next one, or the last level itself when
// a forced compaction rewrites it.
func (c *compaction) out() int { return min(c.level+1, numLevels-1) }

func spanOf(ts []*tableMeta) bounds {
	b := ts[0].b
	for _, t := range ts[1:] {
		b = union(b, t.b)
	}
	return b
}

func (s *Store) levelTarget(level int) uint64 {
	t := uint64(s.o.LevelBaseSize)
	for range level - 1 {
		t *= 10
	}
	return t
}

// pick chooses the most urgent compaction whose levels no running
// compaction uses, or nil. The caller holds cmu.
func (s *Store) pick(v *version) *compaction {
	best, level := 1.0, -1
	free := func(l int) bool { return !s.busy[l] && !s.busy[l+1] }
	if score := float64(len(v.levels[0])) / float64(s.o.L0CompactionTrigger); score >= best && free(0) {
		best, level = score, 0
	}
	for l := 1; l < numLevels-1; l++ {
		if score := float64(v.levelSize(l)) / float64(s.levelTarget(l)); score >= best && free(l) {
			best, level = score, l
		}
	}
	if level < 0 {
		return nil
	}
	c := &compaction{level: level, v: v}
	if level == 0 {
		c.inputs[0] = v.levels[0]
	} else {
		// Take turns through the level's key space.
		files := v.levels[level]
		ptr := s.compactPtr[level]
		i := sort.Search(len(files), func(i int) bool { return bytes.Compare(files[i].b.lo, ptr) > 0 })
		if ptr == nil || i == len(files) {
			i = 0
		}
		c.inputs[0] = files[i : i+1]
		s.compactPtr[level] = files[i].b.lo
	}
	c.inputs[1] = v.overlapping(level+1, spanOf(c.inputs[0]))
	return c
}

// isBase reports whether no level below out has keys in b, so that
// deletions there have nothing left to hide.
func (c *compaction) isBase(b bounds) bool {
	for l := c.out() + 1; l < numLevels; l++ {
		for _, t := range c.v.levels[l] {
			if overlaps(t.b, b) {
				return false
			}
		}
	}
	return true
}

// compact runs a compaction. Of each key's versions it keeps the newest
// one each snapshot can see; it drops versions a range tombstone hides
// from every snapshot that could see them, and deletions (point or
// range) that every snapshot sees once nothing older lies beneath them.
func (s *Store) compact(c *compaction) error {
	e := &edit{}
	for i, ts := range c.inputs {
		for _, t := range ts {
			e.deleted = append(e.deleted, deletedTable{c.level + i, t.num})
		}
	}
	// A single table with nothing to merge into just moves down.
	if len(c.inputs[0]) == 1 && len(c.inputs[1]) == 0 && !c.force {
		t := c.inputs[0][0]
		e.added = []addedTable{describe(c.out(), t)}
		return s.logAndApply(e, []*tableMeta{t}, nil)
	}

	snaps := s.snapshotList()
	stripe := func(seq uint64) int { return sort.Search(len(snaps), func(i int) bool { return snaps[i] >= seq }) }

	var iters []internalIter
	var tombs []tombstone
	for _, ts := range c.inputs {
		for _, t := range ts {
			iters = append(iters, t.r.NewIter())
			tombs = append(tombs, t.tombs...)
		}
	}
	m := newMergeIter(iters, false)
	defer m.close()
	m.start(nil, nil)

	o := &outputs{s: s, split: true}
	for _, t := range tombs {
		if stripe(t.seq) == 0 && c.isBase(bounds{lo: t.start, hi: t.end, hiExcl: true, hiInf: t.end == nil}) {
			continue
		}
		o.tombs = append(o.tombs, t)
	}
	hidden := func(key []byte, seq uint64) bool {
		for _, t := range tombs {
			if t.seq > seq && t.contains(key) && stripe(t.seq) == stripe(seq) {
				return true
			}
		}
		return false
	}

	var key []byte
	lastStripe := -1
	n := 0
	for ; m.valid(); m.next() {
		if n++; n%1024 == 0 && s.quitting() {
			o.abandon()
			return errClosing
		}
		x := m.top()
		if !bytes.Equal(x.Key(), key) {
			key = append(key[:0], x.Key()...)
			lastStripe = -1
		}
		seq := x.Seq()
		st := stripe(seq)
		if st == lastStripe {
			continue // a newer version in the same stripe hides it
		}
		lastStripe = st
		if hidden(key, seq) {
			continue
		}
		if x.Kind() == kindDelete && st == 0 && c.isBase(bounds{lo: key, hi: key}) {
			continue
		}
		if err := o.add(key, seq, x.Kind(), x.Value()); err != nil {
			o.abandon()
			return err
		}
	}
	if err := m.err(); err != nil {
		o.abandon()
		return err
	}
	if err := o.close(); err != nil {
		o.abandon()
		return err
	}
	if err := o.open(); err != nil {
		o.abandon()
		return err
	}
	for _, t := range o.made {
		e.added = append(e.added, describe(c.out(), t))
	}
	s.compactions.Add(1)
	for _, m := range o.made {
		s.compactBytes.Add(m.size)
	}
	if err := s.logAndApply(e, o.made, nil); err != nil {
		o.abandon()
		return err
	}
	return nil
}
