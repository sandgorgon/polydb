// Package storage defines PolyDB's ordered key-value storage interface.
//
// A Store maps byte-string keys to byte-string values in byte order. It
// offers consistent snapshots and atomic batches, each committed with an
// increasing sequence number. Transactions (txn/) are built on top: they
// read from a snapshot, buffer writes in a Batch, check for conflicts and
// then Apply the batch.
//
// Implementations: storage/mem (in memory) and, later, an on-disk LSM
// engine. storage/storagetest holds the conformance tests every
// implementation must pass.
//
// Keys and values returned by a Reader must not be modified, and stay valid
// for as long as the caller holds them. Keys and values passed to a Batch
// are copied, so callers may reuse their buffers.
package storage

import (
	"bytes"
	"errors"
	"slices"
)

// Errors returned by stores.
var (
	ErrClosed   = errors.New("storage: store is closed")
	ErrReleased = errors.New("storage: snapshot released")
)

// Reader reads a consistent view of the data.
type Reader interface {
	// Get returns the value for key, and whether it exists.
	Get(key []byte) (value []byte, ok bool, err error)
	// Iter returns an iterator over the keys in [opts.Lower, opts.Upper).
	Iter(opts IterOptions) Iterator
}

// IterOptions bounds an iteration. A nil bound is unbounded.
type IterOptions struct {
	Lower   []byte // inclusive
	Upper   []byte // exclusive
	Reverse bool   // iterate from the largest key down
}

// Iterator walks keys in order. Call Next before reading the first entry:
//
//	it := r.Iter(opts)
//	defer it.Close()
//	for it.Next() {
//		use(it.Key(), it.Value())
//	}
//	if err := it.Err(); err != nil { ... }
type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

// Snapshot is a read-only view of the store as of one sequence number. It
// doesn't change when later batches are applied. Release it when done so
// the store can free old data.
type Snapshot interface {
	Reader
	// Seq returns the sequence number of the last batch the snapshot sees.
	Seq() uint64
	Release()
}

// Store is an ordered key-value store. It is safe for concurrent use.
// Reads through the Store itself see the latest applied batch.
type Store interface {
	Reader
	// Snapshot returns a consistent view of the current data.
	Snapshot() Snapshot
	// Apply commits b atomically: readers see all of it or none of it.
	// It returns the batch's sequence number, one more than the previous.
	// An empty batch is still given a sequence number.
	Apply(b *Batch) (seq uint64, err error)
	// Seq returns the sequence number of the last applied batch (0 if none).
	Seq() uint64
	Close() error
}

// Preparer is implemented by stores that can split a commit in two, so
// that the transaction layer can check conflicts and order commits under
// its lock while waiting for durability outside it. That is what lets
// concurrent commits share one sync (group commit).
type Preparer interface {
	// Prepare assigns b its sequence number and starts writing it. The
	// batch becomes visible to readers when commit returns nil; batches
	// become visible in sequence order. commit must be called exactly once.
	Prepare(b *Batch) (seq uint64, commit func() error, err error)
}

// OpKind is the kind of a batch operation.
type OpKind uint8

const (
	OpSet OpKind = iota + 1
	OpDelete
	OpDeleteRange // deletes keys in [Key, End)
)

// Op is one operation in a Batch.
type Op struct {
	Kind  OpKind
	Key   []byte
	Value []byte // OpSet
	End   []byte // OpDeleteRange; nil means no upper bound
}

// Batch is a list of writes applied atomically and in order. The zero
// Batch is empty and ready to use.
type Batch struct {
	ops  []Op
	size int
}

// Set adds a write of value to key.
func (b *Batch) Set(key, value []byte) {
	b.ops = append(b.ops, Op{Kind: OpSet, Key: bytes.Clone(nonNil(key)), Value: bytes.Clone(nonNil(value))})
	b.size += len(key) + len(value)
}

// Delete adds a deletion of key.
func (b *Batch) Delete(key []byte) {
	b.ops = append(b.ops, Op{Kind: OpDelete, Key: bytes.Clone(nonNil(key))})
	b.size += len(key)
}

// DeleteRange adds a deletion of every key in [start, end). A nil end
// deletes everything from start on.
func (b *Batch) DeleteRange(start, end []byte) {
	b.ops = append(b.ops, Op{Kind: OpDeleteRange, Key: bytes.Clone(nonNil(start)), End: bytes.Clone(end)})
	b.size += len(start) + len(end)
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// Ops returns the operations in order. They must not be modified.
func (b *Batch) Ops() []Op { return b.ops }

// Len returns the number of operations.
func (b *Batch) Len() int { return len(b.ops) }

// Size returns the total bytes of keys and values in the batch.
func (b *Batch) Size() int { return b.size }

// Reset empties the batch for reuse.
func (b *Batch) Reset() {
	clear(b.ops)
	b.ops, b.size = b.ops[:0], 0
}

// PrefixEnd returns the smallest key greater than every key that starts
// with prefix, for use as an exclusive upper bound. It returns nil
// (unbounded) if no such key exists, such as for an empty prefix or one of
// all 0xFF bytes.
func PrefixEnd(prefix []byte) []byte {
	end := slices.Clone(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// InRange reports whether key is within [lower, upper), where nil bounds
// are unbounded.
func InRange(key, lower, upper []byte) bool {
	return (lower == nil || bytes.Compare(key, lower) >= 0) &&
		(upper == nil || bytes.Compare(key, upper) < 0)
}
