package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// An alter held up halfway through its rewrite survives a restart: the
// kind stays closed to writes, entities not yet rewritten read through
// the new definition, and the job finishes once it may.
func TestAlterRestartMidRewrite(t *testing.T) {
	fs := vfs.NewMem()
	open := func() *Engine {
		return must(Open(Options{Dir: "/data", FS: fs, Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
	}
	var hold atomic.Bool
	hold.Store(true)
	alterHook = func(phase string, done int64) error {
		if hold.Load() && phase == alterRewriting && done >= alterBatch {
			return errors.New("held for the test")
		}
		return nil
	}
	retry := alterRetry
	alterRetry = 20 * time.Millisecond
	defer func() { alterHook, alterRetry = nil, retry }()

	eng := open()
	db := must(eng.CreateDatabase("d"))
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "P", Fields: []model.FieldDef{fd("n", "int"), fd("note", "text?")}})
		return err
	})
	var ids []value.ID
	for i := 0; i < 1500; i += 500 {
		update(t, db, func(tx *Tx) error {
			for j := i; j < i+500; j++ {
				id, err := tx.Create("P", Fields{"n": value.Int(j), "note": txt("x")})
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return nil
		})
	}
	update(t, db, func(tx *Tx) error {
		return tx.AlterKind("P", func(k *model.Kind) error {
			k.Fields = []model.FieldDef{fd("n", "decimal")} // n converts, note goes
			return nil
		})
	})
	// Wait until the rewrite is held.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var st AlterStatus
		db.View(func(tx *Tx) error {
			a, err := tx.Alters()
			if len(a) > 0 {
				st = a[0]
			}
			return err
		})
		if st.Phase == alterRewriting && st.Err != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rewrite wasn't held: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng = open()
	defer eng.Close()
	db = must(eng.Database("d"))
	last := ids[len(ids)-1] // after the held batch: not rewritten yet
	err := db.Update(func(tx *Tx) error { return tx.Set(last, Fields{"n": value.Int(1)}) })
	if err == nil || !strings.Contains(err.Error(), "being altered") {
		t.Fatalf("write after the restart: %v", err)
	}
	e := get(t, db, last)
	if f, ok := e.Field("n"); !ok || f.Value.Code() != value.CodeDecimal {
		t.Errorf("an entity not yet rewritten reads as %v", f)
	}
	if _, ok := e.Field("note"); ok {
		t.Error("a dropped field shows on an entity not yet rewritten")
	}
	err = db.View(func(tx *Tx) error {
		_, v, _, err := tx.EntityField(last, "note")
		if v != nil {
			t.Errorf("EntityField of a dropped field: %v", v)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	hold.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.WaitAlters(ctx); err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error { return tx.Set(last, Fields{"n": value.Int(1)}) })
	if err := db.VerifyIndexes(); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkAlter alters 200,000 entities in the background on disk (set
// TMPDIR to a real disk).
func BenchmarkAlter(b *testing.B) {
	eng := must(Open(Options{Dir: b.TempDir(), Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1}))
	defer eng.Close()
	db := must(eng.CreateDatabase("d"))
	db.Update(func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "P", Fields: []model.FieldDef{fd("n", "int"), fd("note", "text?")}})
		return err
	})
	const n = 200_000
	for i := 0; i < n; i += 1000 {
		db.Update(func(tx *Tx) error {
			for j := i; j < i+1000; j++ {
				if _, err := tx.Create("P", Fields{"n": value.Int(j), "note": txt("some note")}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	b.ResetTimer()
	for i := range b.N {
		typ := []string{"decimal", "int"}[i%2]
		began := time.Now()
		if err := db.Update(func(tx *Tx) error {
			return tx.AlterKind("P", func(k *model.Kind) error {
				k.Fields[0] = fd("n", typ)
				return nil
			})
		}); err != nil {
			b.Fatal(err)
		}
		if err := db.WaitAlters(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(n/time.Since(began).Seconds(), "entities/s")
	}
}
