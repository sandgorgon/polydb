// Package sstable reads and writes PolyDB's sorted table files: the
// immutable files that the LSM store (storage/lsm) flushes memtables to
// and compacts.
//
// A table holds versioned entries (user key, sequence number, kind,
// value) in internal key order — user key ascending, then newest first —
// plus range tombstones. The file is a sequence of blocks, each followed
// by a one-byte compression type (always 0, none, for now) and a CRC-32C
// of the block and that byte:
//
//	data blocks | filter | range tombstones | properties | index | footer
//
// Data blocks (about 4 KiB) hold entries with their keys prefix-compressed
// against the previous key, and restart points (every 16 entries) where a
// key is stored whole, which a seek binary-searches. The index block has
// one entry per data block: its last internal key and its position. The
// filter is a bloom filter over the table's user keys, so most lookups of
// absent keys never read a data block. The fixed-size footer locates the
// metadata blocks and ends with a magic number.
//
// Readers memory-map the file when the file system allows it (vfs.Mapper)
// and verify each block's checksum the first time they read it;
// otherwise blocks are read into a shared block cache (Cache).
package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// Entry kinds.
const (
	KindSet    = 1
	KindDelete = 2
)

// MaxSeq is the largest sequence number a table can hold; the stored
// trailer is seq<<8 | kind.
const MaxSeq = 1<<56 - 1

// seekSeq sorts before every version of a key: seeking to (key, seekSeq)
// finds the newest version of key.
const seekSeq = ^uint64(0)

// ErrCorrupt reports a damaged table file.
var ErrCorrupt = errors.New("sstable: corrupt table")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Compare orders internal keys: by user key, then newest version first.
func Compare(ak []byte, aseq uint64, bk []byte, bseq uint64) int {
	if c := bytes.Compare(ak, bk); c != 0 {
		return c
	}
	switch {
	case aseq > bseq:
		return -1
	case aseq < bseq:
		return 1
	}
	return 0
}

// An internal key in a block is the user key followed by a big-endian
// trailer, seq<<8 | kind.
func appendInternal(b, key []byte, seq uint64, kind uint8) []byte {
	return binary.BigEndian.AppendUint64(append(b, key...), seq<<8|uint64(kind))
}

func splitInternal(ikey []byte) (key []byte, seq uint64, kind uint8) {
	n := len(ikey) - 8
	t := binary.BigEndian.Uint64(ikey[n:])
	return ikey[:n], t >> 8, uint8(t)
}

// blockWriter builds a block.
type blockWriter struct {
	interval int
	buf      []byte
	restarts []uint32
	last     []byte
	sinceRst int
	count    int
}

func (w *blockWriter) add(ikey, value []byte) {
	shared := 0
	if w.count > 0 && w.sinceRst < w.interval {
		for shared < len(ikey) && shared < len(w.last) && ikey[shared] == w.last[shared] {
			shared++
		}
	} else {
		w.restarts = append(w.restarts, uint32(len(w.buf)))
		w.sinceRst = 0
	}
	w.buf = binary.AppendUvarint(w.buf, uint64(shared))
	w.buf = binary.AppendUvarint(w.buf, uint64(len(ikey)-shared))
	w.buf = binary.AppendUvarint(w.buf, uint64(len(value)))
	w.buf = append(w.buf, ikey[shared:]...)
	w.buf = append(w.buf, value...)
	w.last = append(w.last[:0], ikey...)
	w.sinceRst++
	w.count++
}

func (w *blockWriter) estimate() int { return len(w.buf) + 4*len(w.restarts) + 4 }

// finish returns the block; it is valid until reset.
func (w *blockWriter) finish() []byte {
	for _, r := range w.restarts {
		w.buf = binary.LittleEndian.AppendUint32(w.buf, r)
	}
	return binary.LittleEndian.AppendUint32(w.buf, uint32(len(w.restarts)))
}

func (w *blockWriter) reset() {
	w.buf, w.restarts, w.last, w.sinceRst, w.count = w.buf[:0], w.restarts[:0], w.last[:0], 0, 0
}

// blockIter walks one block. Key returns the current internal key, in a
// buffer the iterator reuses.
type blockIter struct {
	data     []byte // the entries
	restarts []byte // the restart array
	nrst     int
	off      int // offset of the current entry
	next     int // offset of the next one
	key      []byte
	val      []byte
	valid    bool
	err      error
}

func (it *blockIter) init(b []byte) error {
	*it = blockIter{key: it.key[:0]}
	if len(b) < 4 {
		return it.fail("block too short")
	}
	n := binary.LittleEndian.Uint32(b[len(b)-4:])
	if uint64(n)*4 > uint64(len(b)-4) {
		return it.fail("bad restart count")
	}
	it.nrst = int(n)
	end := len(b) - 4 - 4*it.nrst
	it.data, it.restarts = b[:end], b[end:len(b)-4]
	if it.nrst == 0 && end != 0 {
		return it.fail("entries without restart points")
	}
	return nil
}

func (it *blockIter) fail(msg string) error {
	it.valid = false
	if it.err == nil {
		it.err = corrupt("%s", msg)
	}
	return it.err
}

func (it *blockIter) restart(i int) int {
	return int(binary.LittleEndian.Uint32(it.restarts[4*i:]))
}

// header parses the entry header at off.
func (it *blockIter) header(off int) (shared, unshared, vlen, p int, ok bool) {
	if off < 0 || off >= len(it.data) {
		return 0, 0, 0, 0, false
	}
	p = off
	var v [3]uint64
	for i := range v {
		x, n := binary.Uvarint(it.data[p:])
		if n <= 0 {
			return 0, 0, 0, 0, false
		}
		v[i], p = x, p+n
	}
	if v[1] > uint64(len(it.data)-p) || v[2] > uint64(len(it.data)-p)-v[1] {
		return 0, 0, 0, 0, false
	}
	return int(v[0]), int(v[1]), int(v[2]), p, true
}

// step decodes the entry at it.next; it.key must hold the previous key.
func (it *blockIter) step() {
	if it.err != nil {
		return
	}
	if it.next >= len(it.data) {
		it.valid = false
		return
	}
	shared, unshared, vlen, p, ok := it.header(it.next)
	if !ok || shared > len(it.key) || (!it.valid && shared != 0) {
		it.fail("bad entry")
		return
	}
	it.key = append(it.key[:shared], it.data[p:p+unshared]...)
	if len(it.key) < 8 {
		it.fail("short key")
		return
	}
	if _, _, kind := splitInternal(it.key); kind != KindSet && kind != KindDelete {
		it.fail("bad entry kind")
		return
	}
	it.val = it.data[p+unshared : p+unshared+vlen : p+unshared+vlen]
	it.off, it.next, it.valid = it.next, p+unshared+vlen, true
}

func (it *blockIter) seekRestart(i int) {
	it.valid, it.key = false, it.key[:0]
	it.next = it.restart(i)
	if it.next > len(it.data) {
		it.fail("bad restart point")
		return
	}
	it.step()
}

// restartKey returns the key stored whole at restart point i.
func (it *blockIter) restartKey(i int) ([]byte, bool) {
	shared, unshared, _, p, ok := it.header(it.restart(i))
	if !ok || shared != 0 || unshared < 8 {
		it.fail("bad restart entry")
		return nil, false
	}
	return it.data[p : p+unshared], true
}

func (it *blockIter) first() {
	if it.nrst == 0 {
		it.valid = false
		return
	}
	it.seekRestart(0)
}

func (it *blockIter) last() {
	if it.nrst == 0 {
		it.valid = false
		return
	}
	it.seekRestart(it.nrst - 1)
	for it.valid && it.next < len(it.data) {
		it.step()
	}
}

func (it *blockIter) nextEntry() { it.step() }

// seekGE moves to the first entry at or after (key, seq).
func (it *blockIter) seekGE(key []byte, seq uint64) {
	if it.nrst == 0 {
		it.valid = false
		return
	}
	// The first restart point whose key is at or after the target; the
	// target is in the interval before it.
	i := sort.Search(it.nrst, func(i int) bool {
		rk, ok := it.restartKey(i)
		if !ok {
			return true
		}
		k, s, _ := splitInternal(rk)
		return Compare(k, s, key, seq) >= 0
	})
	if it.err != nil {
		return
	}
	it.seekRestart(max(i-1, 0))
	for it.valid {
		k, s, _ := splitInternal(it.key)
		if Compare(k, s, key, seq) >= 0 {
			return
		}
		it.step()
	}
}

// prev moves to the entry before the current one, by scanning forward
// from the restart point before it.
func (it *blockIter) prev() {
	if !it.valid {
		return
	}
	target := it.off
	r := sort.Search(it.nrst, func(i int) bool { return it.restart(i) >= target }) - 1
	if r < 0 {
		it.valid = false
		return
	}
	it.seekRestart(r)
	for it.valid && it.next < target {
		it.step()
	}
}

// seekLT moves to the last entry before (key, seq).
func (it *blockIter) seekLT(key []byte, seq uint64) {
	it.seekGE(key, seq)
	if it.err != nil {
		return
	}
	if it.valid {
		it.prev()
	} else {
		it.last()
	}
}
