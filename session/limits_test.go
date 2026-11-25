package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// limitsEngine has kind P with n entities in database d, and a user u
// with write permission there.
func limitsEngine(t *testing.T, lim engine.Limits, n int) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1, PasswordIterations: security.MinIterations, Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	s := New(eng, security.AdminUser)
	defer s.Close()
	if _, err := s.Exec(context.Background(), `create database d; use d; define kind P { n: int, s: text };
		create user u password "pw"; grant write on database d to u; grant read on database d to u`, nil); err != nil {
		t.Fatal(err)
	}
	db, _ := eng.Database("d")
	db.Update(func(tx *engine.Tx) error {
		for i := range n {
			if _, err := tx.Create("P", engine.Fields{"n": value.Int(i), "s": value.MustText(strings.Repeat("x", 100))}); err != nil {
				return err
			}
		}
		return nil
	})
	return eng
}

func userSession(t *testing.T, eng *engine.Engine, user string) *Session {
	t.Helper()
	s := New(eng, user)
	t.Cleanup(s.Close)
	if _, err := s.Exec(context.Background(), `use d`, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// max_memory stops a statement whose rows would outgrow it, early for a
// cross product, and counts what sort and group keep.
func TestMemoryLimit(t *testing.T) {
	eng := limitsEngine(t, engine.Limits{MaxMemory: 1 << 20}, 1000)
	s := userSession(t, eng, security.AdminUser)
	ctx := context.Background()
	began := time.Now()
	_, err := s.Exec(ctx, `find P a, P b | show count()`, nil) // a million rows
	if execCode(err) != "limit" || !strings.Contains(err.Error(), "max_memory") {
		t.Fatalf("cross product: %v", err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("the cross product ran %s before stopping", d)
	}
	// Within the limit, the same statement works, and a statement that
	// holds less than the limit is fine.
	if got := sessionRows(t, s, `find P p | where p.n < 10 | sort p.s | show count()`); got != "10" {
		t.Errorf("a small query: %s", got)
	}
	if _, err := s.Exec(ctx, `set max_memory = 0; find P a, P b | where a.n < 30 and b.n < 30 | show count()`, nil); err != nil {
		t.Errorf("without a limit: %v", err)
	}
	// Rows carrying long text count by their size.
	if _, err := s.Exec(ctx, `set max_memory = 65536; find P p | show p.s ++ p.s as long | sort long`, nil); execCode(err) != "limit" {
		t.Errorf("rows of long text: %v", err)
	}
}

// Sessions start at the server's limits and may lower them; only a
// server admin may raise them past those, or lift them.
func TestLimitCeilings(t *testing.T) {
	eng := limitsEngine(t, engine.Limits{MaxMemory: 1 << 20, StatementTimeout: time.Minute}, 10)
	u := userSession(t, eng, "u")
	admin := userSession(t, eng, security.AdminUser)
	ctx := context.Background()
	if set := u.Settings(); set.MaxMemory != 1<<20 || set.Timeout != time.Minute {
		t.Fatalf("starting settings %+v", set)
	}
	for _, q := range []string{`set max_memory = 0`, `set max_memory = 2097152`, `set timeout = 0s`, `set timeout = 2m`} {
		if _, err := u.Exec(ctx, q, nil); execCode(err) != "permission" {
			t.Errorf("%s as a user: %v", q, err)
		}
	}
	for _, q := range []string{`set max_memory = 524288`, `set timeout = 30s`} {
		if _, err := u.Exec(ctx, q, nil); err != nil {
			t.Errorf("%s as a user: %v", q, err)
		}
	}
	for _, q := range []string{`set max_memory = 0`, `set timeout = 10m`} {
		if _, err := admin.Exec(ctx, q, nil); err != nil {
			t.Errorf("%s as admin: %v", q, err)
		}
	}
	got := sessionRows(t, u, `show limits`)
	for _, want := range []string{`"timeout" 30s`, `"max_memory" 524288`, `"max transactions" 16`, `"upload quota" 17179869184`} {
		if !strings.Contains(got, want) {
			t.Errorf("show limits lacks %s:\n%s", want, got)
		}
	}
}

// The statement timeout stops any statement, and a session holds only so
// many open transactions.
func TestTimeoutAndTransactions(t *testing.T) {
	eng := limitsEngine(t, engine.Limits{StatementTimeout: 50 * time.Millisecond, MaxTransactions: 2, MaxMemory: -1}, 2000)
	s := userSession(t, eng, "u")
	ctx := context.Background()
	began := time.Now()
	if _, err := s.Exec(ctx, `find P a, P b | where a.s = b.s | show count()`, nil); execCode(err) != "limit" {
		t.Fatalf("a long statement: %v", err)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("stopped after %s", d)
	}
	for i := range 3 {
		r, _ := s.Request(nil)
		_, err := runIn(t, r, `begin`)
		r.Close()
		if i < 2 && err != nil {
			t.Fatalf("transaction %d: %v", i, err)
		}
		if i == 2 && codeOf(err) != "limit" {
			t.Fatalf("a third transaction: %v", err)
		}
	}
}
