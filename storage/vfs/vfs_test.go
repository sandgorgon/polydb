package vfs

import (
	"io"
	"math/rand/v2"
	"testing"
)

func read(t *testing.T, fs FS, name string) string {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		return "<missing>"
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func write(t *testing.T, fs FS, name, data string, sync bool) {
	t.Helper()
	f, err := fs.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte(data))
	if sync {
		f.Sync()
	}
	f.Close()
}

func TestMemCrash(t *testing.T) {
	m := NewMem()
	m.MkdirAll("/d")
	write(t, m, "/d/synced", "durable", true)
	write(t, m, "/d/unsynced", "lost", false)
	m.SyncDir("/d")
	write(t, m, "/d/no-dir-sync", "lost too", true) // its name isn't durable

	// Appended data survives only up to the last Sync.
	f, _ := m.OpenAppend("/d/synced")
	f.Write([]byte(" + tail"))
	f.Close()

	c := m.Crash(nil)
	if got := read(t, c, "/d/synced"); got != "durable" {
		t.Errorf("synced file: %q", got)
	}
	if got := read(t, c, "/d/unsynced"); got != "" {
		t.Errorf("unsynced file: %q", got)
	}
	if got := read(t, c, "/d/no-dir-sync"); got != "<missing>" {
		t.Errorf("file whose name wasn't synced: %q", got)
	}

	// A torn write keeps part of the unsynced tail.
	saw := map[string]bool{}
	for i := range 50 {
		got := read(t, m.Crash(rand.New(rand.NewPCG(uint64(i), 0))), "/d/synced")
		saw[got] = true
		if len(got) < len("durable") || got[:7] != "durable" {
			t.Fatalf("torn file lost synced data: %q", got)
		}
	}
	if len(saw) < 3 {
		t.Errorf("tearing produced only %v", saw)
	}

	// Renames and removals are durable only after SyncDir.
	m.Rename("/d/synced", "/d/renamed")
	m.Remove("/d/unsynced")
	c = m.Crash(nil)
	if read(t, c, "/d/synced") != "durable" || read(t, c, "/d/renamed") != "<missing>" || read(t, c, "/d/unsynced") != "" {
		t.Error("rename or remove survived without SyncDir")
	}
	m.SyncDir("/d")
	c = m.Crash(nil)
	if read(t, c, "/d/renamed") != "durable" || read(t, c, "/d/synced") != "<missing>" || read(t, c, "/d/unsynced") != "<missing>" {
		t.Error("rename or remove lost after SyncDir")
	}
	names, _ := m.List("/d")
	if len(names) != 2 {
		t.Errorf("List: %v", names)
	}
	if _, err := m.Create("/nodir/x"); err == nil {
		t.Error("created a file in a missing directory")
	}
	l, err := m.Lock("/d/LOCK")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Lock("/d/LOCK"); err != ErrLocked {
		t.Errorf("second lock: %v", err)
	}
	l.Close()
	if _, err := m.Lock("/d/LOCK"); err != nil {
		t.Errorf("lock after release: %v", err)
	}
}

func TestMemRemoveAll(t *testing.T) {
	m := NewMem()
	m.MkdirAll("/d/sub")
	write(t, m, "/d/sub/f", "data", true)
	m.SyncDir("/d/sub")
	write(t, m, "/d/keep", "x", true)
	m.SyncDir("/d")
	m.RemoveAll("/d/sub")
	if read(t, m, "/d/sub/f") != "<missing>" || read(t, m, "/d/keep") != "x" {
		t.Error("RemoveAll removed the wrong things")
	}
	if names, _ := m.List("/d"); len(names) != 1 {
		t.Errorf("List after RemoveAll: %v", names)
	}
	// Not durable: a crash brings the directory back with its file.
	c := m.Crash(nil)
	if read(t, c, "/d/sub/f") != "data" {
		t.Error("RemoveAll was durable without a SyncDir")
	}
	if names, err := c.List("/d/sub"); err != nil || len(names) != 1 {
		t.Errorf("the restored directory lists %v (%v)", names, err)
	}
	// Syncing the parent makes the removal durable.
	m.SyncDir("/d")
	c = m.Crash(nil)
	if read(t, c, "/d/sub/f") != "<missing>" || read(t, c, "/d/keep") != "x" {
		t.Error("the removal didn't survive a SyncDir of the parent")
	}
}
