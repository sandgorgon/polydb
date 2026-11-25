// Package lsm is PolyDB's on-disk storage engine: a log-structured merge
// tree. Every batch is written to the write-ahead log (storage/wal) and
// then to an in-memory table (the memtable); full memtables are written
// to sorted table files, which compaction merges in the background.
//
// Every stored entry carries the sequence number of the batch that wrote
// it, so a snapshot reads the newest version at or below its sequence
// number, and old versions are kept for as long as a snapshot may need
// them. The store implements storage.Store and storage.Preparer.
package lsm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/internal/storage/sstable"
)

// Entry kinds, shared with table files.
const (
	kindSet    = sstable.KindSet
	kindDelete = sstable.KindDelete
)

// maxSeq is larger than any real sequence number; (key, maxSeq) sorts
// before every version of key.
const maxSeq = ^uint64(0)

// compare orders internal keys: by user key, then newest version first.
func compare(ak []byte, aseq uint64, bk []byte, bseq uint64) int {
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

// point is a set or delete of one key.
type point struct {
	key   []byte
	kind  uint8
	value []byte
}

// tombstone deletes the keys in [start, end) written before seq; a nil
// end is unbounded.
type tombstone struct {
	start, end []byte
	seq        uint64
}

// contains reports whether key is in [start, end).
func (t tombstone) contains(key []byte) bool {
	return bytes.Compare(key, t.start) >= 0 && (t.end == nil || bytes.Compare(key, t.end) < 0)
}

// covers reports whether the tombstone deletes the version of key written
// at seq, as seen by a snapshot at snap.
func (t tombstone) covers(key []byte, seq, snap uint64) bool {
	return t.seq <= snap && t.seq > seq && t.contains(key)
}

// normalize reduces a batch to its effect, so that all its entries can
// share one sequence number: the last write to a key wins, and a range
// deletion cancels the earlier writes it covers. The points that remain
// come after every range deletion covering them, which is why an entry
// survives a tombstone with the same sequence number.
func normalize(b *storage.Batch) (points []point, ranges []tombstone) {
	byKey := map[string]int{}
	for _, op := range b.Ops() {
		switch op.Kind {
		case storage.OpSet, storage.OpDelete:
			p := point{key: op.Key, kind: kindSet, value: op.Value}
			if op.Kind == storage.OpDelete {
				p = point{key: op.Key, kind: kindDelete}
			}
			if i, ok := byKey[string(op.Key)]; ok {
				points[i] = p
			} else {
				byKey[string(op.Key)] = len(points)
				points = append(points, p)
			}
		case storage.OpDeleteRange:
			t := tombstone{start: op.Key, end: op.End}
			kept := points[:0]
			for _, p := range points {
				if t.contains(p.key) {
					delete(byKey, string(p.key))
					continue
				}
				kept = append(kept, p)
			}
			points = kept
			for i, p := range points {
				byKey[string(p.key)] = i
			}
			ranges = append(ranges, t)
		}
	}
	slices.SortFunc(points, func(a, b point) int { return bytes.Compare(a.key, b.key) })
	return points, ranges
}

// A log record is one batch:
//
//	seq (8) | uvarint points | points | uvarint ranges | ranges
//	point: kind (1) | str key | str value (sets only)
//	range: str start | flag (1: bounded) | str end (if bounded)
func encodeRecord(seq uint64, points []point, ranges []tombstone) []byte {
	b := binary.BigEndian.AppendUint64(nil, seq)
	b = binary.AppendUvarint(b, uint64(len(points)))
	for _, p := range points {
		b = append(b, p.kind)
		b = appendStr(b, p.key)
		if p.kind == kindSet {
			b = appendStr(b, p.value)
		}
	}
	b = binary.AppendUvarint(b, uint64(len(ranges)))
	for _, r := range ranges {
		b = appendStr(b, r.start)
		if r.end == nil {
			b = append(b, 0)
		} else {
			b = appendStr(append(b, 1), r.end)
		}
	}
	return b
}

func appendStr(b, s []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

var errBadRecord = errors.New("lsm: malformed log record")

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errBadRecord
		r.b = nil
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) byte() byte {
	if len(r.b) == 0 {
		r.err = errBadRecord
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) str() []byte {
	n := r.uvarint()
	if n > uint64(len(r.b)) {
		r.err = errBadRecord
		r.b = nil
		return nil
	}
	s := r.b[:n:n]
	r.b = r.b[n:]
	return s
}

func decodeRecord(rec []byte) (seq uint64, points []point, ranges []tombstone, err error) {
	if len(rec) < 8 {
		return 0, nil, nil, errBadRecord
	}
	seq = binary.BigEndian.Uint64(rec)
	r := &reader{b: rec[8:]}
	n := r.uvarint()
	if r.err != nil || n > uint64(len(r.b)) {
		return 0, nil, nil, errBadRecord
	}
	for range n {
		p := point{kind: r.byte()}
		p.key = r.str()
		switch p.kind {
		case kindSet:
			p.value = r.str()
		case kindDelete:
		default:
			r.err = errBadRecord
		}
		if r.err != nil {
			return 0, nil, nil, r.err
		}
		points = append(points, p)
	}
	m := r.uvarint()
	if r.err != nil || m > uint64(len(r.b)) {
		return 0, nil, nil, errBadRecord
	}
	for range m {
		t := tombstone{start: r.str(), seq: seq}
		switch r.byte() {
		case 0:
		case 1:
			t.end = r.str()
		default:
			r.err = errBadRecord
		}
		if r.err != nil {
			return 0, nil, nil, r.err
		}
		ranges = append(ranges, t)
	}
	if len(r.b) != 0 {
		return 0, nil, nil, errBadRecord
	}
	return seq, points, ranges, nil
}
