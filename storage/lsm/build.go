package lsm

import (
	"errors"
	"fmt"

	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/storage/wal"
)

// Build writes a new store in o.Dir holding the entries next returns, in
// increasing key order, as tables in the last level: what a fully
// compacted store holds. Open opens it like any other store. next
// returns ok false after the last entry. The directory must not hold a
// store.
//
// Backups use it to write a snapshot of a store, which needs no flush
// and drops every older version and deletion.
func Build(o Options, next func() (key, value []byte, ok bool, err error)) (err error) {
	o.defaults()
	if err := o.FS.MkdirAll(o.Dir); err != nil {
		return err
	}
	if names, err := o.FS.List(o.Dir); err != nil {
		return err
	} else if len(names) > 0 {
		return fmt.Errorf("lsm: %s is not empty", o.Dir)
	}
	s := &Store{o: o}
	s.nextFile.Store(1)
	out := &outputs{s: s, split: true}
	defer func() {
		if err != nil {
			out.abandon()
		}
	}()
	// Every entry is one version, all at sequence 1: the store starts as
	// if one batch had written it.
	var last []byte
	for {
		k, v, ok, err := next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if last != nil && string(k) <= string(last) {
			return errors.New("lsm: Build keys are not in increasing order")
		}
		if err := out.add(k, 1, kindSet, v); err != nil {
			return err
		}
		last = append(last[:0], k...)
	}
	if err := out.close(); err != nil {
		return err
	}
	if err := o.FS.SyncDir(o.Dir); err != nil {
		return err
	}
	m, err := wal.Open(wal.Options{FS: o.FS, Dir: vfs.Join(o.Dir, "manifest"), SegmentSize: 1 << 40}, func([]byte) error {
		return errors.New("lsm: Build found a manifest")
	})
	if err != nil {
		return err
	}
	e := &edit{snapshot: true, nextFile: s.nextFile.Load()}
	if len(out.made) > 0 {
		e.flushedSeq = 1
	}
	for _, t := range out.made {
		e.added = append(e.added, describe(numLevels-1, t))
	}
	if err := m.Append(e.encode()); err != nil {
		m.Close()
		return err
	}
	if err := m.Close(); err != nil {
		return err
	}
	return o.FS.SyncDir(o.Dir)
}
