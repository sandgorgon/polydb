// Package wal is the storage engine's write-ahead log: every batch is
// appended here, and made durable, before it is applied, so that a crash
// loses nothing that was acknowledged.
//
// The log is a series of segment files, NNNNNNNNNNNNNNNN.wal, each
// starting with a header (magic and format version) followed by records:
//
//	crc32c (4) | length (4) | type (1) | payload (length bytes)
//
// The checksum (Castagnoli) covers type, length and payload. A record
// never spans segments. On open, the log is replayed in order. A bad
// record at the end of the last segment is a write torn by the crash: the
// tail is cut off and appending continues there. A bad record anywhere
// else is corruption, and opening fails rather than skip data.
//
// Commits are durable by default with group commit: concurrent appends
// share one sync. In relaxed mode appends return after the write, and a
// background sync runs every interval.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandgorgon/polydb/storage/vfs"
)

const (
	magic       = "PLYWAL\x00\x01" // format version 1
	headerSize  = 9
	recordFull  = 1
	maxRecord   = 1 << 30
	suffix      = ".wal"
	DefaultSize = 64 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt means a segment other than the last one has a bad record.
var ErrCorrupt = errors.New("wal: corrupt log")

// Durability is when Append returns.
type Durability int

const (
	// Durable: after the record is synced (group commit).
	Durable Durability = iota
	// Relaxed: after the record is written; a background sync runs every
	// RelaxedInterval, so a power cut can lose the last moments of writes.
	Relaxed
)

// Options configure a log.
type Options struct {
	FS              vfs.FS
	Dir             string
	SegmentSize     int64 // start a new segment past this size (default 64 MiB)
	Durability      Durability
	RelaxedInterval time.Duration // default 100 ms
}

// Log is a write-ahead log. It is safe for concurrent use.
type Log struct {
	o Options

	mu      sync.Mutex // the current segment and positions
	f       vfs.File
	seg     uint64 // current segment number
	size    int64  // bytes in the current segment
	written uint64 // bytes appended since open, across segments
	closed  bool
	// retired are rotated-out segments, already synced, kept open until no
	// sync can be using them.
	retired []vfs.File

	// Lock order: mu, then syncMu. errMu is a leaf: nothing is locked
	// while it is held, so it can be taken under either.
	errMu sync.Mutex
	err   error // a failed write or sync: the log takes no more writes

	syncMu  sync.Mutex
	cond    *sync.Cond
	synced  uint64 // bytes known durable
	syncing bool

	stop chan struct{}
	done chan struct{}
}

func segName(n uint64) string { return fmt.Sprintf("%016d%s", n, suffix) }

// segments lists the segment numbers in dir, in order.
func segments(fs vfs.FS, dir string) ([]uint64, error) {
	names, err := fs.List(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, n := range names {
		if !strings.HasSuffix(n, suffix) {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSuffix(n, suffix), 10, 64)
		if err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// Open opens the log in o.Dir, creating it if needed, and calls replay
// with every record already in it, in order. Appends go after the last
// good record.
func Open(o Options, replay func(rec []byte) error) (*Log, error) {
	if o.FS == nil {
		o.FS = vfs.OS
	}
	if o.SegmentSize <= 0 {
		o.SegmentSize = DefaultSize
	}
	if o.RelaxedInterval <= 0 {
		o.RelaxedInterval = 100 * time.Millisecond
	}
	if err := o.FS.MkdirAll(o.Dir); err != nil {
		return nil, err
	}
	segs, err := segments(o.FS, o.Dir)
	if err != nil {
		return nil, err
	}
	l := &Log{o: o, stop: make(chan struct{}), done: make(chan struct{})}
	l.cond = sync.NewCond(&l.syncMu)
	var end int64 // valid length of the last segment
	for i, n := range segs {
		last := i == len(segs)-1
		valid, err := l.replaySegment(n, last, replay)
		if err != nil {
			return nil, err
		}
		end = valid
	}
	if len(segs) == 0 {
		if err := l.newSegment(1); err != nil {
			return nil, err
		}
	} else {
		n := segs[len(segs)-1]
		f, err := o.FS.OpenAppend(vfs.Join(o.Dir, segName(n)))
		if err != nil {
			return nil, err
		}
		if err := f.Truncate(end); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		l.f, l.seg, l.size = f, n, end
	}
	if o.Durability == Relaxed {
		go l.background()
	} else {
		close(l.done)
	}
	return l, nil
}

// replaySegment replays one segment and returns the length of its good
// part.
func (l *Log) replaySegment(n uint64, last bool, replay func([]byte) error) (int64, error) {
	name := vfs.Join(l.o.Dir, segName(n))
	f, err := l.o.FS.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		return 0, err
	}
	bad := func(off int64, why string) (int64, error) {
		if last {
			return off, nil // a torn tail: cut it off
		}
		return 0, fmt.Errorf("%w: %s at offset %d: %s", ErrCorrupt, segName(n), off, why)
	}
	head := make([]byte, len(magic))
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != magic {
		if last && size < int64(len(magic)) {
			// The crash came while the segment was being created: write
			// its header again; its good part is just the header.
			return int64(len(magic)), l.rewriteHeader(name)
		}
		return 0, fmt.Errorf("%w: %s has no valid header", ErrCorrupt, segName(n))
	}
	off := int64(len(magic))
	hdr := make([]byte, headerSize)
	for off < size {
		if size-off < headerSize {
			return bad(off, "truncated record header")
		}
		if _, err := f.ReadAt(hdr, off); err != nil {
			return 0, err
		}
		sum := binary.BigEndian.Uint32(hdr[0:4])
		length := binary.BigEndian.Uint32(hdr[4:8])
		if hdr[8] != recordFull || length > maxRecord {
			return bad(off, "bad record header")
		}
		if int64(length) > size-off-headerSize {
			return bad(off, "truncated record")
		}
		payload := make([]byte, length)
		if _, err := f.ReadAt(payload, off+headerSize); err != nil && err != io.EOF {
			return 0, err
		}
		crc := crc32.Update(crc32.Checksum(hdr[4:9], castagnoli), castagnoli, payload)
		if crc != sum {
			return bad(off, "checksum mismatch")
		}
		if err := replay(payload); err != nil {
			return 0, err
		}
		off += headerSize + int64(length)
	}
	return off, nil
}

// rewriteHeader recreates a segment whose header never reached the disk.
func (l *Log) rewriteHeader(name string) error {
	f, err := l.o.FS.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(magic)); err != nil {
		return err
	}
	return f.Sync()
}

// newSegment starts segment n and makes its name durable.
func (l *Log) newSegment(n uint64) error {
	f, err := l.o.FS.Create(vfs.Join(l.o.Dir, segName(n)))
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(magic)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := l.o.FS.SyncDir(l.o.Dir); err != nil {
		f.Close()
		return err
	}
	l.f, l.seg, l.size = f, n, int64(len(magic))
	return nil
}

// Append writes a record. In durable mode it returns once the record is
// synced; concurrent appends share syncs.
func (l *Log) Append(rec []byte) error {
	if len(rec) > maxRecord {
		return fmt.Errorf("wal: record of %d bytes is too large", len(rec))
	}
	buf := make([]byte, headerSize+len(rec))
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(rec)))
	buf[8] = recordFull
	copy(buf[headerSize:], rec)
	binary.BigEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], castagnoli))

	l.mu.Lock()
	if err := l.writable(); err != nil {
		l.mu.Unlock()
		return err
	}
	if l.size > int64(len(magic)) && l.size+int64(len(buf)) > l.o.SegmentSize {
		if err := l.rotate(); err != nil {
			l.mu.Unlock()
			return err
		}
	}
	if _, err := l.f.Write(buf); err != nil {
		l.fail(err)
		l.mu.Unlock()
		return err
	}
	l.size += int64(len(buf))
	l.written += uint64(len(buf))
	target := l.written
	l.mu.Unlock()
	if l.o.Durability == Relaxed {
		return nil
	}
	return l.syncTo(target)
}

func (l *Log) writable() error {
	if l.closed {
		return errors.New("wal: log is closed")
	}
	if err := l.failure(); err != nil {
		return fmt.Errorf("wal: an earlier write or sync failed, so the log takes no more writes: %w", err)
	}
	return nil
}

// fail records the first write or sync failure. A failed fsync can't be
// retried safely: the data it covered may be lost, so the log stops.
func (l *Log) fail(err error) {
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.errMu.Unlock()
}

func (l *Log) failure() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

// syncTo waits until the first target bytes are durable. One caller at a
// time syncs (the leader), covering everyone written so far.
func (l *Log) syncTo(target uint64) error {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	for l.synced < target {
		if l.syncing {
			l.cond.Wait()
			continue
		}
		l.syncing = true
		l.syncMu.Unlock()
		l.mu.Lock()
		f, upTo := l.f, l.written
		// Only one leader syncs at a time, so no sync is using the
		// segments rotated out before now.
		for _, r := range l.retired {
			r.Close()
		}
		l.retired = nil
		l.mu.Unlock()
		err := l.failure()
		if err == nil {
			err = f.Sync()
		}
		l.syncMu.Lock()
		l.syncing = false
		if err != nil {
			l.fail(err)
			l.cond.Broadcast()
			return err
		}
		if upTo > l.synced {
			l.synced = upTo
		}
		l.cond.Broadcast()
	}
	return l.failure()
}

// rotate syncs the current segment and starts the next. The caller holds
// l.mu. It doesn't wait for a sync in progress: a leader may still be
// syncing the old segment, so it stays open (retired) until the next
// leader closes it.
func (l *Log) rotate() error {
	old := l.f
	err := old.Sync()
	if err == nil {
		err = l.newSegment(l.seg + 1)
	}
	if err != nil {
		l.fail(err)
		return err
	}
	l.retired = append(l.retired, old)
	l.syncMu.Lock()
	if l.written > l.synced {
		l.synced = l.written // everything before the new segment is durable
	}
	l.cond.Broadcast()
	l.syncMu.Unlock()
	return nil
}

// Rotate starts a new segment and returns its number. Records appended
// before it are in earlier segments, which RemoveBefore can delete once
// their contents are stored elsewhere.
func (l *Log) Rotate() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.writable(); err != nil {
		return 0, err
	}
	if err := l.rotate(); err != nil {
		return 0, err
	}
	return l.seg, nil
}

// Segment returns the current segment's number.
func (l *Log) Segment() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seg
}

// RemoveBefore deletes the segments numbered below n.
func (l *Log) RemoveBefore(n uint64) error {
	segs, err := segments(l.o.FS, l.o.Dir)
	if err != nil {
		return err
	}
	l.mu.Lock()
	cur := l.seg
	l.mu.Unlock()
	removed := false
	for _, s := range segs {
		if s < n && s < cur {
			if err := l.o.FS.Remove(vfs.Join(l.o.Dir, segName(s))); err != nil {
				return err
			}
			removed = true
		}
	}
	if removed {
		return l.o.FS.SyncDir(l.o.Dir)
	}
	return nil
}

// Sync makes every appended record durable.
func (l *Log) Sync() error {
	l.mu.Lock()
	target := l.written
	l.mu.Unlock()
	return l.syncTo(target)
}

// background syncs periodically in relaxed mode.
func (l *Log) background() {
	defer close(l.done)
	t := time.NewTicker(l.o.RelaxedInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.Sync()
		case <-l.stop:
			return
		}
	}
}

// Close syncs and closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	close(l.stop)
	<-l.done
	err := l.Sync()
	l.mu.Lock()
	l.closed = true
	for _, r := range l.retired {
		r.Close()
	}
	l.retired = nil
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.mu.Unlock()
	return err
}
