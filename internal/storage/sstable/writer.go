package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"

	"github.com/sandgorgon/polydb/internal/storage/vfs"
)

const (
	footerSize   = 80
	blockTrailer = 5 // compression type, CRC-32C
	version      = 1
	compNone     = 0
)

var magic = []byte("PLYSST\x00\x01")

// Tombstone deletes the keys in [Start, End) written before Seq; a nil
// End is unbounded.
type Tombstone struct {
	Start, End []byte
	Seq        uint64
}

// Props describes a table.
type Props struct {
	Entries    uint64 // point entries
	Deletions  uint64 // point entries that are deletions
	Tombstones uint64 // range tombstones
	DataBlocks uint64
	MinSeq     uint64 // over entries and tombstones; 0 if there are none
	MaxSeq     uint64
	// Smallest and Largest are the first and last user keys of the point
	// entries (nil if there are none).
	Smallest, Largest []byte
	Size              uint64 // file size
}

// WriterOptions configure a Writer; zero values get defaults.
type WriterOptions struct {
	BlockSize       int // target data block size; default 4 KiB
	RestartInterval int // default 16
	BloomBitsPerKey int // default 10, about a 1% false-positive rate
}

// Writer writes a table. Entries must be added in internal key order.
type Writer struct {
	f      vfs.File
	o      WriterOptions
	off    uint64
	data   blockWriter
	index  blockWriter
	hashes []uint64
	tombs  []Tombstone
	props  Props
	last   []byte // last user key
	lastSq uint64
	ikey   []byte
	err    error
	done   bool
}

type handle struct{ off, n uint64 }

// NewWriter returns a writer to f, which it closes in Finish.
func NewWriter(f vfs.File, o WriterOptions) *Writer {
	if o.BlockSize <= 0 {
		o.BlockSize = 4096
	}
	if o.RestartInterval <= 0 {
		o.RestartInterval = 16
	}
	if o.BloomBitsPerKey <= 0 {
		o.BloomBitsPerKey = 10
	}
	w := &Writer{f: f, o: o}
	w.data.interval = o.RestartInterval
	w.index.interval = 1
	return w
}

func (w *Writer) seq(seq uint64) {
	if w.props.Entries+w.props.Tombstones == 0 || seq < w.props.MinSeq {
		w.props.MinSeq = seq
	}
	w.props.MaxSeq = max(w.props.MaxSeq, seq)
}

// Add adds an entry. The table keeps key and value only until Add
// returns.
func (w *Writer) Add(key []byte, seq uint64, kind uint8, value []byte) error {
	if w.err != nil {
		return w.err
	}
	switch {
	case w.done:
		return errors.New("sstable: Add after Finish")
	case kind != KindSet && kind != KindDelete:
		return fmt.Errorf("sstable: bad entry kind %d", kind)
	case seq > MaxSeq:
		return fmt.Errorf("sstable: sequence number %d too large", seq)
	case kind == KindDelete && len(value) > 0:
		return errors.New("sstable: a deletion has no value")
	}
	if w.props.Entries > 0 {
		c := Compare(w.last, w.lastSq, key, seq)
		if c >= 0 {
			return fmt.Errorf("sstable: entry (%q, %d) is not after (%q, %d)", key, seq, w.last, w.lastSq)
		}
	}
	if w.props.Entries == 0 || !bytes.Equal(w.last, key) {
		w.hashes = append(w.hashes, bloomHash(key))
	}
	if w.props.Entries == 0 {
		w.props.Smallest = bytes.Clone(key)
	}
	w.seq(seq)
	w.props.Entries++
	if kind == KindDelete {
		w.props.Deletions++
	}
	w.last, w.lastSq = append(w.last[:0], key...), seq
	w.ikey = appendInternal(w.ikey[:0], key, seq, kind)
	w.data.add(w.ikey, value)
	if w.data.estimate() >= w.o.BlockSize {
		return w.flushData()
	}
	return nil
}

// AddTombstone adds a range tombstone; tombstones may come in any order.
func (w *Writer) AddTombstone(t Tombstone) error {
	switch {
	case w.err != nil:
		return w.err
	case w.done:
		return errors.New("sstable: AddTombstone after Finish")
	case t.Seq > MaxSeq:
		return fmt.Errorf("sstable: sequence number %d too large", t.Seq)
	case t.End != nil && bytes.Compare(t.Start, t.End) >= 0:
		return fmt.Errorf("sstable: empty tombstone [%q, %q)", t.Start, t.End)
	}
	w.seq(t.Seq)
	w.props.Tombstones++
	w.tombs = append(w.tombs, Tombstone{Start: bytes.Clone(t.Start), End: bytes.Clone(t.End), Seq: t.Seq})
	return nil
}

// EstimatedSize is the size of the file so far, for splitting output.
func (w *Writer) EstimatedSize() uint64 { return w.off + uint64(w.data.estimate()) }

func (w *Writer) writeBlock(b []byte) (handle, error) {
	h := handle{w.off, uint64(len(b))}
	trailer := []byte{compNone, 0, 0, 0, 0}
	crc := crc32.Update(crc32.Checksum(b, castagnoli), castagnoli, trailer[:1])
	binary.LittleEndian.PutUint32(trailer[1:], crc)
	if _, err := w.f.Write(b); err != nil {
		w.err = err
		return h, err
	}
	if _, err := w.f.Write(trailer); err != nil {
		w.err = err
		return h, err
	}
	w.off += uint64(len(b)) + blockTrailer
	return h, nil
}

func (w *Writer) flushData() error {
	if w.data.count == 0 {
		return nil
	}
	lastKey := slices.Clone(w.data.last)
	h, err := w.writeBlock(w.data.finish())
	if err != nil {
		return err
	}
	w.data.reset()
	w.props.DataBlocks++
	w.index.add(lastKey, binary.AppendUvarint(binary.AppendUvarint(nil, h.off), h.n))
	return nil
}

func appendStr(b, s []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

func encodeTombstones(ts []Tombstone) []byte {
	b := binary.AppendUvarint(nil, uint64(len(ts)))
	for _, t := range ts {
		b = appendStr(b, t.Start)
		if t.End == nil {
			b = append(b, 0)
		} else {
			b = appendStr(append(b, 1), t.End)
		}
		b = binary.AppendUvarint(b, t.Seq)
	}
	return b
}

func encodeProps(p Props) []byte {
	var b []byte
	for _, v := range []uint64{p.Entries, p.Deletions, p.Tombstones, p.DataBlocks, p.MinSeq, p.MaxSeq} {
		b = binary.AppendUvarint(b, v)
	}
	b = appendStr(b, p.Smallest)
	return appendStr(b, p.Largest)
}

// Finish writes the metadata and footer, syncs and closes the file, and
// returns the table's properties. The caller makes the file's name
// durable (vfs.FS.SyncDir).
func (w *Writer) Finish() (Props, error) {
	if w.done {
		return Props{}, errors.New("sstable: Finish called twice")
	}
	w.done = true
	p, err := w.finish()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return p, err
}

func (w *Writer) finish() (Props, error) {
	if w.err != nil {
		return Props{}, w.err
	}
	if err := w.flushData(); err != nil {
		return Props{}, err
	}
	if w.props.Entries > 0 {
		w.props.Largest = bytes.Clone(w.last)
	}
	slices.SortFunc(w.tombs, func(a, b Tombstone) int { return Compare(a.Start, a.Seq, b.Start, b.Seq) })
	var hs [4]handle
	for i, b := range [][]byte{buildFilter(w.hashes, w.o.BloomBitsPerKey), encodeTombstones(w.tombs), encodeProps(w.props), w.index.finish()} {
		h, err := w.writeBlock(b)
		if err != nil {
			return Props{}, err
		}
		hs[i] = h
	}
	// Footer: filter, tombstones, properties and index handles, version,
	// CRC-32C of the above, magic.
	var f []byte
	for _, h := range hs {
		f = binary.LittleEndian.AppendUint64(f, h.off)
		f = binary.LittleEndian.AppendUint64(f, h.n)
	}
	f = binary.LittleEndian.AppendUint32(f, version)
	f = binary.LittleEndian.AppendUint32(f, crc32.Checksum(f, castagnoli))
	f = append(f, magic...)
	if _, err := w.f.Write(f); err != nil {
		return Props{}, err
	}
	w.off += uint64(len(f))
	if err := w.f.Sync(); err != nil {
		return Props{}, err
	}
	w.props.Size = w.off
	return w.props, nil
}
