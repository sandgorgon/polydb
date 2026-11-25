package engine

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// TestSweepWithWriters sweeps nonstop while writers change and delete
// entities of a kind keeping a minute of history, on a clock that moves
// a second per write. A field's newest version must always be its
// current value: the sweep never removes what is still current.
func TestSweepWithWriters(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC).UnixNano())
	eng, err := Open(Options{Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }, HistorySweep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	db, err := eng.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "H", History: model.History{On: true, Keep: time.Minute},
			Fields: []model.FieldDef{fd("a", "int?"), fd("b", "int?")}})
		return err
	})
	var ids []value.ID
	update(t, db, func(tx *Tx) error {
		for range 200 {
			id, err := tx.Create("H", Fields{"a": value.Int(0), "b": value.Int(0)})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 5))
			for !stop.Load() {
				now.Add(int64(time.Second))
				id := ids[rng.IntN(len(ids))]
				err := db.Update(func(tx *Tx) error {
					switch rng.IntN(2000) {
					case 0:
						return tx.Delete(id)
					case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10:
						return tx.Unset(id, "b")
					default:
						return tx.Set(id, Fields{"a": value.Int(int64(rng.IntN(1000)))})
					}
				})
				if err != nil && !IsConflict(err) && CodeOf(err) != CodeNotFound {
					t.Error(err)
					return
				}
			}
		})
	}
	sweeps := 0
	var removed SweepResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := db.SweepHistory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		removed.Versions += r.Versions
		removed.Entities += r.Entities
		sweeps++
	}
	stop.Store(true)
	wg.Wait()
	now.Add(int64(time.Hour))
	if _, err := db.SweepHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d sweeps removed %d versions and %d deleted entities", sweeps, removed.Versions, removed.Entities)
	if removed.Versions == 0 || removed.Entities == 0 {
		t.Error("the sweeps removed nothing")
	}
	live := 0
	err = db.View(func(tx *Tx) error {
		for _, id := range ids {
			st, err := tx.loadEntity(id)
			if CodeOf(err) == CodeNotFound {
				if l, err := tx.life(id); err != nil || l != nil {
					t.Errorf("%s: deleted an hour ago, life record %v (%v)", id, l, err)
				}
				continue
			}
			if err != nil {
				return err
			}
			live++
			for _, name := range []string{"a", "b"} {
				vs, err := tx.versions(id, name)
				if err != nil {
					return err
				}
				if len(vs) != 1 {
					t.Errorf("%s.%s: %d versions an hour after the last write, want 1", id, name, len(vs))
					continue
				}
				var cur value.Value
				if f := st.fields[name]; f != nil {
					cur = f.Value
				}
				if !sameValue(vs[0].Value, cur) {
					t.Errorf("%s.%s: newest version %v, current %v", id, name, vs[0].Value, cur)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if live < 50 {
		t.Errorf("only %d entities left alive to check", live)
	}
}
