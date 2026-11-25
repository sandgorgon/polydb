package transfer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/transfer"
	"github.com/sandgorgon/polydb/value"
)

// BenchmarkExportImport exports and imports 200,000 small entities and
// 100 MiB of large values on disk (set TMPDIR to a real disk), reporting
// speed and the most live heap either takes.
func BenchmarkExportImport(b *testing.B) {
	for _, format := range []transfer.Format{transfer.JSONL, transfer.Native} {
		b.Run(string(format), func(b *testing.B) {
			dir := b.TempDir()
			eng, err := engine.Open(engine.Options{Dir: filepath.Join(dir, "data"), HistorySweep: -1, StatsRefresh: -1})
			if err != nil {
				b.Fatal(err)
			}
			defer eng.Close()
			db, _ := eng.CreateDatabase("src")
			db.Update(func(tx *engine.Tx) error {
				_, err := tx.DefineKind(model.Kind{Name: "E", Fields: []model.FieldDef{
					{Name: "n", Type: value.TypeInt}, {Name: "s", Type: value.TypeText},
					{Name: "d", Type: value.MustParseType("bytes?")}}})
				return err
			})
			for i := 0; i < 200_000; i += 1000 {
				if err := db.Update(func(tx *engine.Tx) error {
					for j := i; j < i+1000; j++ {
						f := engine.Fields{"n": value.Int(j), "s": value.MustText(strings.Repeat("x", 60))}
						if j%2000 == 0 {
							data := make([]byte, 1<<20)
							for k := range data {
								data[k] = byte(j + k*7)
							}
							f["d"] = value.BytesOf(data, "")
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
			b.ResetTimer()
			for i := range b.N {
				path := filepath.Join(dir, fmt.Sprintf("x%d", i))
				f, _ := os.Create(path)
				peak := peakHeap()
				began := time.Now()
				st, err := transfer.ExportDatabase(context.Background(), db, f, format)
				f.Close()
				if err != nil {
					b.Fatal(err)
				}
				exp, expPeak := time.Since(began), peak()
				dst, _ := eng.CreateDatabase(fmt.Sprintf("dst%d", i))
				f, _ = os.Open(path)
				peak = peakHeap()
				began = time.Now()
				if _, err := transfer.ImportDatabase(context.Background(), dst, f, transfer.ImportOptions{}); err != nil {
					b.Fatal(err)
				}
				f.Close()
				imp, impPeak := time.Since(began), peak()
				mib := float64(st.Bytes) / (1 << 20)
				b.ReportMetric(mib, "file-MiB")
				b.ReportMetric(mib/exp.Seconds(), "export-MiB/s")
				b.ReportMetric(mib/imp.Seconds(), "import-MiB/s")
				b.ReportMetric(expPeak, "export-peak-MiB")
				b.ReportMetric(impPeak, "import-peak-MiB")
			}
		})
	}
}

// peakHeap samples the live heap until the returned function gives the
// peak above the start, in MiB.
func peakHeap() func() float64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 { metrics.Read(s); return s[0].Value.Uint64() }
	base, peak := read(), uint64(0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				peak = max(peak, read())
			}
		}
	})
	return func() float64 {
		close(stop)
		wg.Wait()
		return float64(max(peak, base)-base) / (1 << 20)
	}
}
