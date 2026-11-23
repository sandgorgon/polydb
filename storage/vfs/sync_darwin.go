package vfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// fullSync uses F_FULLFSYNC: on macOS, fsync only reaches the drive's
// cache, not stable storage.
func fullSync(f *os.File) error {
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return f.Sync() // file systems that don't support it
	}
	return nil
}
