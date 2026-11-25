package session

import (
	"context"
	"fmt"
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

	// What makes a new large value from the content goes over the limit.
	for _, src := range []string{
		`find F f | where f.name = "a" | show upper(f.body)`,
		`find F f | where f.name = "a" | show f.body ++ "!"`,
		`find F f | where f.name = "a" | show substr(f.body, 10)`,
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

// set max_load changes the session's limit; 0 lifts it.
func TestMaxLoadSetting(t *testing.T) {
	s := lazySession(t, -1)
	ctx := context.Background()
	q := `find F f | where f.name = "a" | show len(upper(f.body))`
	if _, err := s.Exec(ctx, q, nil); err != nil {
		t.Fatalf("no limit: %v", err)
	}
	if _, err := s.Exec(ctx, `set max_load = 1000; `+q, nil); execCode(err) != "limit" {
		t.Fatalf("max_load 1000: %v", err)
	}
	if _, err := s.Exec(ctx, `set max_load = 0; `+q, nil); err != nil {
		t.Fatalf("max_load 0: %v", err)
	}
	if _, err := s.Exec(ctx, `set max_load = -1`, nil); execCode(err) != "constraint" {
		t.Fatalf("negative max_load: %v", err)
	}
}

// Indexes and unique constraints on large values key them by their start
// and hash: keys stay small, writes load nothing, and queries through the
// indexes give the right rows, also for long values whose keys tie on
// their start.
func TestLargeValueIndexes(t *testing.T) {
	s := lazySession(t, 1000) // writes must not load
	ctx := context.Background()
	p := strings.Repeat("p", 300)
	vals := []string{p + "c", p + "a", p + "b", "a", "q", strings.Repeat("p", 256),
		p + " " + strings.Repeat("z", 30<<10) + " needle"}
	// In text order: 3 "a", 5 p…(256), 6 p… z… needle, 1 p…a, 2 p…b, 0 p…c, 4 "q".
	if _, err := s.Exec(ctx, `define kind T { t: text, u: text unique, n: int }`, nil); err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		params := map[string]value.Value{"v": value.MustText(v), "u": value.MustText(v + "!"), "n": value.Int(i)}
		if _, err := s.Exec(ctx, `create T { t: $v, u: $u, n: $n }`, params); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// Built over stored (lazy) values.
	if _, err := s.Exec(ctx, `define index t_t on T(t); define text index t_search on T(t)`, nil); err != nil {
		t.Fatal(err)
	}
	// A duplicate long unique value is refused.
	dup := map[string]value.Value{"u": value.MustText(vals[6] + "!")}
	if _, err := s.Exec(ctx, `create T { t: "x", u: $u, n: 99 }`, dup); execCode(err) != "constraint" {
		t.Fatalf("duplicate long unique value: %v", err)
	}
	// Copying a large value into the unique field reads only its start.
	if _, err := s.Exec(ctx, `find T a, T b | where a.n = 6 and b.n = 4 | update b set u = a.t`, nil); err != nil {
		t.Fatalf("copy into unique field: %v", err)
	}
	if _, err := s.Exec(ctx, `find T a, T b | where a.n = 6 and b.n = 3 | update b set u = a.t`, nil); execCode(err) != "constraint" {
		t.Fatalf("copied duplicate: %v", err)
	}

	// Comparing long values loads them (until streaming comparisons).
	if _, err := s.Exec(ctx, `set max_load = 0`, nil); err != nil {
		t.Fatal(err)
	}
	params := map[string]value.Value{"pb": value.MustText(p + "b")}
	for q, want := range map[string]string{
		`find T x | where x.t > "p" | sort x.n | show x.n`:                "0; 1; 2; 4; 5; 6",
		`find T x | where x.t >= $pb and x.t < "q" | sort x.n | show x.n`: "0; 2",
		`find T x | where x.t = $pb | show x.n`:                           "2",
		`find T x | where x.t starts with $pb | show x.n`:                 "2",
		`find T x | where x.t starts with "pp" | sort x.n | show x.n`:     "0; 1; 2; 5; 6",
		`find T x | sort x.t | limit 3 | show x.n`:                        "3; 5; 6",
		`find T x | sort x.t desc | limit 3 | show x.n`:                   "4; 0; 2",
		`find T x | where search(x.t, "needle") | show x.n`:               "6",
	} {
		res, err := s.Exec(ctx, q, params)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := rowsText(res[0]); got != want {
			t.Errorf("%s:\n got %s\nwant %s", q, got, want)
		}
	}
	res, err := s.Exec(ctx, `explain find T x | where x.t = $pb | show x.n`, params)
	if err != nil {
		t.Fatal(err)
	}
	if plan := fmt.Sprint(res[0].Rows); !strings.Contains(plan, "index t_t") {
		t.Errorf("equality on a long value doesn't use the index: %s", plan)
	}
	// Statistics sample large values without loading them.
	if _, err := s.Exec(ctx, `analyze kind T`, nil); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// A check on a large field counts against the statement's load limit.
func TestCheckLoadLimit(t *testing.T) {
	s := lazySession(t, 1000)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `define kind C { body: text check upper(body) != "" }; create C { body: "small" }`, nil); err != nil {
		t.Fatal(err)
	}
	copyBig := `find F f, C c | where f.name = "b" | update c set body = f.body`
	if _, err := s.Exec(ctx, copyBig, nil); execCode(err) != "limit" {
		t.Fatalf("check over the limit: %v", err)
	}
	if _, err := s.Exec(ctx, `set max_load = 0; `+copyBig, nil); err != nil {
		t.Fatalf("check without a limit: %v", err)
	}
}

// Comparing, searching and cutting large values reads them a piece at a
// time: with a 1000-byte limit every query gives what loading gives.
func TestLazyStreaming(t *testing.T) {
	ctx := context.Background()
	small, unlimited := lazySession(t, 1000), lazySession(t, -1)
	// More texts: ones sharing a long start, bytes, and one past several
	// pieces with a word across a piece boundary.
	extra := map[string]value.Value{
		"x": value.MustText(strings.Repeat("abcd", 25<<10) + "e"),
		"y": value.MustText(strings.Repeat("abcd", 25<<10) + "a" + strings.Repeat("q", 70<<10)),
		"z": value.MustText(strings.Repeat("-", 64<<10-3) + "needle" + strings.Repeat("é", 100<<10)),
		"d": must(value.NewBytes([]byte(strings.Repeat("\x00\x01", 20<<10)), "")),
	}
	for _, s := range []*Session{small, unlimited} {
		if _, err := s.Exec(ctx, `create F { name: "x", body: $x }; create F { name: "y", body: $y };
			create F { name: "z", body: $z }; create F { name: "d", data: $d }; create F { name: "e", data: $d }`, extra); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`find F f | sort f.body, f.name | show f.name`,
		`find F f | sort f.body desc, f.name | show f.name`,
		`find F f, F g | where f.body < g.body | sort f.name, g.name | show f.name, g.name`,
		`find F f, F g | where f.data = g.data and f.name < g.name | show f.name, g.name`,
		`find F f | where f.body between "abcd" and "b" | sort f.name | show f.name`,
		`find F f | where f.body starts with "abcdabcd" | sort f.name | show f.name`,
		`find F f | where f.body ends with "qq" or f.body ends with "é" | sort f.name | show f.name`,
		`find F f | where f.body contains "needle" | show f.name`,
		`find F f | where f.body contains "dabcda" | sort f.name | show f.name`,
		`find F f | where f.body matches "ab(cd)+e$" | show f.name`,
		`find F f | where f.body matches "need?le" | show f.name`,
		`find F f | where f.name = "z" | show substr(f.body, 65530, 12), len(substr(f.body, 100000, 7))`,
		`find F f | where f.body is not null | group by f.body = f.body as same | show same, count()`,
		`find F f | show f.body as b | distinct | show count()`,
		`find F f | where f.body is not null | group by true as all | show min(f.body) = max(f.body), len(min(f.body)), len(max(f.body))`,
		`find F f | where f.data is not null | show f.data as d | distinct | show count()`,
	} {
		want, err := unlimited.Exec(ctx, q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got, err := small.Exec(ctx, q, nil)
		if err != nil {
			t.Errorf("%s with a 1000-byte limit: %v", q, err)
			continue
		}
		if a, b := rowsText(got[len(got)-1]), rowsText(want[len(want)-1]); a != b {
			t.Errorf("%s:\n streamed %s\n loaded   %s", q, a, b)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
