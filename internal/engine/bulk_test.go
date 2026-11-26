package engine

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage/wal"
	"github.com/sandgorgon/polydb/value"
)

// TestBulkLoadReport (POLYDB_BULK=<entities>) loads entities on disk with
// the default store settings and reports throughput, write stalls and
// write amplification, to judge compaction (plan.md, Phase 6 step 3).
func TestBulkLoadReport(t *testing.T) {
	total, _ := strconv.Atoi(os.Getenv("POLYDB_BULK"))
	if total == 0 {
		t.Skip("set POLYDB_BULK to the number of entities to load")
	}
	words := strings.Fields("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega")
	for _, c := range []struct {
		name       string
		durability wal.Durability
		writers    int
		text       bool
	}{
		{"durable, 1 writer", wal.Durable, 1, false},
		{"durable, 4 writers", wal.Durable, 4, false},
		{"relaxed, 4 writers", wal.Relaxed, 4, false},
		{"relaxed, 4 writers, text index", wal.Relaxed, 4, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := Options{Dir: t.TempDir(), Durability: c.durability, StatsRefresh: -1, HistorySweep: -1}
			if n, _ := strconv.Atoi(os.Getenv("POLYDB_BULK_L0STOP")); n > 0 {
				o.Storage.L0StopWrites = n
			}
			eng, err := Open(o)
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			db, err := eng.CreateDatabase("bulk")
			if err != nil {
				t.Fatal(err)
			}
			update(t, db, func(tx *Tx) error {
				k := model.Kind{Name: "Item", Fields: []model.FieldDef{fd("sku", "text"), fd("n", "int"), fd("body", "text")}}
				k.Fields[0].Unique = true
				if _, err := tx.DefineKind(k); err != nil {
					return err
				}
				if _, err := tx.DefineIndex("item_n", "Item", false, false, model.IndexKey{Field: "n"}); err != nil {
					return err
				}
				if c.text {
					_, err := tx.DefineIndex("item_body", "Item", false, true, model.IndexKey{Field: "body"})
					return err
				}
				return nil
			})
			const batch = 1000
			var wg sync.WaitGroup
			var mu sync.Mutex
			next := 0
			began := time.Now()
			for w := range c.writers {
				wg.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(w), 1))
					for {
						mu.Lock()
						start := next
						next += batch
						mu.Unlock()
						if start >= total {
							return
						}
						err := db.Update(func(tx *Tx) error {
							for i := start; i < min(start+batch, total); i++ {
								var b strings.Builder
								for range 30 {
									b.WriteString(words[rng.IntN(len(words))] + " ")
								}
								_, err := tx.Create("Item", Fields{"sku": txt(fmt.Sprintf("sku-%09d", i)),
									"n": value.Int(int64(rng.IntN(1000))), "body": txt(b.String())})
								if err != nil {
									return err
								}
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
			took := time.Since(began)
			m, _ := db.StoreMetrics()
			var tables []string
			for l, lv := range m.Levels {
				if lv.Tables > 0 {
					tables = append(tables, fmt.Sprintf("L%d %d/%dMB", l, lv.Tables, lv.Bytes>>20))
				}
			}
			t.Logf("%d entities in %v: %.0f/s; flushed %dMB, compacted %dMB in %d compactions (write amplification %.1f); %d stalls, %v stalled; %s",
				total, took.Round(time.Millisecond), float64(total)/took.Seconds(), m.FlushBytes>>20, m.CompactBytes>>20, m.Compactions,
				float64(m.FlushBytes+m.CompactBytes)/float64(max(m.FlushBytes, 1)), m.Stalls, m.StallTime.Round(time.Millisecond), strings.Join(tables, ", "))
		})
	}
}
