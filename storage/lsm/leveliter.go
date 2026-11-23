package lsm

import (
	"bytes"
	"sort"

	"github.com/sandgorgon/polydb/storage/sstable"
)

// levelIter walks the tables of one sorted level (1 and below) as one
// sequence, opening a table's iterator only when it gets there.
type levelIter struct {
	files []*tableMeta
	i     int // current file; -1 or len(files) when off the ends
	it    *sstable.Iter
	err   error
}

func newLevelIter(files []*tableMeta) *levelIter { return &levelIter{files: files, i: -1} }

func (l *levelIter) open(i int) bool {
	if l.it != nil {
		if err := l.it.Err(); err != nil && l.err == nil {
			l.err = err
		}
		l.it.Close()
		l.it = nil
	}
	l.i = i
	if l.err != nil || i < 0 || i >= len(l.files) {
		return false
	}
	l.it = l.files[i].r.NewIter()
	return true
}

func (l *levelIter) fail() bool {
	if l.it != nil && l.it.Err() != nil {
		if l.err == nil {
			l.err = l.it.Err()
		}
		return true
	}
	return l.err != nil
}

func (l *levelIter) skipForward() {
	for !l.fail() && l.it != nil && !l.it.Valid() {
		if !l.open(l.i + 1) {
			return
		}
		l.it.First()
	}
}

func (l *levelIter) skipBackward() {
	for !l.fail() && l.it != nil && !l.it.Valid() {
		if !l.open(l.i - 1) {
			return
		}
		l.it.Last()
	}
}

// SeekGE starts at the first file whose range reaches key.
func (l *levelIter) SeekGE(key []byte) {
	i := sort.Search(len(l.files), func(i int) bool { return l.files[i].b.beforeEnd(key) })
	if l.open(i) {
		l.it.SeekGE(key)
		l.skipForward()
	}
}

// SeekLT starts at the last file that starts below key.
func (l *levelIter) SeekLT(key []byte) {
	i := sort.Search(len(l.files), func(i int) bool { return bytes.Compare(l.files[i].b.lo, key) >= 0 }) - 1
	if l.open(i) {
		l.it.SeekLT(key)
		l.skipBackward()
	}
}

func (l *levelIter) First() {
	if l.open(0) {
		l.it.First()
		l.skipForward()
	}
}

func (l *levelIter) Last() {
	if l.open(len(l.files) - 1) {
		l.it.Last()
		l.skipBackward()
	}
}

func (l *levelIter) Next() {
	if l.Valid() {
		l.it.Next()
		l.skipForward()
	}
}

func (l *levelIter) Prev() {
	if l.Valid() {
		l.it.Prev()
		l.skipBackward()
	}
}

func (l *levelIter) Valid() bool   { return l.err == nil && l.it != nil && l.it.Valid() }
func (l *levelIter) Key() []byte   { return l.it.Key() }
func (l *levelIter) Seq() uint64   { return l.it.Seq() }
func (l *levelIter) Kind() uint8   { return l.it.Kind() }
func (l *levelIter) Value() []byte { return l.it.Value() }

func (l *levelIter) Err() error {
	l.fail()
	return l.err
}

func (l *levelIter) Close() error {
	l.open(-1)
	return nil
}
