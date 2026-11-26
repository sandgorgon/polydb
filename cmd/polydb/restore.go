package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
)

// restoreConfig is what restore needs, parsed from flags.
type restoreConfig struct {
	dataDir  string
	database string
	from     string
}

// restoreCmd restores a database from a backup into a stopped server's
// data directory. A running server restores with restore database.
func restoreCmd(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(errOut)
	c := restoreConfig{}
	fs.StringVar(&c.dataDir, "data-dir", dataDir(), "the server's data directory")
	fs.StringVar(&c.database, "database", "", "name of the database to create (default: the backed-up database's name)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(errOut, "usage: polydb restore [--data-dir dir] [--database name] backup-dir")
		return 2
	}
	c.from = fs.Arg(0)
	if err := restoreData(ctx, c, out); err != nil {
		fmt.Fprintln(errOut, "polydb restore:", err)
		return 1
	}
	return 0
}

func restoreData(ctx context.Context, c restoreConfig, out io.Writer) error {
	from, err := filepath.Abs(c.from)
	if err != nil {
		return err
	}
	info, err := engine.ReadBackup(vfs.OS, from)
	if err != nil {
		return err
	}
	name := c.database
	if name == "" {
		name = info.Database
	}
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	eng, err := engine.Open(engine.Options{Dir: c.dataDir, HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		return fmt.Errorf("%w (is the server running? use restore database in a shell instead)", err)
	}
	defer eng.Close()
	if _, info, err = eng.RestoreDatabase(ctx, name, vfs.OS, from); err != nil {
		return err
	}
	fmt.Fprintf(out, "restored database %s from %s (backup of %s taken %s): %d keys, %d files, %d bytes\n",
		name, from, info.Database, info.Created.Format("2006-01-02 15:04:05Z07:00"), info.Keys, len(info.Files), info.Bytes())
	return nil
}
