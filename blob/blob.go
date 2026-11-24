// Package blob keeps the chunks of a database's large values in
// append-only segment files (spec/types.md §13).
//
// A chunk is up to a few MiB of content, named by its SHA-256 hash. The
// store only appends chunks and reads them back by location; which chunks
// are in use, and where each one is, is recorded by the engine in the
// database's key-value store, in the transaction that first references
// it. Chunks are written and synced before that transaction commits, so a
// committed reference always finds its data. A crash before the commit
// leaves unreferenced data at the end of a segment, which a later rewrite
// reclaims.
//
// Segments are files named by number in the store's directory:
//
//	00000001.seg   "PDBBLOB1", then entries:
//	                 4 bytes length n, 32 bytes hash, n bytes data,
//	                 4 bytes CRC-32C of the hash and data
//
// One segment, the active one, takes new chunks until it reaches the
// segment size; then it is synced and sealed and a new one starts. A
// store reopened after a restart seals what it finds and starts afresh.
//
// The engine moves the live chunks out of sealed segments that are mostly
// garbage, then retires them. A retired segment is deleted once nothing
// can still read it: no transaction whose snapshot predates the move, and
// no pin (a download lease holding locations).
package blob

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandgorgon/polydb/storage/vfs"
)

// ChunkSize is the size content is split into.
const ChunkSize = 1 << 20

// DefaultSegmentSize is the default size at which a segment is sealed.
const DefaultSegmentSize = 256 << 20

const (
	magic     = "PDBBLOB1"
	entryHead = 4 + 32
	entryTail = 4
	maxChunk  = 16 << 20
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Hash is a chunk's SHA-256.
type Hash = [32]byte

// Loc is where a chunk is: its segment, the offset of its entry and the
// length of its data.
type Loc struct {
	Seg uint32
	Off int64
	Len uint32
}

// Options configure a Store.
type Options struct {
	FS  vfs.FS
	Dir string
	// SegmentSize is the size at which the active segment is sealed
	// (default DefaultSegmentSize).
	SegmentSize int64
	// NoSync skips fsync, for stores that don't outlive the process.
	NoSync bool
}

// Segment describes a sealed segment.
type Segment struct {
	Num  uint32
	Size int64
}

type segment struct {
	num  uint32
	f    vfs.File
	size atomic.Int64 // bytes written (changed on Store.mu)

	syncMu sync.Mutex
	synced int64 // bytes known durable (on syncMu)
}

type pending struct {
	loc   Loc
	until time.Time
}

type retired struct {
	after uint64 // removable once every snapshot is at least this
}

// Store is a directory of segments. It is safe for concurrent use.
type Store struct {
	opts Options

	mu      sync.Mutex
	active  *segment
	next    uint32
	segs    map[uint32]*segment // every segment, active included
	pins    map[uint32]int
	retired map[uint32]retired
	pending map[Hash]pending
	closed  bool
}

// Open opens the store in opts.Dir, creating the directory if needed.
func Open(opts Options) (*Store, error) {
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if err := opts.FS.MkdirAll(opts.Dir); err != nil {
		return nil, err
	}
	s := &Store{opts: opts, next: 1, segs: map[uint32]*segment{}, pins: map[uint32]int{},
		retired: map[uint32]retired{}, pending: map[Hash]pending{}}
	names, err := opts.FS.List(opts.Dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		num, ok := parseName(name)
		if !ok {
			continue
		}
		f, err := opts.FS.Open(vfs.Join(opts.Dir, name))
		if err != nil {
			s.Close()
			return nil, err
		}
		size, err := f.Size()
		if err != nil {
			f.Close()
			s.Close()
			return nil, err
		}
		seg := &segment{num: num, f: f, synced: size}
		seg.size.Store(size)
		s.segs[num] = seg
		s.next = max(s.next, num+1)
	}
	return s, nil
}

func segName(num uint32) string { return fmt.Sprintf("%08d.seg", num) }

func parseName(name string) (uint32, bool) {
	base, ok := strings.CutSuffix(name, ".seg")
	if !ok || len(base) != 8 {
		return 0, false
	}
	n, err := strconv.ParseUint(base, 10, 32)
	return uint32(n), err == nil && n > 0
}

// Close closes the store's files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var first error
	for _, seg := range s.segs {
		if err := seg.f.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.segs = map[uint32]*segment{}
	s.active = nil
	return first
}

var errClosed = errors.New("blob: store is closed")

// Put appends a chunk and returns its location. It isn't durable until
// Sync.
func (s *Store) Put(h Hash, data []byte) (Loc, error) {
	if len(data) > maxChunk {
		return Loc{}, fmt.Errorf("blob: chunk of %d bytes", len(data))
	}
	entry := make([]byte, entryHead, entryHead+len(data)+entryTail)
	binary.BigEndian.PutUint32(entry, uint32(len(data)))
	copy(entry[4:], h[:])
	entry = append(entry, data...)
	entry = binary.BigEndian.AppendUint32(entry, crc32.Checksum(entry[4:], crcTable))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Loc{}, errClosed
	}
	if a := s.active; a == nil || a.size.Load()+int64(len(entry)) > s.opts.SegmentSize && a.size.Load() > int64(len(magic)) {
		if err := s.rotate(); err != nil {
			return Loc{}, err
		}
	}
	seg := s.active
	if _, err := seg.f.Write(entry); err != nil {
		// The file's end is now unknown: start a new segment next time.
		s.active = nil
		return Loc{}, err
	}
	loc := Loc{Seg: seg.num, Off: seg.size.Load(), Len: uint32(len(data))}
	seg.size.Add(int64(len(entry)))
	return loc, nil
}

// rotate seals the active segment, syncing it, and starts a new one.
// s.mu is held.
func (s *Store) rotate() error {
	if old := s.active; old != nil {
		if err := s.syncSeg(old, old.size.Load()); err != nil {
			return err
		}
		s.active = nil
	}
	num := s.next
	name := vfs.Join(s.opts.Dir, segName(num))
	f, err := s.opts.FS.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(magic)); err != nil {
		f.Close()
		return err
	}
	if !s.opts.NoSync {
		if err := s.opts.FS.SyncDir(s.opts.Dir); err != nil {
			f.Close()
			return err
		}
	}
	s.next++
	seg := &segment{num: num, f: f}
	seg.size.Store(int64(len(magic)))
	s.segs[num] = seg
	s.active = seg
	return nil
}

// Sync makes every chunk up to and including loc durable. Concurrent
// calls share one fsync where they can.
func (s *Store) Sync(loc Loc) error {
	s.mu.Lock()
	seg, ok := s.segs[loc.Seg]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("blob: no segment %d", loc.Seg)
	}
	return s.syncSeg(seg, loc.Off+entryHead+int64(loc.Len)+entryTail)
}

// syncSeg syncs seg up to at least end. It may be called with s.mu held
// (when sealing), so it doesn't take it.
func (s *Store) syncSeg(seg *segment, end int64) error {
	if s.opts.NoSync {
		return nil
	}
	seg.syncMu.Lock()
	defer seg.syncMu.Unlock()
	if seg.synced >= end {
		return nil
	}
	size := seg.size.Load()
	if err := seg.f.Sync(); err != nil {
		return err
	}
	seg.synced = size
	return nil
}

// Get reads the chunk at loc, checking it is h and undamaged.
func (s *Store) Get(h Hash, loc Loc) ([]byte, error) {
	s.mu.Lock()
	seg, ok := s.segs[loc.Seg]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("blob: no segment %d for chunk %x", loc.Seg, h[:8])
	}
	buf := make([]byte, entryHead+int(loc.Len)+entryTail)
	if _, err := seg.f.ReadAt(buf, loc.Off); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("blob: reading chunk %x: %w", h[:8], err)
	}
	n := len(buf) - entryTail
	if binary.BigEndian.Uint32(buf) != loc.Len || !bytes.Equal(buf[4:entryHead], h[:]) ||
		binary.BigEndian.Uint32(buf[n:]) != crc32.Checksum(buf[4:n], crcTable) {
		return nil, fmt.Errorf("blob: chunk %x in segment %d at %d is damaged", h[:8], loc.Seg, loc.Off)
	}
	return buf[entryHead:n], nil
}

// Sealed returns the sealed segments that may be rewritten: not the
// active one, not retired, and holding no pending upload.
func (s *Store) Sealed() []Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	busy := map[uint32]bool{}
	now := time.Now()
	for h, p := range s.pending {
		if now.After(p.until) {
			delete(s.pending, h)
			continue
		}
		busy[p.loc.Seg] = true
	}
	var out []Segment
	for num, seg := range s.segs {
		if seg == s.active || busy[num] {
			continue
		}
		if _, ok := s.retired[num]; ok {
			continue
		}
		out = append(out, Segment{num, seg.size.Load()})
	}
	slices.SortFunc(out, func(a, b Segment) int { return int(a.Num) - int(b.Num) })
	return out
}

// Size returns the total size of the segments.
func (s *Store) Size() (total int64, segments int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seg := range s.segs {
		total += seg.size.Load()
	}
	return total, len(s.segs)
}

// Retire marks a sealed segment for deletion once every snapshot is at
// least after and nothing pins it. Its chunks must have moved.
func (s *Store) Retire(num uint32, after uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seg, ok := s.segs[num]; ok && seg != s.active {
		s.retired[num] = retired{after}
	}
}

// Remove deletes the retired segments that nothing can read any more:
// oldest is the oldest snapshot still open. It returns how many it
// removed.
func (s *Store) Remove(oldest uint64) (int, error) {
	s.mu.Lock()
	var gone []*segment
	for num, r := range s.retired {
		if r.after > oldest || s.pins[num] > 0 {
			continue
		}
		gone = append(gone, s.segs[num])
		delete(s.segs, num)
		delete(s.retired, num)
	}
	s.mu.Unlock()
	for _, seg := range gone {
		seg.f.Close()
		if err := s.opts.FS.Remove(vfs.Join(s.opts.Dir, segName(seg.num))); err != nil {
			return 0, err
		}
	}
	if len(gone) > 0 && !s.opts.NoSync {
		if err := s.opts.FS.SyncDir(s.opts.Dir); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}

// PinnedRetired reports whether a retired segment that Remove(oldest)
// could delete is held only by pins.
func (s *Store) PinnedRetired(oldest uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for num, r := range s.retired {
		if r.after <= oldest && s.pins[num] > 0 {
			return true
		}
	}
	return false
}

// Pin keeps the segments holding locs from being removed until unpin is
// called. The caller must hold a snapshot that can read locs while it
// pins them.
func (s *Store) Pin(locs []Loc) (unpin func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nums := map[uint32]bool{}
	for _, l := range locs {
		nums[l.Seg] = true
	}
	for n := range nums {
		s.pins[n]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for n := range nums {
				if s.pins[n]--; s.pins[n] <= 0 {
					delete(s.pins, n)
				}
			}
		})
	}
}

// AddPending records an uploaded chunk that no committed record
// references yet, so that a write of the same content within ttl uses it
// instead of storing it again, and its segment isn't rewritten meanwhile.
func (s *Store) AddPending(h Hash, loc Loc, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[h] = pending{loc, time.Now().Add(ttl)}
}

// Pending returns the location of an uploaded chunk, if there is one.
func (s *Store) Pending(h Hash) (Loc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[h]
	if !ok || time.Now().After(p.until) {
		return Loc{}, false
	}
	return p.loc, true
}
