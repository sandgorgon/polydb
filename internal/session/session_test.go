package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/query"
	"github.com/sandgorgon/polydb/value"
)

func TestExecErrorIndex(t *testing.T) {
	s := newScriptSession(t, false)
	res, err := s.Exec(context.Background(), `define kind K { a: int }; create K { a: 1 }; create K { a: "x" }; create K { a: 2 }`, nil)
	var se *Error
	if !errors.As(err, &se) || se.Index != 2 || se.Code != "type" || se.Pos.Line != 1 {
		t.Fatalf("got %#v", err)
	}
	if len(res) != 2 {
		t.Errorf("got %d results before the error", len(res))
	}
	if _, err := s.Exec(context.Background(), "find K k |", nil); err == nil || !strings.Contains(err.Error(), "syntax") {
		t.Errorf("syntax error: %v", err)
	}
}

func TestParamsAndExplainAnalyze(t *testing.T) {
	s := newScriptSession(t, false)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `define kind K { a: int }; create K { a: 1 }; create K { a: 5 }; create K { a: 9 }`, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Exec(ctx, `find K k | where k.a > $min | show count()`, map[string]value.Value{"min": value.Int(2)})
	if err != nil || res[0].Rows[0][0].(value.Value).String() != "2" {
		t.Fatalf("params: %v, %v", res, err)
	}
	res, err = s.Exec(ctx, `explain analyze find K k | where k.a > 2 | show k.a`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, row := range res[0].Rows {
		steps = append(steps, row[0].(value.Value).String()+" "+row[1].(value.Value).String()+"="+row[3].(value.Value).String())
	}
	want := `"find" "k: scan kind K"=2 "source" "the rows the source produced"=2 ` +
		`"where" "where k.a > 2 (checked at the source)"=null "show" "show k.a"=2 "total" ""=2`
	if got := strings.Join(steps, " "); got != want {
		t.Errorf("explain analyze:\n got %s\nwant %s", got, want)
	}
}

func TestCancel(t *testing.T) {
	s := newScriptSession(t, false)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `define kind K { a: int }`, nil); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 600 {
		b.WriteString("create K { a: " + itoa(i) + " };")
	}
	if _, err := s.Exec(ctx, b.String(), nil); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := s.Exec(cancelled, `find K k | where k.a > 0`, nil)
	var se *Error
	if !errors.As(err, &se) || se.Code != "limit" || !strings.Contains(se.Msg, "cancelled") {
		t.Errorf("got %v", err)
	}
}

func itoa(i int) string { return value.Int(i).String() }

// run executes a script in a request and returns the results.
func runIn(t *testing.T, r *Request, src string) ([]*query.Result, error) {
	t.Helper()
	stmts, err := lang.ParseScript(src)
	if err != nil {
		t.Fatal(err)
	}
	var out []*query.Result
	for _, st := range stmts {
		res, err := r.Exec(context.Background(), st, nil)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func TestRequestsAndTransactions(t *testing.T) {
	s := newScriptSession(t, false)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `define kind K { a: int }`, nil); err != nil {
		t.Fatal(err)
	}

	// A request that begins a transaction leaves it open for later ones.
	r1, _ := s.Request(nil)
	if _, err := runIn(t, r1, `begin; create K @x { a: 1 }`); err != nil {
		t.Fatal(err)
	}
	id, state := r1.Txn()
	if id == nil || state != "open" {
		t.Fatalf("after begin: %v %s", id, state)
	}
	r1.Close()

	// Handles bound in the transaction aren't visible outside it yet.
	if _, err := s.Exec(ctx, `find @x as k`, nil); err == nil {
		t.Error("an uncommitted handle is visible outside its transaction")
	}

	// The transaction runs one request at a time.
	r2, err := s.Request(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Request(id); codeOf(err) != "txn_busy" {
		t.Errorf("second request: %v", err)
	}
	res, err := runIn(t, r2, `find @x as k | show k.a`)
	if err != nil || len(res[0].Rows) != 1 {
		t.Fatalf("inside: %v %v", res, err)
	}
	// A second transaction runs alongside the first.
	r3, _ := s.Request(nil)
	if _, err := runIn(t, r3, `begin; create K @y { a: 2 }`); err != nil {
		t.Fatal(err)
	}
	id3, _ := r3.Txn()
	r3.Close()
	if _, err := runIn(t, r2, `commit`); err != nil {
		t.Fatal(err)
	}
	if _, st := r2.Txn(); st != "committed" {
		t.Errorf("after commit: %s", st)
	}
	r2.Close()
	if _, err := s.Exec(ctx, `find @x as k`, nil); err != nil {
		t.Errorf("committed handle: %v", err)
	}
	if _, err := s.Request(id); codeOf(err) != "txn_not_found" {
		t.Errorf("finished transaction: %v", err)
	}

	// A failed statement aborts the transaction; rollback drops its handles.
	r4, _ := s.Request(id3)
	if _, err := runIn(t, r4, `create K { a: "x" }`); err == nil {
		t.Fatal("bad create succeeded")
	}
	if _, st := r4.Txn(); st != "failed" {
		t.Errorf("after failure: %s", st)
	}
	if _, err := runIn(t, r4, `find K k`); codeOf(err) != "txn_aborted" {
		t.Errorf("after failure: %v", err)
	}
	if _, err := runIn(t, r4, `rollback`); err != nil {
		t.Fatal(err)
	}
	r4.Close()
	if _, err := s.Exec(ctx, `find @y as k`, nil); err == nil {
		t.Error("a rolled-back handle survived")
	}

	// Idle transactions expire.
	r5, _ := s.Request(nil)
	runIn(t, r5, `begin`)
	id5, _ := r5.Txn()
	r5.Close()
	if got := s.ExpireIdle(0); len(got) != 1 || got[0] != *id5 {
		t.Errorf("expired %v", got)
	}

	// Prepared statements.
	pid, _, err := s.Prepare(`find K k | where k.a > $min | show count()`)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := s.Prepared(pid)
	if !ok {
		t.Fatal("prepared statement missing")
	}
	r6, _ := s.Request(nil)
	res6, err := r6.Exec(ctx, st, map[string]value.Value{"min": value.Int(0)})
	if err != nil || res6.Rows[0][0].(value.Value).String() != "1" {
		t.Errorf("execute: %v %v", res6, err)
	}
	if !s.ClosePrepared(pid) || s.ClosePrepared(pid) {
		t.Error("ClosePrepared")
	}
}

func TestConcurrentRequests(t *testing.T) {
	s := newScriptSession(t, false)
	ctx := context.Background()
	s.Exec(ctx, `define kind K { a: int }`, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				r, _ := s.Request(nil)
				st, _ := lang.ParseStatement("create K @h" + itoa(i) + " { a: " + itoa(j) + " }")
				if _, err := r.Exec(ctx, st, nil); err != nil {
					t.Error(err)
				}
				r.Close()
			}
		}()
	}
	wg.Wait()
	res, err := s.Exec(ctx, `find K k | show count()`, nil)
	if err != nil || res[0].Rows[0][0].(value.Value).String() != "160" {
		t.Errorf("count: %v %v", res, err)
	}
	if len(s.Handles()) != 8 {
		t.Errorf("handles: %d", len(s.Handles()))
	}
}

func codeOf(err error) string {
	var le *lang.Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}
