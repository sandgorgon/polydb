package transfer_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/session"
	"github.com/sandgorgon/polydb/internal/transfer"
	"github.com/sandgorgon/polydb/value"
)

// TestBenchReport (POLYDB_BENCH=<entities>) measures the main operations
// on disk with the default settings, on a database bigger than the block
// cache: loading, point reads, index and scan queries, large values,
// backup and restore, export and import, and a background alter
// (plan.md, Phase 6 step 9). Set TMPDIR to a real disk.
func TestBenchReport(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("POLYDB_BENCH"))
	if n == 0 {
		t.Skip("set POLYDB_BENCH to the number of entities")
	}
	ctx := context.Background()
	dir := t.TempDir()
	eng, err := engine.Open(engine.Options{Dir: filepath.Join(dir, "data"), HistorySweep: -1, StatsRefresh: -1,
		PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	db, _ := eng.CreateDatabase("bench")
	s := session.New(eng, security.AdminUser)
	defer s.Close()
	if _, err := s.Exec(ctx, `use bench; define kind P { n: int, tag: int, name: text, body: text, data: bytes? };
		define index p_n on P(n)`, nil); err != nil {
		t.Fatal(err)
	}
	var report []string
	row := func(what string, count int, d time.Duration, unit string) {
		report = append(report, fmt.Sprintf("%-44s %12.0f %s/s  (%d in %s)", what, float64(count)/d.Seconds(), unit, count,
			d.Round(time.Millisecond)))
	}
	body := strings.Repeat("lorem ipsum dolor sit amet ", 8)

	// Load, 4 writers, durable commits of 1,000.
	began := time.Now()
	var wg sync.WaitGroup
	ids := make([]value.ID, n)
	for w := range 4 {
		wg.Go(func() {
			for i := w * 1000; i < n; i += 4000 {
				err := db.Update(func(tx *engine.Tx) error {
					for j := i; j < min(i+1000, n); j++ {
						id, err := tx.Create("P", engine.Fields{"n": value.Int(j), "tag": value.Int(j % 100),
							"name": value.MustText("name " + strconv.Itoa(j)), "body": value.MustText(body)})
						if err != nil {
							return err
						}
						ids[j] = id
					}
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	row("load (4 writers, durable, 1,000 per commit)", n, time.Since(began), "entities")
	m, _ := db.StoreMetrics()
	var size uint64
	for _, l := range m.Levels {
		size += l.Bytes
	}
	report = append(report, fmt.Sprintf("%-44s %12d MiB on disk, block cache 64 MiB", "store size", size>>20))

	rng := rand.New(rand.NewPCG(1, 2))
	const reads = 20000
	began = time.Now()
	db.View(func(tx *engine.Tx) error {
		for range reads {
			if _, err := tx.Get(ids[rng.IntN(n)]); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	row("point reads of whole entities (random)", reads, time.Since(began), "reads")

	query := func(what, q string, times int, args func() map[string]value.Value) {
		began := time.Now()
		for range times {
			if _, err := s.Exec(ctx, q, args()); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		row(what, times, time.Since(began), "queries")
	}
	query("index equality query (random n)", `find P p | where p.n = $n | show p.name`, 5000,
		func() map[string]value.Value { return map[string]value.Value{"n": value.Int(rng.IntN(n))} })
	query("index range query, 100 rows", `find P p | where p.n >= $a and p.n < $a + 100 | show count()`, 1000,
		func() map[string]value.Value { return map[string]value.Value{"a": value.Int(rng.IntN(n - 100))} })
	began = time.Now()
	if _, err := s.Exec(ctx, `find P p | where p.tag = 7 | show count()`, nil); err != nil {
		t.Fatal(err)
	}
	row("full scan with a filter (no index)", n, time.Since(began), "entities")

	// Large values: 100 × 1 MiB, durable, then read back.
	const blobs = 100
	data := make([]byte, 1<<20)
	var bids []value.ID
	began = time.Now()
	for i := range blobs {
		data[0] = byte(i)
		data[1] = byte(i >> 8)
		db.Update(func(tx *engine.Tx) error {
			id, err := tx.Create("P", engine.Fields{"n": value.Int(-i - 1), "tag": value.Int(0), "name": value.MustText("blob"),
				"body": value.MustText(""), "data": value.BytesOf(bytes.Clone(data), "")})
			bids = append(bids, id)
			return err
		})
	}
	row("large values written (1 MiB, durable)", blobs<<20, time.Since(began), "bytes")
	began = time.Now()
	db.View(func(tx *engine.Tx) error {
		for _, id := range bids {
			e, _ := tx.Get(id)
			f, _ := e.Field("data")
			if _, err := value.Load(f.Value); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	row("large values read", blobs<<20, time.Since(began), "bytes")

	// Backup and restore.
	began = time.Now()
	info, err := db.Backup(ctx, nil, filepath.Join(dir, "backup"))
	if err != nil {
		t.Fatal(err)
	}
	row("backup", int(info.Bytes()), time.Since(began), "bytes")
	began = time.Now()
	if _, _, err := eng.RestoreDatabase(ctx, "restored", nil, filepath.Join(dir, "backup")); err != nil {
		t.Fatal(err)
	}
	row("restore", int(info.Bytes()), time.Since(began), "bytes")

	// Export and import (native).
	f, _ := os.Create(filepath.Join(dir, "export"))
	began = time.Now()
	st, err := transfer.ExportDatabase(ctx, db, f, transfer.Native)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	row("export (native)", int(st.Entities), time.Since(began), "entities")
	dst, _ := eng.CreateDatabase("imported")
	f, _ = os.Open(filepath.Join(dir, "export"))
	began = time.Now()
	if _, err := transfer.ImportDatabase(ctx, dst, f, transfer.ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	row("import (native)", int(st.Entities), time.Since(began), "entities")

	// A background alter of every entity.
	began = time.Now()
	if err := db.Update(func(tx *engine.Tx) error {
		return tx.AlterKind("P", func(k *model.Kind) error {
			k.Fields[1].Type = value.TypeDecimal
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitAlters(ctx); err != nil {
		t.Fatal(err)
	}
	row("background alter (int to decimal)", n+blobs, time.Since(began), "entities")

	t.Logf("benchmark report, %d entities:\n%s", n, strings.Join(report, "\n"))
}
