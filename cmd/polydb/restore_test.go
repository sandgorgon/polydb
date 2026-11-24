package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/engine"
)

// polydb restore puts a backup into a stopped server's data directory.
func TestRestoreCommand(t *testing.T) {
	home := isolate(t)
	data, backup := filepath.Join(home, "data"), filepath.Join(home, "bk")
	eng, err := engine.Open(engine.Options{Dir: data, HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		t.Fatal(err)
	}
	db, err := eng.CreateDatabase("shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(context.Background(), nil, backup); err != nil {
		t.Fatal(err)
	}
	// While the server (here, the engine) runs, the data directory is
	// locked.
	if _, errOut, code := runCLI(t, "", "restore", "--data-dir", data, "--database", "copy", backup); code == 0 ||
		!strings.Contains(errOut, "running") {
		t.Fatalf("restore into a running server: %d %s", code, errOut)
	}
	eng.Close()
	out, errOut, code := runCLI(t, "", "restore", "--data-dir", data, "--database", "copy", backup)
	if code != 0 || !strings.Contains(out, "restored database copy") {
		t.Fatalf("restore: %d %s %s", code, out, errOut)
	}
	// Without --database, the backed-up name is taken, which exists.
	if _, errOut, code := runCLI(t, "", "restore", "--data-dir", data, backup); code == 0 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("restore over shop: %d %s", code, errOut)
	}
	eng, err = engine.Open(engine.Options{Dir: data, HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if got := eng.Databases(); strings.Join(got, ",") != "copy,shop" {
		t.Fatalf("databases %v", got)
	}
}
