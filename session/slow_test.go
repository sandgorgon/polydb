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

func TestSlowQueryLog(t *testing.T) {
	var logged []engine.SlowQuery
	eng, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1, PasswordIterations: security.MinIterations,
		SlowQuery: time.Nanosecond, OnSlowQuery: func(q engine.SlowQuery) { logged = append(logged, q) }})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	s := New(eng, security.AdminUser)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Exec(ctx, `create database d; use d; define kind P { n: int, q: queue.fifo<int> }; define index p_n on P(n);
		create P { n: 1 }; create P { n: 2 }; create user u password "pw"; grant read on database d to u`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, `find P p | where p.n = $n | show p.n`, map[string]value.Value{"n": value.Int(2)}); err != nil {
		t.Fatal(err)
	}
	s.Exec(ctx, `find P p | where p.n = "x"`, nil) // a failing statement
	s.Exec(ctx, `find P p | pop p.q wait 1ms`, nil)

	res, err := s.Exec(ctx, `show slow queries`, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := rowsText(res[0])
	first := res[0].Rows[0] // newest first: the failing query (the waiting pop isn't logged)
	if !strings.Contains(first[4].(value.Text).Go(), `p.n = "x"`) || value.IsNull(first[8].(value.Value)) {
		t.Errorf("newest entry: %v", first)
	}
	var q engine.SlowQuery
	for _, l := range logged {
		if strings.Contains(l.Statement, "$n") {
			q = l
		}
	}
	if q.Statement == "" || q.User != "admin" || q.Database != "d" || q.Rows != 1 {
		t.Fatalf("logged query: %+v", q)
	}
	if !strings.Contains(q.Plan, "index p_n") || !strings.Contains(q.Plan, "(1 rows)") {
		t.Errorf("plan: %q", q.Plan)
	}
	if strings.Contains(rows, `pw\"`) || !strings.Contains(rows, `(hidden)`) {
		t.Errorf("a password in the log:\n%s", rows)
	}
	if strings.Contains(rows, "pop") {
		t.Errorf("a waiting pop was logged:\n%s", rows)
	}
	// Only server admins see it.
	u := New(eng, "u")
	defer u.Close()
	if _, err := u.Exec(ctx, `show slow queries`, nil); execCode(err) != "permission" {
		t.Errorf("non-admin: %v", err)
	}

	// Off: nothing is logged.
	off, _ := engine.Open(engine.Options{SlowQuery: -1})
	defer off.Close()
	o := New(off, security.AdminUser)
	defer o.Close()
	o.Exec(ctx, `create database x`, nil)
	if n := len(off.SlowQueries()); n != 0 {
		t.Errorf("%d slow queries with the log off", n)
	}
}
