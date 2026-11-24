package session

import (
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// lazySession returns a session on an engine whose statements may load
// at most maxLoad bytes of large values, with kind F holding two large
// texts (a 100 KiB body, a 300 KiB one) and a small one.
func lazySession(t *testing.T, maxLoad int64) *Session {
	t.Helper()
	eng, err := engine.Open(engine.Options{MaxLoad: maxLoad, HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	s := New(eng, security.AdminUser)
	t.Cleanup(s.Close)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `create database d; use d;
		define kind F { name: text, body: text?, data: bytes?, n: int? }`, nil); err != nil {
		t.Fatal(err)
	}
	params := map[string]value.Value{
		"a": value.MustText(strings.Repeat("abcd", 25<<10)),
		"b": value.MustText(strings.Repeat("wxyé", 60<<10)),
	}
	if _, err := s.Exec(ctx, `create F { name: "a", body: $a }; create F { name: "b", body: $b };
		create F { name: "c", body: "small" }`, params); err != nil {
		t.Fatal(err)
	}
	return s
}

// lazyRun runs statements in a request that leaves large values in
// results lazy, as the server does, and returns the last result.
func lazyRun(t *testing.T, s *Session, src string) (*query.Result, error) {
	t.Helper()
	r, err := s.Request(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.LazyResults = true
	out, err := runIn(t, r, src)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[len(out)-1], nil
}

// Passing large values along loads none of them, so it works with a
// load limit smaller than any of them.
func TestLazyPassAlong(t *testing.T) {
	s := lazySession(t, 1000)
	ok := map[string]string{
		`find F f | where f.name = "a" | show f.body`:                                 "<text of 102400 bytes>",
		`find F f | where f.name = "b" | show len(f.body)`:                            "245760",
		`find F f | where f.body is not null | show count(f.body)`:                    "3",
		`find F f | where f.name = "a" | show typeof(f.body)`:                         `"text"`,
		`find F f, F g | where f.name = "a" and g.name = "a" | show f.body = g.body`:  "true",
		`find F f, F g | where f.name = "a" and g.name = "b" | show f.body != g.body`: "true",
		`find F f | where f.name = "a" | show f.body = "short"`:                       "false",
		`find F f | where f.name = "a" | show if(f.n is null, f.body, "x")`:           "<text of 102400 bytes>",
		`find F f | where f.name = "b" | show coalesce(f.body, "none")`:               "<text of 307200 bytes>",
	}
	for src, want := range ok {
		res, err := lazyRun(t, s, src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if want == "" {
			continue
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			t.Errorf("%s: no rows", src)
			continue
		}
		if got := res.Rows[0][0].(value.Value).String(); got != want {
			t.Errorf("%s: got %s, want %s", src, got, want)
		}
	}
	// Copies, by create and by update, share the stored content.
	if _, err := lazyRun(t, s, `find F f, F g | where f.name = "b" and g.name = "c" | update g set body = f.body`); err != nil {
		t.Fatal(err)
	}
	res, err := lazyRun(t, s, `find F f | where f.body = f.body | sort f.name | show f.name, len(f.body)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsText(res); got != `"a" 102400; "b" 245760; "c" 245760` {
		t.Errorf("after copying: %s", got)
	}
	// A whole entity in a result keeps its large fields lazy.
	res, err = lazyRun(t, s, `find F f | where f.name = "a"`)
	if err != nil {
		t.Fatal(err)
	}
	e := res.Rows[0][0].(*query.Entity)
	for _, f := range e.Fields {
		if f.Name == "body" {
			if _, ok := f.Value.(value.Lazy); !ok {
				t.Errorf("entity field body is %T", f.Value)
			}
		}
	}

	// What needs the content goes over the limit.
	for _, src := range []string{
		`find F f | where f.name = "a" | show upper(f.body)`,
		`find F f | where f.body contains "zz" | show f.name`,
		`find F f | sort f.body | show f.name`,
		`find F f | where f.name = "a" | show f.body ++ "!"`,
	} {
		if _, err := lazyRun(t, s, src); codeOf(err) != "limit" {
			t.Errorf("%s: %v, want a limit error", src, err)
		}
	}
	// Results that aren't lazy load their large values, within the limit.
	if _, err := s.Exec(context.Background(), `find F f | where f.name = "a" | show f.body`, nil); execCode(err) != "limit" {
		t.Errorf("loaded result: %v, want a limit error", err)
	}
}

// Without a limit, values load where needed and every operation sees the
// content.
func TestLazyLoadsWhenNeeded(t *testing.T) {
	s := lazySession(t, -1)
	ctx := context.Background()
	check := func(src, want string) {
		t.Helper()
		res, err := s.Exec(ctx, src, nil)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := rowsText(res[len(res)-1]); got != want {
			t.Errorf("%s:\n got %s\nwant %s", src, got, want)
		}
	}
	check(`find F f | sort f.body desc | show f.name`, `"b"; "c"; "a"`)
	check(`find F f | where f.body contains "dabc" | show f.name`, `"a"`)
	check(`find F f | where f.body starts with "wxyé" | show f.name, len(f.body)`, `"b" 245760`)
	check(`find F f | show f.body as b | distinct | sort len(b) | show len(b)`, `5; 102400; 245760`)
	check(`find F f | group by len(f.body) > 10 as big | sort big | show big, count()`, `false 1; true 2`)
	check(`find F f | where f.name = "a" | show substr(f.body, 0, 6)`, `"abcdab"`)
	// ++= reads the current value and stores the joined one.
	check(`find F f | where f.name = "a" | update f set body ++= "!"`, ``)
	check(`find F f | where f.name = "a" | show len(f.body), f.body ends with "d!"`, `102401 true`)
	// A unique field keys on content, also when the value is copied.
	if _, err := s.Exec(ctx, `define kind U { t: text unique }; create U { t: "1" }; create U { t: "2" };
		find F f, U u | where f.name = "b" and u.t = "1" | update u set t = f.body`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, `find F f, U u | where f.name = "b" and u.t = "2" | update u set t = f.body`, nil); execCode(err) != "constraint" {
		t.Errorf("duplicate large unique value: %v", err)
	}
	if _, err := s.Exec(ctx, `find F f, U u | where f.name = "a" and u.t = "2" | update u set t = f.body`, nil); err != nil {
		t.Errorf("different large unique value: %v", err)
	}
}

// execCode returns the code of an error from Session.Exec.
func execCode(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

// rowsText renders a result's rows as "c1 c2; c1 c2".
func rowsText(res *query.Result) string {
	var rows []string
	for _, r := range res.Rows {
		var cells []string
		for _, c := range r {
			if v, ok := c.(value.Value); ok {
				cells = append(cells, v.String())
			} else {
				cells = append(cells, "?")
			}
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return strings.Join(rows, "; ")
}
