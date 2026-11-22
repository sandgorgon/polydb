package txn

import (
	"bytes"
	"sort"

	"github.com/sandgorgon/polydb/storage"
)

// Iter iterates over the snapshot merged with the transaction's own
// writes. The transaction must not be written to while the iterator is in
// use.
func (t *Txn) Iter(opts storage.IterOptions) storage.Iterator {
	if t.done {
		return &mergeIter{err: ErrDone}
	}
	// The buffered keys in range, in iteration order.
	keys := t.sortedKeys()
	lo, hi := 0, len(keys)
	if opts.Lower != nil {
		lo = sort.SearchStrings(keys, string(opts.Lower))
	}
	if opts.Upper != nil {
		hi = sort.SearchStrings(keys, string(opts.Upper))
	}
	var buf []string
	if lo < hi {
		buf = keys[lo:hi]
	}
	return &mergeIter{t: t, snap: t.snap.Iter(opts), buf: buf, reverse: opts.Reverse, bi: -1}
}

// mergeIter merges a snapshot iterator with buffered writes. Buffered
// entries take precedence; deleted ones and snapshot keys covered by a
// buffered range deletion are skipped.
type mergeIter struct {
	t       *Txn
	snap    storage.Iterator
	buf     []string
	reverse bool
	bi      int // position in buf, in iteration order; -1 before the start

	snapOK   bool // snap holds an entry not yet consumed
	started  bool
	key, val []byte
	err      error
}

func (it *mergeIter) bufKey(i int) string {
	if it.reverse {
		return it.buf[len(it.buf)-1-i]
	}
	return it.buf[i]
}

// before reports whether a comes before b in iteration order.
func (it *mergeIter) before(a, b []byte) bool {
	c := bytes.Compare(a, b)
	if it.reverse {
		return c > 0
	}
	return c < 0
}

func (it *mergeIter) Next() bool {
	if it.err != nil || it.t == nil {
		return false
	}
	if !it.started {
		it.started = true
		it.bi = 0
		it.snapOK = it.snap.Next()
	}
	for {
		haveBuf := it.bi < len(it.buf)
		if !haveBuf && !it.snapOK {
			it.key, it.val = nil, nil
			if err := it.snap.Err(); err != nil {
				it.err = err
			}
			return false
		}
		var bk []byte
		if haveBuf {
			bk = []byte(it.bufKey(it.bi))
		}
		switch {
		case it.snapOK && (!haveBuf || it.before(it.snap.Key(), bk)):
			// A snapshot key with no buffered write.
			k := it.snap.Key()
			v := it.snap.Value()
			it.snapOK = it.snap.Next()
			if it.t.rangeDeleted(k) {
				continue
			}
			it.key, it.val = k, v
			return true
		default:
			// A buffered key; it hides an equal snapshot key.
			if it.snapOK && bytes.Equal(it.snap.Key(), bk) {
				it.snapOK = it.snap.Next()
			}
			e := it.t.writes[string(bk)]
			it.bi++
			if e.del {
				continue
			}
			it.key, it.val = bk, e.val
			return true
		}
	}
}

func (it *mergeIter) Key() []byte   { return it.key }
func (it *mergeIter) Value() []byte { return it.val }
func (it *mergeIter) Err() error    { return it.err }
func (it *mergeIter) Close() error {
	if it.snap != nil {
		it.snap.Close()
	}
	it.t, it.key, it.val = nil, nil, nil
	return nil
}
