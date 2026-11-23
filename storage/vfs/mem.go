package vfs

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Mem is an in-memory file system that models what survives a crash:
//
//   - a file's data survives only up to its last Sync;
//   - a created, removed or renamed name survives only after SyncDir of
//     its directory.
//
// Crash returns the file system as it would be after a power cut.
type Mem struct {
	mu      sync.Mutex
	files   map[string]*memNode // the current names
	durable map[string]*memNode // the names as of each directory's last SyncDir
	dirs    map[string]bool
	locks   map[string]bool
}

type memNode struct {
	data   []byte
	synced []byte // the data as of the last Sync
}

// NewMem returns an empty in-memory file system.
func NewMem() *Mem {
	return &Mem{files: map[string]*memNode{}, durable: map[string]*memNode{}, dirs: map[string]bool{".": true, "/": true},
		locks: map[string]bool{}}
}

func clean(name string) string { return filepath.Clean(name) }

func notExist(op, name string) error { return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist} }

func (m *Mem) checkDir(op, name string) error {
	if !m.dirs[filepath.Dir(name)] {
		return notExist(op, filepath.Dir(name))
	}
	return nil
}

func (m *Mem) Create(name string) (File, error) {
	name = clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkDir("create", name); err != nil {
		return nil, err
	}
	n := &memNode{}
	m.files[name] = n
	return &memFile{m: m, n: n, writable: true}, nil
}

func (m *Mem) OpenAppend(name string) (File, error) {
	name = clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	return &memFile{m: m, n: n, writable: true, appending: true, pos: int64(len(n.data))}, nil
}

func (m *Mem) Open(name string) (File, error) {
	name = clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	return &memFile{m: m, n: n}, nil
}

func (m *Mem) Remove(name string) error {
	name = clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[name]; !ok {
		return notExist("remove", name)
	}
	delete(m.files, name)
	return nil
}

func (m *Mem) Rename(oldname, newname string) error {
	oldname, newname = clean(oldname), clean(newname)
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.files[oldname]
	if !ok {
		return notExist("rename", oldname)
	}
	if err := m.checkDir("rename", newname); err != nil {
		return err
	}
	delete(m.files, oldname)
	m.files[newname] = n
	return nil
}

func (m *Mem) List(dir string) ([]string, error) {
	dir = clean(dir)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirs[dir] {
		return nil, notExist("list", dir)
	}
	var out []string
	for name := range m.files {
		if filepath.Dir(name) == dir {
			out = append(out, filepath.Base(name))
		}
	}
	for d := range m.dirs {
		if d != dir && filepath.Dir(d) == dir {
			out = append(out, filepath.Base(d))
		}
	}
	slices.Sort(out)
	return out, nil
}

func (m *Mem) MkdirAll(dir string) error {
	dir = clean(dir)
	m.mu.Lock()
	defer m.mu.Unlock()
	for d := dir; !m.dirs[d]; d = filepath.Dir(d) {
		m.dirs[d] = true // directories themselves are modelled as durable
	}
	return nil
}

func (m *Mem) SyncDir(dir string) error {
	dir = clean(dir)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirs[dir] {
		return notExist("sync", dir)
	}
	for name := range m.durable {
		if filepath.Dir(name) == dir {
			delete(m.durable, name)
		}
	}
	for name, n := range m.files {
		if filepath.Dir(name) == dir {
			m.durable[name] = n
		}
	}
	return nil
}

type memLock struct {
	m    *Mem
	name string
}

func (l *memLock) Close() error {
	l.m.mu.Lock()
	delete(l.m.locks, l.name)
	l.m.mu.Unlock()
	return nil
}

func (m *Mem) Lock(name string) (io.Closer, error) {
	name = clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[name] {
		return nil, ErrLocked
	}
	m.locks[name] = true
	return &memLock{m, name}, nil
}

// Crash returns the file system as a power cut would leave it: only
// durable names, each with only its synced data. If tear is set, a file
// that had unsynced data appended keeps a random part of it, as a write
// cut off mid-way would.
func (m *Mem) Crash(tear *rand.Rand) *Mem {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := NewMem()
	for d := range m.dirs {
		out.dirs[d] = true
	}
	nodes := map[*memNode]*memNode{} // hard links stay shared
	for name, n := range m.durable {
		c, ok := nodes[n]
		if !ok {
			data := slices.Clone(n.synced)
			if tear != nil && len(n.data) > len(n.synced) && strings.HasPrefix(string(n.data), string(n.synced)) {
				extra := n.data[len(n.synced):]
				data = append(data, extra[:tear.IntN(len(extra)+1)]...)
			}
			c = &memNode{data: data, synced: slices.Clone(data)}
			nodes[n] = c
		}
		out.files[name] = c
		out.durable[name] = c
	}
	return out
}

// memFile is an open file of a Mem.
type memFile struct {
	m         *Mem
	n         *memNode
	pos       int64
	writable  bool
	appending bool
	closed    bool
}

var errClosed = errors.New("vfs: file is closed")

func (f *memFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.pos)
	f.pos += int64(n)
	return n, err
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return 0, errClosed
	}
	if off >= int64(len(f.n.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.n.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) Write(p []byte) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return 0, errClosed
	}
	if !f.writable {
		return 0, errors.New("vfs: file is read-only")
	}
	if f.appending {
		f.pos = int64(len(f.n.data))
	}
	end := f.pos + int64(len(p))
	if end > int64(len(f.n.data)) {
		f.n.data = append(f.n.data, make([]byte, end-int64(len(f.n.data)))...)
	}
	copy(f.n.data[f.pos:], p)
	f.pos = end
	return len(p), nil
}

func (f *memFile) Sync() error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.closed {
		return errClosed
	}
	f.n.synced = slices.Clone(f.n.data)
	return nil
}

func (f *memFile) Size() (int64, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return int64(len(f.n.data)), nil
}

func (f *memFile) Truncate(size int64) error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if size < int64(len(f.n.data)) {
		f.n.data = f.n.data[:size]
	}
	return nil
}

func (f *memFile) Close() error {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	f.closed = true
	return nil
}
