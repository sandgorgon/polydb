package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/internal/storage/sstable"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/internal/storage/wal"
)

// Options configure a store; zero values get defaults.
type Options struct {
	FS  vfs.FS // default vfs.OS
	Dir string
	// Durability: durable (default) or relaxed commits (wal.Durability).
	Durability wal.Durability
	// LogSegmentSize is the write-ahead log's segment size.
	LogSegmentSize int64

	MemtableSize        int64 // flush when the memtable reaches this; default 64 MiB
	MaxImmutable        int   // memtables waiting to flush before writes stall; default 2
	TableSize           int64 // target size of compacted tables; default 8 MiB
	LevelBaseSize       int64 // target size of level 1; ×10 per level; default 64 MiB
	L0CompactionTrigger int   // level-0 tables that start a compaction; default 4
	L0StopWrites        int   // level-0 tables that stall writes; default 24 (bloom filters keep reads cheap)
	CacheSize           int64 // block cache size; default 64 MiB
	BlockSize           int   // table block size; default 4 KiB
	NoMmap              bool  // read tables with ReadAt, through the cache
	// MaxCompactions is how many compactions may run at once, on
	// different levels; default 2, so that a long compaction between
	// deeper levels doesn't hold up level 0, which writes wait on.
	MaxCompactions int
}

func (o *Options) defaults() {
	if o.FS == nil {
		o.FS = vfs.OS
	}
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.MemtableSize, 64<<20)
	def(&o.TableSize, 8<<20)
	def(&o.LevelBaseSize, 64<<20)
	def(&o.CacheSize, 64<<20)
	if o.MaxImmutable <= 0 {
		o.MaxImmutable = 2
	}
	if o.L0CompactionTrigger <= 0 {
		o.L0CompactionTrigger = 4
	}
	if o.L0StopWrites <= 0 {
		o.L0StopWrites = 24
	}
	if o.MaxCompactions <= 0 {
		o.MaxCompactions = 2
	}
}

// Store is an LSM store. It is safe for concurrent use.
//
// Lock order: wmu, then stateMu, then pubMu; vmu before stateMu; cmu
// before vmu. snapMu is a leaf.
type Store struct {
	o        Options
	lock     io.Closer
	log      *wal.Log
	manifest *wal.Log
	cache    *sstable.Cache

	// wmu orders writes: it assigns sequence numbers, writes the log and
	// fills the memtable in the same order.
	wmu  sync.Mutex
	next uint64

	// stateMu guards what readers see: the memtable, the memtables
	// waiting to flush (oldest first) and the current version.
	stateMu   sync.RWMutex
	stallCond *sync.Cond // on stateMu: flushes and compactions finished
	mem       *memtable
	imms      []*memtable
	cur       *version

	// Batches become visible in sequence order, once durable.
	pubMu     sync.Mutex
	pubCond   *sync.Cond
	published atomic.Uint64
	failed    error // a write failed: the store takes no more writes

	// Counters since the store opened (Metrics).
	stalls, stallNanos       atomic.Int64
	flushBytes, compactBytes atomic.Uint64
	compactions              atomic.Int64

	// vmu serializes version edits.
	vmu        sync.Mutex
	nextFile   atomic.Uint64
	flushedSeq uint64
	logNum     uint64
	editBytes  int

	// cmu guards which levels compactions are using and where each
	// level's compactions take turns. A compaction from level l uses l
	// and l+1; compactions run at once only on levels apart.
	cmu        sync.Mutex
	cmuCond    *sync.Cond // on cmu: a compaction finished
	busy       [numLevels]bool
	compactPtr [numLevels][]byte

	snapMu sync.Mutex
	snaps  map[uint64]int // live snapshots by sequence number

	flushCh, compactCh chan struct{}
	quit               chan struct{}
	wg                 sync.WaitGroup
	closed             atomic.Bool
}

var _ storage.Store = (*Store)(nil)
var _ storage.Preparer = (*Store)(nil)

// manifestRewrite is how much the manifest may grow before it is
// rewritten as one snapshot.
const manifestRewrite = 1 << 20

// Open opens the store in o.Dir, creating it if needed, and recovers it:
// the tables the manifest lists, then the batches in the log that no
// table holds yet.
func Open(o Options) (_ *Store, err error) {
	o.defaults()
	if err := o.FS.MkdirAll(o.Dir); err != nil {
		return nil, err
	}
	lock, err := o.FS.Lock(vfs.Join(o.Dir, "LOCK"))
	if err != nil {
		if errors.Is(err, vfs.ErrLocked) {
			return nil, fmt.Errorf("lsm: %s is in use by another process", o.Dir)
		}
		return nil, err
	}
	s := &Store{o: o, lock: lock, mem: newMemtable(), snaps: map[uint64]int{},
		cache: sstable.NewCache(o.CacheSize), flushCh: make(chan struct{}, 1),
		compactCh: make(chan struct{}, 1), quit: make(chan struct{})}
	s.pubCond = sync.NewCond(&s.pubMu)
	s.stallCond = sync.NewCond(&s.stateMu)
	defer func() {
		if err != nil {
			if s.cur != nil {
				s.cur.unref()
			}
			if s.manifest != nil {
				s.manifest.Close()
			}
			if s.log != nil {
				s.log.Close()
			}
			lock.Close()
		}
	}()

	b := newBuilder()
	s.manifest, err = wal.Open(wal.Options{FS: o.FS, Dir: vfs.Join(o.Dir, "manifest"), SegmentSize: 1 << 40},
		func(rec []byte) error {
			e, err := decodeEdit(rec)
			if err != nil {
				return err
			}
			b.apply(e)
			return nil
		})
	if err != nil {
		return nil, err
	}
	s.nextFile.Store(max(b.nextFile, 1))
	s.flushedSeq, s.logNum = b.flushedSeq, b.logNum
	var levels [numLevels][]*tableMeta
	live := map[string]bool{}
	for l, m := range b.levels {
		for _, a := range m {
			t := a.meta()
			if err := s.openTable(t); err != nil {
				for _, level := range levels {
					for _, t := range level {
						t.r.Close()
					}
				}
				return nil, fmt.Errorf("lsm: table %s: %w", tableName(t.num), err)
			}
			levels[l] = append(levels[l], t)
			live[tableName(t.num)] = true
		}
	}
	s.cur = s.newVersion(levels)
	// Tables a crash left behind before the manifest listed them.
	names, err := o.FS.List(o.Dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".sst") && !live[name] {
			if _, perr := strconv.ParseUint(strings.TrimSuffix(name, ".sst"), 10, 64); perr == nil {
				o.FS.Remove(vfs.Join(o.Dir, name))
			}
		}
	}
	if err := s.writeSnapshot(); err != nil {
		return nil, err
	}

	last := s.flushedSeq
	replay := func(rec []byte) error {
		seq, points, ranges, err := decodeRecord(rec)
		if err != nil {
			return err
		}
		if seq <= s.flushedSeq {
			return nil // already in a table
		}
		if seq <= last {
			return fmt.Errorf("lsm: log record %d after %d", seq, last)
		}
		s.mem.add(seq, points, ranges)
		last = seq
		return nil
	}
	s.log, err = wal.Open(wal.Options{FS: o.FS, Dir: vfs.Join(o.Dir, "wal"), Durability: o.Durability,
		SegmentSize: o.LogSegmentSize}, replay)
	if err != nil {
		return nil, err
	}
	if err := s.log.RemoveBefore(s.logNum); err != nil {
		return nil, err
	}
	// The log and manifest directories and the lock file must survive a
	// crash of a new store.
	if err := o.FS.SyncDir(o.Dir); err != nil {
		return nil, err
	}
	s.next = last + 1
	s.published.Store(last)
	s.cmuCond = sync.NewCond(&s.cmu)
	s.wg.Add(1 + o.MaxCompactions)
	go s.flusher()
	for range o.MaxCompactions {
		go s.compactor()
	}
	notify(s.compactCh)
	return s, nil
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Store) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *Store) newFileNum() uint64 { return s.nextFile.Add(1) - 1 }

// openTable opens a table's reader.
func (s *Store) openTable(t *tableMeta) error {
	f, err := s.o.FS.Open(vfs.Join(s.o.Dir, tableName(t.num)))
	if err != nil {
		return err
	}
	r, err := sstable.Open(f, sstable.ReaderOptions{ID: t.num, Cache: s.cache, NoMmap: s.o.NoMmap})
	if err != nil {
		return err
	}
	t.r = r
	for _, x := range r.Tombstones() {
		t.tombs = append(t.tombs, tombstone{start: x.Start, end: x.End, seq: x.Seq})
	}
	return nil
}

// releaseTable closes a table no version holds, and deletes its file if
// an edit removed it.
func (s *Store) releaseTable(t *tableMeta) {
	t.r.Close()
	if t.obsolete.Load() {
		s.o.FS.Remove(vfs.Join(s.o.Dir, tableName(t.num)))
		s.cache.EvictTable(t.num)
	}
}

// writeSnapshot starts a new manifest segment holding the whole current
// state, then removes the old segments. The caller holds vmu, or is
// Open.
func (s *Store) writeSnapshot() error {
	seg, err := s.manifest.Rotate()
	if err != nil {
		return err
	}
	e := &edit{snapshot: true, nextFile: s.nextFile.Load(), flushedSeq: s.flushedSeq, logNum: s.logNum}
	for l, level := range s.cur.levels {
		for _, t := range level {
			e.added = append(e.added, describe(l, t))
		}
	}
	if err := s.manifest.Append(e.encode()); err != nil {
		return err
	}
	s.editBytes = 0
	return s.manifest.RemoveBefore(seg)
}

// logAndApply records an edit in the manifest and installs the version
// it makes. added are the tables the edit adds, in order; flushed is the
// memtable a flush wrote, which readers stop seeing.
func (s *Store) logAndApply(e *edit, added []*tableMeta, flushed *memtable) error {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	if flushed != nil {
		s.flushedSeq = max(s.flushedSeq, flushed.maxSeq)
		s.logNum = max(s.logNum, flushed.logSeg)
	}
	e.nextFile, e.flushedSeq, e.logNum = s.nextFile.Load(), s.flushedSeq, s.logNum
	rec := e.encode()
	if err := s.manifest.Append(rec); err != nil {
		return err
	}
	s.editBytes += len(rec)

	kept := map[uint64]bool{}
	for _, a := range e.added {
		kept[a.num] = true
	}
	s.stateMu.Lock()
	old := s.cur
	for _, level := range old.levels {
		for _, t := range level {
			for _, d := range e.deleted {
				if d.num == t.num && !kept[t.num] {
					t.obsolete.Store(true)
				}
			}
		}
	}
	s.cur = old.apply(e, added)
	if flushed != nil {
		if len(s.imms) == 0 || s.imms[0] != flushed {
			panic("lsm: flushed memtable is not the oldest")
		}
		s.imms = s.imms[1:]
	}
	s.stallCond.Broadcast()
	s.stateMu.Unlock()
	old.unref()
	notify(s.compactCh)
	if s.editBytes > manifestRewrite {
		return s.writeSnapshot()
	}
	return nil
}

func (s *Store) flusher() {
	defer s.wg.Done()
	for {
		select {
		case <-s.quit:
			return
		case <-s.flushCh:
		}
		for {
			s.stateMu.RLock()
			var imm *memtable
			if len(s.imms) > 0 {
				imm = s.imms[0]
			}
			s.stateMu.RUnlock()
			if imm == nil {
				break
			}
			if err := s.flush(imm); err != nil {
				if !errors.Is(err, errClosing) {
					s.fail(err)
				}
				return
			}
		}
	}
}

func (s *Store) compactor() {
	defer s.wg.Done()
	for {
		select {
		case <-s.quit:
			return
		case <-s.compactCh:
		}
		for !s.quitting() {
			did, err := s.maybeCompact()
			if err != nil {
				if !errors.Is(err, errClosing) {
					s.fail(err)
				}
				return
			}
			if !did {
				break
			}
		}
	}
}

func (s *Store) maybeCompact() (bool, error) {
	s.cmu.Lock()
	rs, ok := s.acquire()
	if !ok {
		s.cmu.Unlock()
		return false, nil
	}
	defer rs.v.unref()
	c := s.pick(rs.v)
	if c == nil {
		s.cmu.Unlock()
		return false, nil
	}
	s.busy[c.level], s.busy[c.out()] = true, true
	s.cmu.Unlock()
	err := s.compact(c)
	s.cmu.Lock()
	s.busy[c.level], s.busy[c.out()] = false, false
	s.cmuCond.Broadcast()
	s.cmu.Unlock()
	notify(s.compactCh) // another compactor may find work its levels blocked
	return true, err
}

// waitPublished waits until every batch up to seq is durable and
// visible.
func (s *Store) waitPublished(seq uint64) error {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	for s.published.Load() < seq && s.failed == nil && !s.quitting() {
		s.pubCond.Wait()
	}
	if s.failed != nil {
		return s.failed
	}
	if s.published.Load() < seq {
		return errClosing
	}
	return nil
}

// rotateMemtable hands the memtable to the flusher and starts a new one,
// with a new log segment. The caller holds wmu. It waits while too much
// is waiting to flush or compact (a write stall).
func (s *Store) rotateMemtable() error {
	s.stateMu.Lock()
	var stalled time.Time
	if len(s.imms) >= s.o.MaxImmutable || len(s.cur.levels[0]) >= s.o.L0StopWrites {
		stalled = time.Now()
		s.stalls.Add(1)
	}
	for len(s.imms) >= s.o.MaxImmutable || len(s.cur.levels[0]) >= s.o.L0StopWrites {
		if s.closed.Load() {
			s.stateMu.Unlock()
			return storage.ErrClosed
		}
		if err := s.failure(); err != nil {
			s.stateMu.Unlock()
			return err
		}
		s.stallCond.Wait()
	}
	s.stateMu.Unlock()
	if !stalled.IsZero() {
		s.stallNanos.Add(int64(time.Since(stalled)))
	}
	seg, err := s.log.Rotate()
	if err != nil {
		s.fail(err)
		return err
	}
	imm := s.mem
	imm.maxSeq, imm.logSeg = s.next-1, seg
	s.stateMu.Lock()
	s.imms = append(s.imms, imm)
	s.mem = newMemtable()
	s.stateMu.Unlock()
	notify(s.flushCh)
	return nil
}

// Prepare writes a batch to the log and the memtable; commit waits until
// it is durable and makes it visible (storage.Preparer).
func (s *Store) Prepare(b *storage.Batch) (uint64, func() error, error) {
	points, ranges := normalize(b)
	s.wmu.Lock()
	if s.closed.Load() {
		s.wmu.Unlock()
		return 0, nil, storage.ErrClosed
	}
	if err := s.failure(); err != nil {
		s.wmu.Unlock()
		return 0, nil, err
	}
	if s.mem.size.Load() >= s.o.MemtableSize {
		if err := s.rotateMemtable(); err != nil {
			s.wmu.Unlock()
			return 0, nil, err
		}
	}
	seq := s.next
	wait, err := s.log.Write(encodeRecord(seq, points, ranges))
	if err != nil {
		s.fail(err)
		s.wmu.Unlock()
		return 0, nil, err
	}
	s.next++
	// Entries newer than the published sequence number are invisible, so
	// they can go in now.
	s.mem.add(seq, points, ranges)
	s.wmu.Unlock()
	commit := func() error {
		if err := wait(); err != nil {
			s.fail(err)
			return err
		}
		return s.publish(seq)
	}
	return seq, commit, nil
}

// publish makes seq visible after every earlier batch.
func (s *Store) publish(seq uint64) error {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	for s.published.Load() != seq-1 && s.failed == nil {
		s.pubCond.Wait()
	}
	if s.failed != nil {
		return s.failed
	}
	s.published.Store(seq)
	s.pubCond.Broadcast()
	return nil
}

func (s *Store) fail(err error) {
	s.pubMu.Lock()
	if s.failed == nil {
		s.failed = fmt.Errorf("lsm: a write failed, so the store takes no more writes: %w", err)
	}
	s.pubCond.Broadcast()
	s.pubMu.Unlock()
	s.stateMu.Lock()
	s.stallCond.Broadcast()
	s.stateMu.Unlock()
}

func (s *Store) failure() error {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	return s.failed
}

// Apply writes a batch and waits until it is durable and visible.
func (s *Store) Apply(b *storage.Batch) (uint64, error) {
	seq, commit, err := s.Prepare(b)
	if err != nil {
		return 0, err
	}
	return seq, commit()
}

// Flush writes the memtable to a table and waits until every memtable is
// flushed.
func (s *Store) Flush() error {
	s.wmu.Lock()
	if s.closed.Load() {
		s.wmu.Unlock()
		return storage.ErrClosed
	}
	if s.mem.size.Load() > 0 {
		if err := s.rotateMemtable(); err != nil {
			s.wmu.Unlock()
			return err
		}
	}
	s.wmu.Unlock()
	s.stateMu.Lock()
	for len(s.imms) > 0 && !s.closed.Load() && s.failure() == nil {
		s.stallCond.Wait()
	}
	s.stateMu.Unlock()
	return s.failure()
}

// CompactAll flushes, then compacts every level into the next, so that
// all data ends up in the last level with its garbage collected.
func (s *Store) CompactAll() error {
	if err := s.Flush(); err != nil {
		return err
	}
	// Wait for running compactions, and keep new ones out.
	s.cmu.Lock()
	for slices.Contains(s.busy[:], true) {
		s.cmuCond.Wait()
	}
	for l := range s.busy {
		s.busy[l] = true
	}
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		clear(s.busy[:])
		s.cmuCond.Broadcast()
		s.cmu.Unlock()
		notify(s.compactCh)
	}()
	for l := range numLevels {
		rs, ok := s.acquire()
		if !ok {
			return storage.ErrClosed
		}
		if len(rs.v.levels[l]) > 0 {
			c := &compaction{level: l, v: rs.v, force: true}
			c.inputs[0] = rs.v.levels[l]
			if l < numLevels-1 {
				c.inputs[1] = rs.v.overlapping(l+1, spanOf(c.inputs[0]))
			}
			if err := s.compact(c); err != nil {
				rs.v.unref()
				return err
			}
		}
		rs.v.unref()
	}
	return nil
}

// LevelMetrics describes one level.
type LevelMetrics struct {
	Tables int
	Bytes  uint64
}

// Metrics describes the store's shape, and its work since it opened.
type Metrics struct {
	Levels        [numLevels]LevelMetrics
	Memtables     int // including the active one
	MemtableBytes int64

	Stalls       int64         // writes that waited for flushes or compactions
	StallTime    time.Duration // how long they waited in all
	FlushBytes   uint64        // table bytes written by flushes
	CompactBytes uint64        // table bytes written by compactions
	Compactions  int64
}

// Metrics returns the store's current shape.
func (s *Store) Metrics() Metrics {
	m := Metrics{
		Stalls: s.stalls.Load(), StallTime: time.Duration(s.stallNanos.Load()),
		FlushBytes: s.flushBytes.Load(), CompactBytes: s.compactBytes.Load(), Compactions: s.compactions.Load(),
	}
	rs, ok := s.acquire()
	if !ok {
		return m
	}
	defer rs.v.unref()
	for l, level := range rs.v.levels {
		m.Levels[l] = LevelMetrics{len(level), rs.v.levelSize(l)}
	}
	for _, mt := range append([]*memtable{rs.mem}, rs.imms...) {
		m.Memtables++
		m.MemtableBytes += mt.size.Load()
	}
	return m
}

// Seq returns the sequence number of the last visible batch.
func (s *Store) Seq() uint64 { return s.published.Load() }

// Get reads the latest visible value of key.
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	if s.closed.Load() {
		return nil, false, storage.ErrClosed
	}
	return s.get(key, s.published.Load())
}

// Iter iterates over the latest visible data.
func (s *Store) Iter(opts storage.IterOptions) storage.Iterator {
	if s.closed.Load() {
		return errIter{storage.ErrClosed}
	}
	return s.iter(opts, s.published.Load())
}

// readState is what a read uses: memtables and a referenced version.
type readState struct {
	mem  *memtable
	imms []*memtable // oldest first
	v    *version
}

func (s *Store) acquire() (readState, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.cur == nil {
		return readState{}, false
	}
	rs := readState{s.mem, s.imms, s.cur}
	rs.v.ref()
	return rs, true
}

func (rs readState) tombstones() []tombstone {
	ts := slices.Clone(rs.v.tombstones)
	ts = append(ts, rs.mem.rangeDels()...)
	for _, m := range rs.imms {
		ts = append(ts, m.rangeDels()...)
	}
	return ts
}

func (s *Store) get(key []byte, snap uint64) ([]byte, bool, error) {
	rs, ok := s.acquire()
	if !ok {
		return nil, false, storage.ErrClosed
	}
	defer rs.v.unref()
	// Sources from newest to oldest: the first version at or below snap
	// is the newest.
	var (
		val   []byte
		seq   uint64
		kind  uint8
		found bool
	)
	mems := []*memtable{rs.mem}
	for i := len(rs.imms) - 1; i >= 0; i-- {
		mems = append(mems, rs.imms[i])
	}
	for _, m := range mems {
		if n, ok := m.get(key, snap); ok {
			val, seq, kind, found = n.value, n.seq, n.kind, true
			break
		}
	}
	try := func(t *tableMeta) error {
		v, sq, k, ok, err := t.r.Get(key, snap)
		if ok {
			val, seq, kind, found = v, sq, k, true
		}
		return err
	}
	for _, t := range rs.v.levels[0] {
		if found {
			break
		}
		if t.b.contains(key) {
			if err := try(t); err != nil {
				return nil, false, err
			}
		}
	}
	for l := 1; l < numLevels && !found; l++ {
		files := rs.v.levels[l]
		i := sort.Search(len(files), func(i int) bool { return files[i].b.beforeEnd(key) })
		if i < len(files) && files[i].b.contains(key) {
			if err := try(files[i]); err != nil {
				return nil, false, err
			}
		}
	}
	if !found || kind == kindDelete {
		return nil, false, nil
	}
	for _, t := range rs.tombstones() {
		if t.covers(key, seq, snap) {
			return nil, false, nil
		}
	}
	return bytes.Clone(val), true, nil
}

func (s *Store) iter(opts storage.IterOptions, snap uint64) storage.Iterator {
	rs, ok := s.acquire()
	if !ok {
		return errIter{storage.ErrClosed}
	}
	iters := []internalIter{rs.mem.iter()}
	for _, m := range rs.imms {
		iters = append(iters, m.iter())
	}
	for _, t := range rs.v.levels[0] {
		iters = append(iters, t.r.NewIter())
	}
	for l := 1; l < numLevels; l++ {
		if len(rs.v.levels[l]) > 0 {
			iters = append(iters, newLevelIter(rs.v.levels[l]))
		}
	}
	return &userIter{m: newMergeIter(iters, opts.Reverse), snap: snap, lower: opts.Lower, upper: opts.Upper,
		reverse: opts.Reverse, tombstones: rs.tombstones(), release: rs.v.unref}
}

// Snapshot returns a consistent view of the current data.
func (s *Store) Snapshot() storage.Snapshot {
	s.snapMu.Lock()
	seq := s.published.Load()
	s.snaps[seq]++
	s.snapMu.Unlock()
	return &snapshot{s: s, seq: seq}
}

// snapshotList returns the live snapshots' sequence numbers, ascending.
// Compaction keeps, for each key, the newest version each one sees.
func (s *Store) snapshotList() []uint64 {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	list := make([]uint64, 0, len(s.snaps))
	for seq := range s.snaps {
		list = append(list, seq)
	}
	slices.Sort(list)
	return list
}

// Close closes the store. Every acknowledged batch is already durable;
// background work stops, and memtables are not flushed (the log has
// them).
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.quit)
	s.pubMu.Lock()
	s.pubCond.Broadcast()
	s.pubMu.Unlock()
	s.stateMu.Lock()
	s.stallCond.Broadcast()
	s.stateMu.Unlock()
	s.wmu.Lock() // let a write in progress finish
	defer s.wmu.Unlock()
	s.wg.Wait()
	err := s.log.Close()
	if merr := s.manifest.Close(); err == nil {
		err = merr
	}
	s.stateMu.Lock()
	v := s.cur
	s.cur = nil
	s.stateMu.Unlock()
	v.unref() // tables close once open iterators are done with them
	if lerr := s.lock.Close(); err == nil {
		err = lerr
	}
	return err
}

type snapshot struct {
	s        *Store
	seq      uint64
	released atomic.Bool
}

func (sn *snapshot) check() error {
	switch {
	case sn.released.Load():
		return storage.ErrReleased
	case sn.s.closed.Load():
		return storage.ErrClosed
	}
	return nil
}

func (sn *snapshot) Get(key []byte) ([]byte, bool, error) {
	if err := sn.check(); err != nil {
		return nil, false, err
	}
	return sn.s.get(key, sn.seq)
}

func (sn *snapshot) Iter(opts storage.IterOptions) storage.Iterator {
	if err := sn.check(); err != nil {
		return errIter{err}
	}
	return sn.s.iter(opts, sn.seq)
}

func (sn *snapshot) Seq() uint64 { return sn.seq }

func (sn *snapshot) Release() {
	if sn.released.Swap(true) {
		return
	}
	sn.s.snapMu.Lock()
	if sn.s.snaps[sn.seq]--; sn.s.snaps[sn.seq] <= 0 {
		delete(sn.s.snaps, sn.seq)
	}
	sn.s.snapMu.Unlock()
}
