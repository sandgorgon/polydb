package engine

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage/lsm"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// With a block cache far smaller than the data, reads still give the
// right answers: point reads, index ranges and full scans all go through
// evictions.
func TestLargerThanCache(t *testing.T) {
	o := Options{Dir: "/data", FS: vfs.NewMem(), Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1,
		Storage: lsm.Options{CacheSize: 64 << 10, NoMmap: true, MemtableSize: 512 << 10, TableSize: 256 << 10}}
	eng := must(Open(o))
	defer eng.Close()
	db := must(eng.CreateDatabase("d"))
	update(t, db, func(tx *Tx) error {
		if _, err := tx.DefineKind(model.Kind{Name: "E", Fields: []model.FieldDef{fd("n", "int"), fd("pad", "text")}}); err != nil {
			return err
		}
		_, err := tx.DefineIndex("e_n", "E", false, false, model.IndexKey{Field: "n"})
		return err
	})
	const n = 30000
	pad := txt("padding padding padding padding padding padding padding padding")
	ids := make([]value.ID, n)
	for i := 0; i < n; i += 1000 {
		update(t, db, func(tx *Tx) error {
			for j := i; j < i+1000; j++ {
				id, err := tx.Create("E", Fields{"n": value.Int(j), "pad": pad})
				if err != nil {
					return err
				}
				ids[j] = id
			}
			return nil
		})
	}
	m, _ := db.StoreMetrics()
	var tables int
	for _, l := range m.Levels {
		tables += l.Tables
	}
	t.Logf("%d tables for %d entities, 64 KiB of cache", tables, n)

	rng := rand.New(rand.NewPCG(1, 1))
	err := db.View(func(tx *Tx) error {
		for range 2000 {
			j := rng.IntN(n)
			_, v, _, err := tx.EntityField(ids[j], "n")
			if err != nil {
				return err
			}
			if v != value.Int(j) {
				t.Fatalf("entity %d reads n = %v", j, v)
			}
		}
		ix, _ := tx.Catalog().Index("e_n")
		for range 50 {
			lo := rng.IntN(n - 500)
			klo, _ := value.Key(value.Int(lo))
			khi, _ := value.Key(value.Int(lo + 500))
			got := 0
			err := tx.ScanIndex(ix, klo, khi, func(id value.ID, _ []byte) bool { got++; return true })
			if err != nil {
				return err
			}
			if got != 500 {
				t.Fatalf("index range [%d, %d) found %d", lo, lo+500, got)
			}
		}
		sum := 0
		k, _ := tx.Catalog().Kind("E")
		err := tx.scan(keys.Members(k.ID), false, func(k, _ []byte) error {
			sum++
			return nil
		})
		if sum != n {
			t.Fatalf("a scan found %d of %d", sum, n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyIndexes(); err != nil {
		t.Fatal(err)
	}
}
