package vfs

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// ErrInjected is the error Faulty injects.
var ErrInjected = errors.New("vfs: injected fault")

// Faulty wraps a file system and fails its operations on request, to
// test how the storage engine handles a disk that fails. Operations are
// numbered from 1 as they happen; reads are not counted (or failed).
type Faulty struct {
	FS
	ops    atomic.Int64
	failAt atomic.Int64 // the operation to fail; 0 for none
	sticky atomic.Bool  // fail every operation from failAt on
	mu     sync.Mutex
	failed []string // the operations that failed
}

// NewFaulty wraps fs.
func NewFaulty(fs FS) *Faulty { return &Faulty{FS: fs} }

// FailAt makes operation n fail; with sticky, every operation after it
// fails too, as on a disk that died.
func (f *Faulty) FailAt(n int64, sticky bool) {
	f.sticky.Store(sticky)
	f.failAt.Store(n)
}

// Ops returns the number of operations so far.
func (f *Faulty) Ops() int64 { return f.ops.Load() }

// Failed returns the operations that failed.
func (f *Faulty) Failed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.failed...)
}

func (f *Faulty) check(op string) error {
	n := f.ops.Add(1)
	at := f.failAt.Load()
	if at == 0 || n < at || (n > at && !f.sticky.Load()) {
		return nil
	}
	f.mu.Lock()
	f.failed = append(f.failed, op)
	f.mu.Unlock()
	return ErrInjected
}

func (f *Faulty) Create(name string) (File, error) {
	if err := f.check("create " + name); err != nil {
		return nil, err
	}
	file, err := f.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &faultyFile{file, f, name}, nil
}

func (f *Faulty) OpenAppend(name string) (File, error) {
	if err := f.check("open-append " + name); err != nil {
		return nil, err
	}
	file, err := f.FS.OpenAppend(name)
	if err != nil {
		return nil, err
	}
	return &faultyFile{file, f, name}, nil
}

func (f *Faulty) Remove(name string) error {
	if err := f.check("remove " + name); err != nil {
		return err
	}
	return f.FS.Remove(name)
}

func (f *Faulty) Rename(oldname, newname string) error {
	if err := f.check("rename " + oldname); err != nil {
		return err
	}
	return f.FS.Rename(oldname, newname)
}

func (f *Faulty) MkdirAll(dir string) error {
	if err := f.check("mkdir " + dir); err != nil {
		return err
	}
	return f.FS.MkdirAll(dir)
}

func (f *Faulty) SyncDir(dir string) error {
	if err := f.check("syncdir " + dir); err != nil {
		return err
	}
	return f.FS.SyncDir(dir)
}

func (f *Faulty) Lock(name string) (io.Closer, error) {
	if err := f.check("lock " + name); err != nil {
		return nil, err
	}
	return f.FS.Lock(name)
}

type faultyFile struct {
	File
	f    *Faulty
	name string
}

// Write fails before writing anything, or, when it fails, writes a
// prefix first: a failed write may still have reached the file.
func (ff *faultyFile) Write(p []byte) (int, error) {
	if err := ff.f.check("write " + ff.name); err != nil {
		n := len(p) / 2
		ff.File.Write(p[:n])
		return n, err
	}
	return ff.File.Write(p)
}

func (ff *faultyFile) Sync() error {
	if err := ff.f.check("sync " + ff.name); err != nil {
		return err
	}
	return ff.File.Sync()
}

func (ff *faultyFile) Truncate(size int64) error {
	if err := ff.f.check("truncate " + ff.name); err != nil {
		return err
	}
	return ff.File.Truncate(size)
}
