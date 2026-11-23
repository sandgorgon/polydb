package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sync"
	"sync/atomic"

	"github.com/sandgorgon/polydb/storage/vfs"
)

// ReaderOptions configure a Reader.
type ReaderOptions struct {
	// ID identifies the table in the block cache; unique per table.
	ID uint64
	// Cache holds blocks read without memory mapping; nil means none.
	Cache *Cache
	// NoMmap reads with ReadAt even when the file can be mapped.
	NoMmap bool
}

// Reader reads a table. It is safe for concurrent use. Values and keys
// it returns stay valid until the reader and its iterators are closed.
type Reader struct {
	f     vfs.File
	o     ReaderOptions
	size  uint64
	data  []byte // the mapping, if mapped
	unmap func() error

	verified sync.Map // offsets of mapped blocks whose checksum is good

	index      []byte
	filter     []byte
	tombstones []Tombstone
	props      Props

	refs atomic.Int64
}

// Open opens a table. The reader owns f and closes it when the reader and
// all its iterators are closed.
func Open(f vfs.File, o ReaderOptions) (_ *Reader, err error) {
	r := &Reader{f: f, o: o}
	r.refs.Store(1)
	defer func() {
		if err != nil {
			r.release() // closes f
		}
	}()
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	if size < footerSize {
		return nil, corrupt("file of %d bytes is too short", size)
	}
	r.size = uint64(size)
	if m, ok := f.(vfs.Mapper); ok && !o.NoMmap {
		if r.data, r.unmap, err = m.Map(); err != nil {
			return nil, err
		}
	}
	foot := make([]byte, footerSize)
	if err := r.readAt(foot, r.size-footerSize); err != nil {
		return nil, err
	}
	if !bytes.Equal(foot[72:], magic) {
		return nil, corrupt("bad magic number")
	}
	if crc32.Checksum(foot[:68], castagnoli) != binary.LittleEndian.Uint32(foot[68:]) {
		return nil, corrupt("bad footer checksum")
	}
	if v := binary.LittleEndian.Uint32(foot[64:]); v != version {
		return nil, corrupt("format version %d is not supported", v)
	}
	var hs [4]handle
	for i := range hs {
		hs[i] = handle{binary.LittleEndian.Uint64(foot[16*i:]), binary.LittleEndian.Uint64(foot[16*i+8:])}
	}
	var meta [4][]byte
	for i, h := range hs {
		if meta[i], err = r.readBlock(h, false); err != nil {
			return nil, err
		}
	}
	r.filter, r.index = meta[0], meta[3]
	if r.tombstones, err = decodeTombstones(meta[1]); err != nil {
		return nil, err
	}
	if r.props, err = decodeProps(meta[2]); err != nil {
		return nil, err
	}
	r.props.Size = r.size
	var it blockIter
	if err := it.init(r.index); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) readAt(b []byte, off uint64) error {
	if r.data != nil {
		copy(b, r.data[off:])
		return nil
	}
	_, err := r.f.ReadAt(b, int64(off))
	return err
}

// readBlock returns a block's contents after checking its checksum.
// Mapped blocks are slices of the mapping, checked once; others are read
// into memory, through the cache if useCache.
func (r *Reader) readBlock(h handle, useCache bool) ([]byte, error) {
	limit := r.size - footerSize // blocks end before the footer
	if h.n > limit || h.off > limit-h.n || limit-h.n-h.off < blockTrailer {
		return nil, corrupt("block at %d of %d bytes is out of bounds", h.off, h.n)
	}
	if r.data != nil {
		b := r.data[h.off : h.off+h.n+blockTrailer]
		if _, ok := r.verified.Load(h.off); !ok {
			if err := check(b, h); err != nil {
				return nil, err
			}
			r.verified.Store(h.off, struct{}{})
		}
		return b[:h.n:h.n], nil
	}
	if useCache && r.o.Cache != nil {
		if b, ok := r.o.Cache.get(r.o.ID, h.off); ok {
			return b, nil
		}
	}
	b := make([]byte, h.n+blockTrailer)
	if _, err := r.f.ReadAt(b, int64(h.off)); err != nil {
		return nil, err
	}
	if err := check(b, h); err != nil {
		return nil, err
	}
	b = b[:h.n:h.n]
	if useCache && r.o.Cache != nil {
		r.o.Cache.put(r.o.ID, h.off, b)
	}
	return b, nil
}

func check(b []byte, h handle) error {
	n := len(b) - blockTrailer
	if b[n] != compNone {
		return corrupt("block at %d: unknown compression type %d", h.off, b[n])
	}
	if crc32.Checksum(b[:n+1], castagnoli) != binary.LittleEndian.Uint32(b[n+1:]) {
		return corrupt("block at %d: checksum mismatch", h.off)
	}
	return nil
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err, d.b = ErrCorrupt, nil
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) str() []byte {
	n := d.uvarint()
	if n > uint64(len(d.b)) {
		d.err, d.b = ErrCorrupt, nil
		return nil
	}
	s := bytes.Clone(d.b[:n])
	d.b = d.b[n:]
	return s
}

func decodeTombstones(b []byte) ([]Tombstone, error) {
	d := &decoder{b: b}
	n := d.uvarint()
	if n > uint64(len(d.b)) {
		return nil, corrupt("bad tombstone count")
	}
	ts := make([]Tombstone, 0, n)
	for range n {
		t := Tombstone{Start: d.str()}
		if len(d.b) == 0 {
			return nil, corrupt("truncated tombstone")
		}
		flag := d.b[0]
		d.b = d.b[1:]
		switch flag {
		case 0:
		case 1:
			t.End = d.str()
		default:
			return nil, corrupt("bad tombstone")
		}
		t.Seq = d.uvarint()
		if d.err != nil {
			return nil, corrupt("bad tombstone")
		}
		ts = append(ts, t)
	}
	if len(d.b) != 0 {
		return nil, corrupt("trailing tombstone data")
	}
	return ts, nil
}

func decodeProps(b []byte) (Props, error) {
	d := &decoder{b: b}
	var p Props
	for _, v := range []*uint64{&p.Entries, &p.Deletions, &p.Tombstones, &p.DataBlocks, &p.MinSeq, &p.MaxSeq} {
		*v = d.uvarint()
	}
	p.Smallest, p.Largest = d.str(), d.str()
	if d.err != nil || len(d.b) != 0 {
		return Props{}, corrupt("bad properties")
	}
	if p.Entries == 0 {
		p.Smallest, p.Largest = nil, nil
	}
	return p, nil
}

// Props returns the table's properties.
func (r *Reader) Props() Props { return r.props }

// Tombstones returns the table's range tombstones, sorted by start key.
// The caller must not modify them.
func (r *Reader) Tombstones() []Tombstone { return r.tombstones }

// MayContain reports whether the table may hold key; false is certain.
func (r *Reader) MayContain(key []byte) bool { return mayContain(r.filter, key) }

// Get returns the newest entry for key at or below snap. Range
// tombstones are not applied; that is the caller's job.
func (r *Reader) Get(key []byte, snap uint64) (value []byte, seq uint64, kind uint8, ok bool, err error) {
	if !r.MayContain(key) {
		return nil, 0, 0, false, nil
	}
	it := r.NewIter()
	defer it.Close()
	it.seekGE(key, min(snap, MaxSeq))
	if !it.Valid() {
		return nil, 0, 0, false, it.Err()
	}
	if !bytes.Equal(it.Key(), key) {
		return nil, 0, 0, false, nil
	}
	return it.Value(), it.Seq(), it.Kind(), true, nil
}

// Close releases the reader; the file is closed once its iterators are
// closed too.
func (r *Reader) Close() error {
	r.release()
	return nil
}

func (r *Reader) release() {
	if r.refs.Add(-1) != 0 {
		return
	}
	if r.unmap != nil {
		r.unmap()
		r.data = nil
	}
	r.f.Close()
}

// NewIter returns an iterator over the table's entries. It has the
// methods of the LSM store's internal iterators.
func (r *Reader) NewIter() *Iter {
	r.refs.Add(1)
	it := &Iter{r: r}
	if err := it.index.init(r.index); err != nil {
		it.err = err
	}
	return it
}

// Iter iterates over a table's entries in internal key order. Key and
// Value are valid until the iterator moves.
type Iter struct {
	r      *Reader
	index  blockIter
	data   blockIter
	err    error
	closed bool
}

var errClosed = errors.New("sstable: iterator is closed")

// load opens the data block the index is positioned at.
func (it *Iter) load() bool {
	if !it.index.valid {
		it.data.valid = false
		return false
	}
	d := &decoder{b: it.index.val}
	h := handle{d.uvarint(), d.uvarint()}
	if d.err != nil || len(d.b) != 0 {
		it.err = corrupt("bad index entry")
		return false
	}
	b, err := it.r.readBlock(h, true)
	if err != nil {
		it.err = err
		return false
	}
	if err := it.data.init(b); err != nil {
		it.err = err
		return false
	}
	return true
}

func (it *Iter) ok() bool {
	if it.closed {
		it.err = errClosed
	}
	if it.err == nil {
		it.err = it.index.err
	}
	if it.err == nil {
		it.err = it.data.err
	}
	if it.err != nil {
		it.data.valid = false
		return false
	}
	return true
}

func (it *Iter) skipForward() {
	for it.ok() && !it.data.valid && it.index.valid {
		it.index.nextEntry()
		if it.load() {
			it.data.first()
		}
	}
}

func (it *Iter) skipBackward() {
	for it.ok() && !it.data.valid && it.index.valid {
		it.index.prev()
		if it.load() {
			it.data.last()
		}
	}
}

func (it *Iter) seekGE(key []byte, seq uint64) {
	if !it.ok() {
		return
	}
	it.index.seekGE(key, seq)
	if it.load() {
		it.data.seekGE(key, seq)
	}
	it.skipForward()
}

// SeekGE moves to the first entry whose user key is at least key.
func (it *Iter) SeekGE(key []byte) { it.seekGE(key, seekSeq) }

// SeekLT moves to the last entry whose user key is less than key.
func (it *Iter) SeekLT(key []byte) {
	if !it.ok() {
		return
	}
	it.index.seekGE(key, seekSeq)
	if !it.index.valid {
		if it.index.err == nil {
			it.Last()
		}
		it.ok()
		return
	}
	if it.load() {
		it.data.seekLT(key, seekSeq)
	}
	it.skipBackward()
}

// First moves to the first entry.
func (it *Iter) First() {
	if !it.ok() {
		return
	}
	it.index.first()
	if it.load() {
		it.data.first()
	}
	it.skipForward()
}

// Last moves to the last entry.
func (it *Iter) Last() {
	if !it.ok() {
		return
	}
	it.index.last()
	if it.load() {
		it.data.last()
	}
	it.skipBackward()
}

// Next moves to the next entry.
func (it *Iter) Next() {
	if it.ok() && it.data.valid {
		it.data.nextEntry()
		it.skipForward()
	}
}

// Prev moves to the previous entry.
func (it *Iter) Prev() {
	if it.ok() && it.data.valid {
		it.data.prev()
		it.skipBackward()
	}
}

// Valid reports whether the iterator is at an entry.
func (it *Iter) Valid() bool { return it.err == nil && it.data.valid }

// Key returns the current entry's user key.
func (it *Iter) Key() []byte { k, _, _ := splitInternal(it.data.key); return k }

// Seq returns the current entry's sequence number.
func (it *Iter) Seq() uint64 { _, s, _ := splitInternal(it.data.key); return s }

// Kind returns the current entry's kind.
func (it *Iter) Kind() uint8 { _, _, k := splitInternal(it.data.key); return k }

// Value returns the current entry's value.
func (it *Iter) Value() []byte { return it.data.val }

// Err returns the error that stopped the iterator, if any.
func (it *Iter) Err() error {
	if it.err == errClosed {
		return nil
	}
	return it.err
}

// Close releases the iterator.
func (it *Iter) Close() error {
	if !it.closed {
		it.closed = true
		it.data.valid = false
		it.r.release()
	}
	return nil
}
