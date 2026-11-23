package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/value"
)

func TestExecErrorIndex(t *testing.T) {
	s := newScriptSession(t)
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
	s := newScriptSession(t)
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
		steps = append(steps, row[0].(value.Value).String()+"="+row[1].(value.Value).String())
	}
	if got := strings.Join(steps, " "); got != `"source"=3 "where where k.a > 2"=2 "show show k.a"=2 "total"=2` {
		t.Errorf("explain analyze: %s", got)
	}
}

func TestCancel(t *testing.T) {
	s := newScriptSession(t)
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
