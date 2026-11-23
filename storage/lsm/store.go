package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/storage/wal"
)

// Options configure a store.
type Options struct {
	FS  vfs.FS // default vfs.OS
	Dir string
	// Durability: durable (default) or relaxed commits (wal.Durability).
	Durability wal.Durability
	// LogSegmentSize is the write-ahead log's segment size.
	LogSegmentSize int64
}

// Store is an LSM store. It is safe for concurrent use.
type Store struct {
	o    Options
	lock io.Closer
	log  *wal.Log

	// wmu orders writes: it assigns sequence numbers, writes the log and
	// fills the memtable in the same order.
	wmu  sync.Mutex
	next uint64
	mem  *memtable

	// Batches become visible in sequence order, once durable.
	pubMu     sync.Mutex
	pubCond   *sync.Cond
	published atomic.Uint64
	failed    error // a write failed: the store takes no more writes

	snapMu sync.Mutex
	snaps  map[uint64]int // live snapshots by sequence number

	closed atomic.Bool
}

var _ storage.Store = (*Store)(nil)
var _ storage.Preparer = (*Store)(nil)

// Open opens the store in o.Dir, creating it if needed, and recovers
// everything the log holds.
func Open(o Options) (*Store, error) {
	if o.FS == nil {
		o.FS = vfs.OS
	}
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
	s := &Store{o: o, lock: lock, mem: newMemtable(), snaps: map[uint64]int{}}
	s.pubCond = sync.NewCond(&s.pubMu)
	var last uint64
	replay := func(rec []byte) error {
		seq, points, ranges, err := decodeRecord(rec)
		if err != nil {
			return err
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
		lock.Close()
		return nil, err
	}
	s.next = last + 1
	s.published.Store(last)
	return s, nil
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
	return s.iter(opts, s.published.Load(), nil)
}

func (s *Store) get(key []byte, snap uint64) ([]byte, bool, error) {
	n, ok := s.mem.get(key, snap)
	if !ok || n.kind == kindDelete {
		return nil, false, nil
	}
	for _, t := range s.mem.rangeDels() {
		if t.covers(key, n.seq, snap) {
			return nil, false, nil
		}
	}
	return bytes.Clone(n.value), true, nil
}

func (s *Store) iter(opts storage.IterOptions, snap uint64, release func()) storage.Iterator {
	m := newMergeIter([]internalIter{s.mem.iter()}, opts.Reverse)
	return &userIter{m: m, snap: snap, lower: opts.Lower, upper: opts.Upper, reverse: opts.Reverse,
		tombstones: s.mem.rangeDels(), release: release}
}

// Snapshot returns a consistent view of the current data.
func (s *Store) Snapshot() storage.Snapshot {
	seq := s.published.Load()
	s.snapMu.Lock()
	s.snaps[seq]++
	s.snapMu.Unlock()
	return &snapshot{s: s, seq: seq}
}

// oldestSnapshot returns the sequence number of the oldest live snapshot,
// or the published one if there are none. Versions a snapshot can still
// see are kept until it is released.
func (s *Store) oldestSnapshot() uint64 {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	oldest := s.published.Load()
	for seq := range s.snaps {
		oldest = min(oldest, seq)
	}
	return oldest
}

// Close closes the store. Every acknowledged batch is already durable.
func (s *Store) Close() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	err := s.log.Close()
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
	return sn.s.iter(opts, sn.seq, nil)
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
