package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// openDisk opens an engine on fs at dir.
func openDisk(t *testing.T, fs vfs.FS, dir string) *Engine {
	t.Helper()
	return must(Open(Options{Dir: dir, FS: fs, Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
}

// accounts makes n accounts holding 100 each, plus a file with a large
// value, and returns their IDs.
func accounts(t *testing.T, db *DB, n int) []value.ID {
	t.Helper()
	update(t, db, func(tx *Tx) error {
		if _, err := tx.DefineKind(model.Kind{Name: "Acct", History: model.History{On: true},
			Fields: []model.FieldDef{fd("n", "int"), fd("bal", "int"), fd("doc", "bytes?")}}); err != nil {
			return err
		}
		_, err := tx.DefineIndex("acct_n", "Acct", false, false, model.IndexKey{Field: "n"})
		return err
	})
	ids := make([]value.ID, n)
	for i := range ids {
		f := Fields{"n": value.Int(i), "bal": value.Int(100)}
		if i%10 == 0 {
			f["doc"] = blobBytes(i, 40<<10+i)
		}
		ids[i] = create(t, db, "Acct", f)
	}
	return ids
}

// total sums the balances, checking the large values on the way.
func total(t *testing.T, db *DB) (sum int64, count int) {
	t.Helper()
	err := db.View(func(tx *Tx) error {
		return tx.Entities("Acct", func(id value.ID) bool {
			e, err := tx.Get(id)
			if err != nil {
				t.Error(err)
				return false
			}
			f, _ := e.Field("bal")
			sum += int64(f.Value.(value.Int))
			count++
			if d, ok := e.Field("doc"); ok && d.Value != nil {
				n, _ := e.Field("n")
				i := int(n.Value.(value.Int))
				sameVal(t, d.Value, blobBytes(i, 40<<10+i))
			}
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return sum, count
}

// A backup taken while transfers run is the database as of one commit:
// money is neither made nor lost, and the restored database works like
// the original, before and after a restart.
func TestBackupRestore(t *testing.T) {
	fs := vfs.NewMem()
	eng := openDisk(t, fs, "/data")
	db := must(eng.CreateDatabase("bank"))
	ids := accounts(t, db, 200)

	stop := atomic.Bool{}
	var wg sync.WaitGroup
	var transfers atomic.Int64
	for w := range 4 {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				a, b := ids[(i*7+w)%len(ids)], ids[(i*13+w*3+1)%len(ids)]
				if a == b {
					continue
				}
				err := db.Retry(10, func(tx *Tx) error {
					ea, err := tx.Get(a)
					if err != nil {
						return err
					}
					eb, err := tx.Get(b)
					if err != nil {
						return err
					}
					fa, _ := ea.Field("bal")
					fb, _ := eb.Field("bal")
					if err := tx.Set(a, Fields{"bal": fa.Value.(value.Int) - 1}); err != nil {
						return err
					}
					return tx.Set(b, Fields{"bal": fb.Value.(value.Int) + 1})
				})
				if err != nil && !IsConflict(err) {
					t.Error(err)
					return
				}
				transfers.Add(1)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	info, err := db.Backup(context.Background(), nil, "/backups/one")
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if transfers.Load() == 0 {
		t.Fatal("no transfers ran")
	}
	if info.Database != "bank" || info.Keys == 0 || len(info.Files) < 3 || info.Bytes() == 0 {
		t.Fatalf("backup info %+v", info)
	}
	if _, err := ReadBackup(fs, "/backups/one"); err != nil {
		t.Fatal(err)
	}

	restored, _, err := eng.RestoreDatabase(context.Background(), "bank2", nil, "/backups/one")
	if err != nil {
		t.Fatal(err)
	}
	if sum, n := total(t, restored); sum != 200*100 || n != 200 {
		t.Fatalf("restored: %d accounts holding %d", n, sum)
	}
	// It takes writes, keeps its history and indexes, and survives a
	// restart.
	update(t, restored, func(tx *Tx) error {
		e, err := tx.Get(ids[0])
		if err != nil {
			return err
		}
		f, _ := e.Field("bal")
		return tx.Set(ids[0], Fields{"bal": f.Value.(value.Int) + 900})
	})
	if err := restored.VerifyIndexes(); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng = openDisk(t, fs, "/data")
	defer eng.Close()
	restored = must(eng.Database("bank2"))
	if sum, n := total(t, restored); sum != 200*100+900 || n != 200 {
		t.Fatalf("after restart: %d accounts holding %d", n, sum)
	}
	if sum, _ := total(t, must(eng.Database("bank"))); sum != 200*100 {
		t.Fatalf("original: %d", sum)
	}

	// A restore doesn't replace a database, and a backup doesn't
	// overwrite one.
	if _, _, err := eng.RestoreDatabase(context.Background(), "bank2", nil, "/backups/one"); CodeOf(err) != CodeConstraint {
		t.Errorf("restore over a database: %v", err)
	}
	if _, err := restored.Backup(context.Background(), nil, "/backups/one"); CodeOf(err) != CodeConstraint {
		t.Errorf("backup over a backup: %v", err)
	}
}

// A damaged or unfinished backup isn't restored, and leaves nothing
// behind.
func TestRestoreChecks(t *testing.T) {
	fs := vfs.NewMem()
	eng := openDisk(t, fs, "/data")
	defer eng.Close()
	db := must(eng.CreateDatabase("src"))
	accounts(t, db, 30)
	info := must(db.Backup(context.Background(), nil, "/b"))
	before := must(fs.List("/data/databases"))

	// Flip a byte in the middle of the largest file.
	var big BackupFile
	for _, f := range info.Files {
		if f.Size > big.Size {
			big = f
		}
	}
	data := readAll(t, fs, "/b/"+big.Path)
	data[len(data)/2] ^= 0xff
	writeFile(t, fs, "/b/"+big.Path, data)
	if _, _, err := eng.RestoreDatabase(context.Background(), "x", nil, "/b"); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged backup: %v", err)
	}
	if _, err := eng.Database("x"); err == nil {
		t.Fatal("damaged backup registered")
	}
	if after := must(fs.List("/data/databases")); len(after) != len(before) {
		t.Fatalf("restore left %v, had %v", after, before)
	}
	// Without its BACKUP file a directory is an unfinished backup.
	fs.Remove("/b/BACKUP")
	if _, _, err := eng.RestoreDatabase(context.Background(), "x", nil, "/b"); CodeOf(err) != CodeNotFound {
		t.Fatalf("unfinished backup: %v", err)
	}
	// A cancelled backup removes what it wrote.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.Backup(ctx, nil, "/c"); CodeOf(err) != CodeLimit {
		t.Fatalf("cancelled backup: %v", err)
	}
	if names, err := fs.List("/c"); err == nil && len(names) > 0 {
		t.Fatalf("cancelled backup left %v", names)
	}
	// Statement paths stay inside the backup directory.
	for _, p := range []string{"", "/abs", "../up", "a/../../up", "."} {
		if _, err := eng.BackupPath(p); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if p, err := eng.BackupPath("nightly/mon"); err != nil || p != "/data/backups/nightly/mon" {
		t.Errorf("path: %q %v", p, err)
	}
}

// An in-memory engine backs up to and restores from any file system.
func TestBackupInMemory(t *testing.T) {
	db := newDB(t)
	accounts(t, db, 50)
	fs := vfs.NewMem()
	if _, err := db.Backup(context.Background(), fs, "/b"); err != nil {
		t.Fatal(err)
	}
	r, _, err := db.eng.RestoreDatabase(context.Background(), "copy", fs, "/b")
	if err != nil {
		t.Fatal(err)
	}
	if sum, n := total(t, r); sum != 50*100 || n != 50 {
		t.Fatalf("restored: %d accounts holding %d", n, sum)
	}
	if _, err := db.eng.BackupPath("x"); CodeOf(err) != CodeUnsupported {
		t.Errorf("backup path without a backup directory: %v", err)
	}
}

func readAll(t *testing.T, fs vfs.FS, name string) []byte {
	t.Helper()
	f := must(fs.Open(name))
	defer f.Close()
	size := must(f.Size())
	b := make([]byte, size)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, fs vfs.FS, name string, data []byte) {
	t.Helper()
	f := must(fs.Create(name))
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// BenchmarkBackup backs up and restores a database of 200,000 small
// entities and 100 MiB of large values, on disk (set TMPDIR to a real
// disk: /tmp is often in memory).
func BenchmarkBackup(b *testing.B) {
	eng := must(Open(Options{Dir: b.TempDir(), Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
	defer eng.Close()
	db := must(eng.CreateDatabase("bench"))
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "E", Fields: []model.FieldDef{fd("n", "int"), fd("s", "text"), fd("d", "bytes?")}})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200_000; i += 1000 {
		if err := db.Update(func(tx *Tx) error {
			for j := i; j < i+1000; j++ {
				f := Fields{"n": value.Int(j), "s": txt(strings.Repeat("x", 60))}
				if j%2000 == 0 {
					f["d"] = blobBytes(j, 1<<20)
				}
				if _, err := tx.Create("E", f); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	dir := b.TempDir()
	b.ResetTimer()
	for i := range b.N {
		began := time.Now()
		info, err := db.Backup(context.Background(), nil, fmt.Sprintf("%s/b%d", dir, i))
		if err != nil {
			b.Fatal(err)
		}
		took := time.Since(began)
		began = time.Now()
		if _, _, err := eng.RestoreDatabase(context.Background(), fmt.Sprintf("r%d", i), nil, fmt.Sprintf("%s/b%d", dir, i)); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(info.Bytes())/(1<<20), "backup-MiB")
		b.ReportMetric(float64(info.Bytes())/(1<<20)/took.Seconds(), "backup-MiB/s")
		b.ReportMetric(float64(info.Bytes())/(1<<20)/time.Since(began).Seconds(), "restore-MiB/s")
	}
}
