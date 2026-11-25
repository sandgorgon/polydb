package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// alterSession is a session on database d of eng, with kind P holding n
// entities: n (0…n-1), note, tag (i mod 7).
func alterSession(t *testing.T, eng *engine.Engine, n int) *Session {
	t.Helper()
	s := New(eng, security.AdminUser)
	t.Cleanup(s.Close)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `create database d; use d; define kind P { n: int, note: text?, tag: int };
		define index p_n on P(n)`, nil); err != nil {
		t.Fatal(err)
	}
	db, _ := eng.Database("d")
	for i := 0; i < n; i += 500 {
		err := db.Update(func(tx *engine.Tx) error {
			for j := i; j < min(i+500, n); j++ {
				if _, err := tx.Create("P", engine.Fields{"n": value.Int(j), "note": value.MustText("x"), "tag": value.Int(j % 7)}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func sessionRows(t *testing.T, s *Session, q string) string {
	t.Helper()
	res, err := s.Exec(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return rowsText(res[len(res)-1])
}

func waitAlters(t *testing.T, eng *engine.Engine) {
	t.Helper()
	db, _ := eng.Database("d")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.WaitAlters(ctx); err != nil {
		t.Fatal(err)
	}
}

func memEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng
}

// Changes that leave stored data valid touch no entity.
func TestAlterInstant(t *testing.T) {
	eng := memEngine(t)
	s := alterSession(t, eng, 20)
	ctx := context.Background()
	for _, q := range []string{
		`alter kind P add extra : int?`, // extensible: entities may own "extra", so this one rewrites
	} {
		if _, err := s.Exec(ctx, q, nil); err != nil {
			t.Fatal(err)
		}
	}
	before := sessionRows(t, s, `find P p | show sum(p::version)`)
	for _, q := range []string{
		`alter kind P alter note : text? "A note"`,
		`alter kind P alter extra : int? check extra > 0`, // only removes nothing: a check on an optional, absent field passes
		`alter kind P alter n : int? `,                    // required to optional
		`alter kind P alter n : int? unique`,
		`alter kind P alter n : int?`, // unique dropped: its keys go
		`alter kind P set closed`,
		`alter kind P set extensible`,
		`alter kind P rename to Q`,
		`alter kind Q rename to P`,
	} {
		if _, err := s.Exec(ctx, q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Only the unique, check and closing changes check entities; none
	// of these rewrites one.
	if after := sessionRows(t, s, `find P p | show sum(p::version)`); after != before {
		t.Errorf("entity versions went from %s to %s", before, after)
	}
	// The dropped unique constraint left no keys: n may repeat now.
	if _, err := s.Exec(ctx, `create P { n: 3, tag: 0 }`, nil); err != nil {
		t.Errorf("a value of a field that was unique: %v", err)
	}
}

// A large kind is altered in the background: writes to it wait, reads go
// on, and the new definition applies when it is done.
func TestAlterInBackground(t *testing.T) {
	eng := memEngine(t)
	s := alterSession(t, eng, 2500)
	ctx := context.Background()
	other := New(eng, security.AdminUser)
	defer other.Close()
	if _, err := other.Exec(ctx, `use d; define kind Other { a: int }`, nil); err != nil {
		t.Fatal(err)
	}

	res, err := s.Exec(ctx, `alter kind P alter n : decimal`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Warnings) != 1 || res[0].Warnings[0].Code != "background" {
		t.Fatalf("warnings %v", res[0].Warnings)
	}
	// While it runs (or once it's done) reads work; writes to P wait,
	// other kinds don't.
	if got := sessionRows(t, s, `find P p | show count()`); got != "2500" {
		t.Errorf("count while altering: %s", got)
	}
	if _, err := other.Exec(ctx, `create Other { a: 1 }`, nil); err != nil {
		t.Errorf("write to another kind: %v", err)
	}
	_, werr := s.Exec(ctx, `create P { n: 1, tag: 0 }`, nil)
	if _, err := s.Exec(ctx, `alter kind P drop note`, nil); err == nil {
		t.Error("a second alter of the kind while one runs")
	}
	waitAlters(t, eng)
	if werr != nil && !strings.Contains(werr.Error(), "being altered") {
		t.Errorf("write during the alter: %v", werr)
	}
	if got := sessionRows(t, s, `find P p | where p.n = 7 | show typeof(p.n), p.n`); got != `"decimal" 7d` {
		t.Errorf("after the alter: %s", got)
	}
	if got := sessionRows(t, s, `show alters`); !strings.HasPrefix(got, `"P" "done" 2500 2500 0 true`) {
		t.Errorf("show alters: %s", got)
	}
	if _, err := s.Exec(ctx, `create P { n: 2.5d, tag: 0 }`, nil); err != nil {
		t.Errorf("write after the alter: %v", err)
	}
	db, _ := eng.Database("d")
	if err := db.VerifyIndexes(); err != nil {
		t.Errorf("indexes after the alter: %v", err)
	}
	if plan := sessionRows(t, s, `explain find P p | where p.n = 7`); !strings.Contains(plan, "index p_n") {
		t.Errorf("the index isn't used after the alter: %s", plan)
	}

	// Dropping a field, then adding one with a default.
	if _, err := s.Exec(ctx, `alter kind P drop note; alter kind P add flag : bool default true`, nil); err == nil ||
		!strings.Contains(err.Error(), "being altered") {
		t.Errorf("an alter while another runs: %v", err)
	}
	waitAlters(t, eng)
	if _, err := s.Exec(ctx, `alter kind P add flag : bool default true`, nil); err != nil {
		t.Fatal(err)
	}
	waitAlters(t, eng)
	if got := sessionRows(t, s, `find P p | where p.n = 9 | show p.note, p.flag`); got != `null true` {
		t.Errorf("after drop and add: %s", got)
	}
	if got := sessionRows(t, s, `describe kind P`); strings.Contains(got, "note") {
		t.Errorf("note still declared: %s", got)
	}
}

// A check that some entity doesn't pass fails the alter, which changes
// nothing; a new unique field with duplicates fails the same way.
func TestAlterCheckFails(t *testing.T) {
	eng := memEngine(t)
	s := alterSession(t, eng, 1500)
	ctx := context.Background()
	for i, q := range []string{`alter kind P alter n : int check n < 1000`, `alter kind P alter tag : int unique`} {
		if _, err := s.Exec(ctx, q, nil); err != nil {
			t.Fatal(err)
		}
		waitAlters(t, eng)
		got := sessionRows(t, s, `show alters`)
		if !strings.HasPrefix(got, `"P" "failed" `) {
			t.Fatalf("%s: show alters %s", q, got)
		}
		if _, err := s.Exec(ctx, fmt.Sprintf(`create P { n: %d, tag: 1 }`, 5000+i), nil); err != nil {
			t.Fatalf("%s: writes didn't resume: %v", q, err)
		}
	}
	if got := sessionRows(t, s, `describe kind P`); strings.Contains(got, "unique") || strings.Contains(got, "n < 1000") {
		t.Errorf("a failed alter changed the kind: %s", got)
	}
	// Without duplicates, a new unique field is enforced afterwards.
	if _, err := s.Exec(ctx, `alter kind P alter n : int unique`, nil); err != nil {
		t.Fatal(err)
	}
	waitAlters(t, eng)
	if got := sessionRows(t, s, `show alters`); !strings.HasPrefix(got, `"P" "done"`) {
		t.Fatalf("show alters: %s", got)
	}
	if _, err := s.Exec(ctx, `create P { n: 42, tag: 1 }`, nil); execCode(err) != "constraint" {
		t.Errorf("a duplicate after the unique alter: %v", err)
	}
}

// A write that began before the alter can't commit after it.
func TestAlterWriteRace(t *testing.T) {
	eng := memEngine(t)
	s := alterSession(t, eng, 1200)
	db, _ := eng.Database("d")
	tx := db.Begin(engine.TxOptions{})
	if _, err := tx.Create("P", engine.Fields{"n": value.Int(-1), "tag": value.Int(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(context.Background(), `alter kind P alter n : decimal`, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Error("a write from before the alter committed after it")
	}
	waitAlters(t, eng)
}

// An alter a restart interrupts resumes, and its kind stays closed to
// writes until it is done.
func TestAlterResumes(t *testing.T) {
	fs := vfs.NewMem()
	open := func() *engine.Engine {
		eng, err := engine.Open(engine.Options{Dir: "/data", FS: fs, HistorySweep: -1, StatsRefresh: -1})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	eng := open()
	s := alterSession(t, eng, 3000)
	if _, err := s.Exec(context.Background(), `alter kind P alter n : decimal`, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng = open()
	defer eng.Close()
	s = New(eng, security.AdminUser)
	defer s.Close()
	waitAlters(t, eng)
	if got := sessionRows(t, s, `use d; find P p | where p.n = 2999 | show typeof(p.n)`); got != `"decimal"` {
		t.Errorf("after the restart: %s", got)
	}
	if got := sessionRows(t, s, `show alters`); !strings.HasPrefix(got, `"P" "done" 3000 3000`) {
		t.Errorf("show alters: %s", got)
	}
}

// rebuild index builds an index again, at once or in the background.
func TestRebuildIndex(t *testing.T) {
	for _, n := range []int{50, 2500} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			eng := memEngine(t)
			s := alterSession(t, eng, n)
			ctx := context.Background()
			if _, err := s.Exec(ctx, `define unique index p_tag_n on P(tag, n); rebuild index p_n; rebuild index p_tag_n`, nil); err != nil {
				t.Fatal(err)
			}
			db, _ := eng.Database("d")
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := db.WaitIndexes(wctx); err != nil {
				t.Fatal(err)
			}
			if err := db.VerifyIndexes(); err != nil {
				t.Fatal(err)
			}
			if got := sessionRows(t, s, `find P p | where p.n = 7 | show p.tag`); got != "0" {
				t.Errorf("lookup after rebuild: %s", got)
			}
			if _, err := s.Exec(ctx, `rebuild index nope`, nil); execCode(err) != "not_found" {
				t.Errorf("rebuild of a missing index: %v", err)
			}
		})
	}
}
