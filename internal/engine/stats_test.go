package engine

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// column samples a population the way SampleStats does and builds its
// statistics.
func column(pop []value.Value, sample int, rng *rand.Rand) (*ColumnStats, int) {
	idx := rng.Perm(len(pop))[:min(sample, len(pop))]
	counts := map[string]int{}
	var keyed [][]byte
	for _, i := range idx {
		k, _ := value.Key(pop[i])
		keyed = append(keyed, k)
		counts[string(k)]++
	}
	c := &ColumnStats{Present: len(idx), Keyed: len(keyed)}
	c.Distinct = estimateDistinct(counts, len(idx), len(pop))
	c.Common, c.Bounds = distribution(keyed, len(counts))
	return c, len(idx)
}

func key(v value.Value) []byte { k, _ := value.Key(v); return k }

// truth is the real share of a population in a key range, or equal to a key.
func truth(pop []value.Value, in func(k []byte) bool) float64 {
	n := 0
	for _, v := range pop {
		if in(key(v)) {
			n++
		}
	}
	return float64(n) / float64(len(pop))
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: estimate %.4f, actual %.4f", what, got, want)
	}
}

func TestStatsEstimates(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	const N = 100000
	t.Run("uniform", func(t *testing.T) {
		pop := make([]value.Value, N)
		for i := range pop {
			pop[i] = value.Int(int64(rng.IntN(1000)))
		}
		c, n := column(pop, statsSample, rng)
		if c.Distinct < 900 || c.Distinct > 1100 {
			t.Errorf("distinct %d, actual 1000", c.Distinct)
		}
		near(t, "= 5", c.EqFraction(key(value.Int(5)), n), 0.001, 0.0007)
		lo, hi := key(value.Int(100)), key(value.Int(300))
		near(t, "[100, 300)", c.RangeFraction(lo, hi, n), 0.2, 0.03)
		near(t, ">= 900", c.RangeFraction(key(value.Int(900)), nil, n), 0.1, 0.03)
		near(t, "= 5000 (never)", c.EqFraction(key(value.Int(5000)), n), 0, 0.0002)
	})
	t.Run("skewed", func(t *testing.T) {
		z := rand.NewZipf(rng, 1.3, 1, 5000)
		pop := make([]value.Value, N)
		for i := range pop {
			pop[i] = value.Int(int64(z.Uint64()))
		}
		c, n := column(pop, statsSample, rng)
		for _, v := range []int64{0, 1, 2, 10} {
			k := key(value.Int(v))
			want := truth(pop, func(x []byte) bool { return string(x) == string(k) })
			near(t, "= "+value.Int(v).String(), c.EqFraction(k, n), want, max(0.01, want/3))
		}
		lo := key(value.Int(100))
		near(t, ">= 100", c.RangeFraction(lo, nil, n), truth(pop, func(x []byte) bool { return string(x) >= string(lo) }), 0.03)
	})
	t.Run("distinct", func(t *testing.T) {
		pop := make([]value.Value, N)
		for i := range pop {
			pop[i] = value.MustText(value.Int(int64(i)).String() + "@x")
		}
		c, n := column(pop, statsSample, rng)
		if c.Distinct < N*9/10 || c.Distinct > N {
			t.Errorf("distinct %d, actual %d", c.Distinct, N)
		}
		near(t, "one email", c.EqFraction(key(value.MustText("77@x")), n), 1.0/N, 2.0/N)
		lo, hi := key(value.MustText("1")), key(value.MustText("2"))
		near(t, "starting with 1", c.RangeFraction(lo, hi, n), truth(pop, func(x []byte) bool { return string(x) >= string(lo) && string(x) < string(hi) }), 0.03)
	})
	t.Run("small kinds are exact", func(t *testing.T) {
		pop := make([]value.Value, 500)
		for i := range pop {
			pop[i] = value.Int(int64(i % 7))
		}
		c, n := column(pop, statsSample, rng)
		if c.Distinct != 7 {
			t.Errorf("distinct %d, want 7", c.Distinct)
		}
		near(t, "= 3", c.EqFraction(key(value.Int(3)), n), truth(pop, func(x []byte) bool { return string(x) == string(key(value.Int(3))) }), 1e-9)
	})
}

// TestStatsRefresh checks that a kind gets statistics in the background,
// and new ones once enough of it has changed.
func TestStatsRefresh(t *testing.T) {
	eng, err := Open(Options{StatsRefresh: 5 * time.Millisecond, HistorySweep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	db, err := eng.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "S", Fields: []model.FieldDef{fd("n", "int")}})
		return err
	})
	create := func(count int) {
		update(t, db, func(tx *Tx) error {
			for i := range count {
				if _, err := tx.Create("S", Fields{"n": value.Int(int64(i % 10))}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	stats := func() *KindStats {
		var s *KindStats
		db.View(func(tx *Tx) error {
			k, _ := tx.Catalog().Kind("S")
			var err error
			s, err = tx.KindStats(k)
			return err
		})
		return s
	}
	waitStats := func(entities int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s := stats(); s != nil && s.Entities == entities {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no statistics for %d entities (have %+v)", entities, stats())
	}
	create(1000)
	waitStats(1000)
	if c := stats().Column(model.IndexKey{Field: "n"}); c == nil || c.Distinct != 10 || len(c.Common) != 10 {
		t.Errorf("column n: %+v", c)
	}
	create(40) // fewer than statsMinChanges and a tenth: still fresh
	time.Sleep(50 * time.Millisecond)
	if s := stats(); s.Entities != 1000 {
		t.Errorf("resampled after 40 changes: %d entities", s.Entities)
	}
	create(100)
	waitStats(1140)
}
