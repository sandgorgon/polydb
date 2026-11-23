// Package vfs is the small file-system interface the storage engine uses,
// so that tests can run it on a file system that simulates crashes.
//
// The OS implementation makes Sync durable: on macOS it uses
// F_FULLFSYNC, because fsync there doesn't reach stable storage. Mem
// keeps everything in memory and can Crash, losing whatever wasn't
// synced, as a power cut would.
package vfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// File is an open file.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	// Sync makes the file's data durable.
	Sync() error
	// Size returns the file's length.
	Size() (int64, error)
	// Truncate cuts the file to size bytes.
	Truncate(size int64) error
	Close() error
}

// FS is a file system.
type FS interface {
	// Create creates a file for writing, replacing any existing one. The
	// new name is durable only after SyncDir of its directory.
	Create(name string) (File, error)
	// OpenAppend opens an existing file for appending.
	OpenAppend(name string) (File, error)
	// Open opens a file for reading.
	Open(name string) (File, error)
	// Remove deletes a file; durable after SyncDir.
	Remove(name string) error
	// Rename atomically replaces newname with oldname; durable after
	// SyncDir.
	Rename(oldname, newname string) error
	// List returns the names in a directory, sorted.
	List(dir string) ([]string, error)
	// MkdirAll creates a directory and its parents.
	MkdirAll(dir string) error
	// SyncDir makes the directory's entries durable.
	SyncDir(dir string) error
	// Lock takes an exclusive lock on a file, so that two processes can't
	// open the same store.
	Lock(name string) (io.Closer, error)
}

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("vfs: locked by another process")

// OS is the operating system's file system.
var OS FS = osFS{}

type osFS struct{}

type osFile struct{ *os.File }

func (f osFile) Sync() error { return fullSync(f.File) }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (osFS) Create(name string) (File, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) OpenAppend(name string) (File, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) Open(name string) (File, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) Remove(name string) error             { return os.Remove(name) }
func (osFS) Rename(oldname, newname string) error { return os.Rename(oldname, newname) }
func (osFS) MkdirAll(dir string) error            { return os.MkdirAll(dir, 0o700) }

func (osFS) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	slices.Sort(names)
	return names, nil
}

func (osFS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fullSync(d)
}

func (osFS) Lock(name string) (io.Closer, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil // closing the file releases the lock
}

// Join joins path elements (filepath.Join; here so callers need only
// this package).
func Join(elem ...string) string { return filepath.Join(elem...) }
