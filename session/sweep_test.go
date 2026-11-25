package session

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/value"
)

// sweepView is everything a read within retention can see: each live
// entity's history, and each kind as of times in the last hours.
func sweepView(t *testing.T, s *Session, handles []string, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	run := func(q string) {
		res, err := s.Exec(ctx, q, nil)
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", q, err)
			return
		}
		for _, r := range res {
			for _, row := range r.Rows {
				fmt.Fprintln(&b, row...)
			}
		}
	}
	for _, h := range handles {
		for _, f := range []string{"x", "y", "z", "v", "w"} {
			run(fmt.Sprintf("history @%s.%s", h, f))
		}
	}
	for _, back := range []time.Duration{0, time.Hour, 3 * time.Hour, 4*time.Hour + 59*time.Minute} {
		at := at(now.Add(-back))
		run(fmt.Sprintf("find A a as of %s | sort a::id | show a::id, a.x, a.y, a.z", at))
		run(fmt.Sprintf("find B b as of %s | sort b::id | show b::id, b.v", at))
		run(fmt.Sprintf("find C c as of %s | sort c::id | show c::id, c.v", at))
		run(fmt.Sprintf("find A a as of %s | show count()", at))
	}
	return b.String()
}

// historyKeys counts versions and life records left in the store.
func historyKeys(t *testing.T, db *engine.DB) (versions, lives int) {
	t.Helper()
	err := db.View(func(tx *engine.Tx) error {
		var err error
		versions, lives, err = tx.CountHistory()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return versions, lives
}

// at formats a time as a Rhizome literal.
func at(t time.Time) string {
	v, err := value.NewTime(t)
	if err != nil {
		panic(err)
	}
	return v.String()
}

// TestHistorySweep writes random histories over simulated days, then
// checks that sweeping changes no read within retention, removes what
// is past it, and finds nothing more the second time.
func TestHistorySweep(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint("disk=", disk), func(t *testing.T) {
			s, clock := newClockedSession(t, disk)
			ctx := context.Background()
			exec := func(q string) {
				t.Helper()
				if _, err := s.Exec(ctx, q, nil); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			exec(`
define kind A history keep 10h { x: int?, y: text? history keep 30h, z: int? };
define kind B { v: int? history keep 5h, w: int? history };
define kind C history { v: int? };`)
			db, err := s.eng.Database("test")
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(1, 2))
			var live []string
			kinds := map[string]string{}
			n := 0
			for step := range 600 {
				clock.Add(time.Duration(rng.IntN(240)) * time.Minute)
				switch op := rng.IntN(10); {
				case op < 3 || len(live) == 0:
					k := []string{"A", "B", "C"}[rng.IntN(3)]
					h := fmt.Sprintf("e%d", n)
					n++
					fields := map[string]string{"A": "x: 1, y: \"a\", z: 2", "B": "v: 1, w: 1", "C": "v: 1"}[k]
					exec(fmt.Sprintf("create %s @%s { %s }", k, h, fields))
					live, kinds[h] = append(live, h), k
				case op < 5:
					i := rng.IntN(len(live))
					exec("delete @" + live[i])
					live = slices.Delete(live, i, i+1)
				default:
					h := live[rng.IntN(len(live))]
					f := map[string][]string{"A": {"x", "y", "z"}, "B": {"v", "w"}, "C": {"v"}}[kinds[h]]
					name := f[rng.IntN(len(f))]
					if rng.IntN(4) == 0 {
						exec(fmt.Sprintf("update @%s unset %s", h, name))
					} else {
						val := fmt.Sprint(rng.IntN(100))
						if name == "y" {
							val = fmt.Sprintf("%q", val)
						}
						exec(fmt.Sprintf("update @%s set %s = %s", h, name, val))
					}
				}
				if step%150 != 149 {
					continue
				}
				before := sweepView(t, s, live, clock.Now())
				v0, l0 := historyKeys(t, db)
				res, err := db.SweepHistory(ctx)
				if err != nil {
					t.Fatal(err)
				}
				v1, l1 := historyKeys(t, db)
				if after := sweepView(t, s, live, clock.Now()); after != before {
					t.Fatalf("step %d: the sweep changed reads:\nbefore\n%s\nafter\n%s", step, before, after)
				}
				if v0-v1 != res.Versions || l0-l1 != res.Entities {
					t.Errorf("step %d: sweep reports %+v, but %d versions and %d life records went", step, res, v0-v1, l0-l1)
				}
				if again, err := db.SweepHistory(ctx); err != nil || again != (engine.SweepResult{}) {
					t.Errorf("step %d: a second sweep removed %+v (%v)", step, again, err)
				}
				t.Logf("step %d: removed %d of %d versions, and forgot %d of %d entities with life records", step, res.Versions, v0, res.Entities, l0)
			}
		})
	}
}

// TestHistorySweepRemovesUnreadable checks the two cases writes never
// clean up: history turned off, and deleted entities past retention.
func TestHistorySweepRemovesUnreadable(t *testing.T) {
	s, clock := newClockedSession(t, false)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := s.Exec(ctx, q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		"define kind D { v: int? history }; define kind K history keep 1h { v: int? }",
		"create D @d { v: 1 }", "update @d set v = 2",
		"create K @k { v: 1 }", "create K @gone { v: 5 }", "delete @gone",
	} {
		clock.Add(time.Minute) // versions are keyed by their time
		exec(q)
	}
	db, _ := s.eng.Database("test")
	exec(`alter kind D alter v : int?`)
	clock.Add(2 * time.Hour)
	res, err := db.SweepHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// @d's two versions, and @gone's one version and life record.
	if res != (engine.SweepResult{Versions: 3, Entities: 1}) {
		t.Errorf("swept %+v", res)
	}
	if v, l := historyKeys(t, db); v != 1 || l != 1 {
		t.Errorf("%d versions and %d life records left, want @k's 1 and 1", v, l)
	}
	if _, err := s.Exec(ctx, "find K k as of "+at(clock.Now().Add(-30*time.Minute))+" | show k.v", nil); err != nil {
		t.Error(err)
	}
}
