package vfs

import "os"

func fullSync(f *os.File) error { return f.Sync() }
