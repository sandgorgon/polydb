//go:build linux || darwin

package vfs

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Map maps the whole file read-only (Mapper).
func (f osFile) Map() ([]byte, func() error, error) {
	size, err := f.Size()
	if err != nil {
		return nil, nil, err
	}
	if size == 0 {
		return nil, nil, errors.New("vfs: can't map an empty file")
	}
	if int64(int(size)) != size {
		return nil, nil, errors.New("vfs: file too large to map")
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return unix.Munmap(data) }, nil
}
