package session

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/lsm"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// The .rhz scripts in testdata are Rhizome scripts with expectations in
// comments after a statement:
//
//	find Person p | show p.name;
//	// => "Ann"
//	// => "Bob"
//	update @x set age = "old";
//	// error type: can't be stored
//
// "// => row" lines give the expected rows in order, with cells joined by
// " | "; "// => (none)" means no rows. "// error code" or "// error code:
// text" expects an error with that code whose message contains the text.
// "// warning: text" expects a warning, and "// affected: n" a count of
// changes. A statement without expectations must succeed. Each script
// runs in a fresh engine as the admin user, with database "test" in use
// and a fixed clock (2026-11-22T12:00:00Z). "// as name" continues in a
// new session for that user, using "test" if the user may. "// clock
// <RFC 3339 time>" moves the clock, for history.
//
// IDs change from run to run, so rendered rows show every ID as #ID.
//
// go test -update rewrites the "// =>" lines with the actual rows.

var update = flag.Bool("update", false, "rewrite // => lines in testdata/*.rhz with actual results")

func TestScripts(t *testing.T) {
	files, err := filepath.Glob("testdata/*.rhz")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	// Every script runs in memory and on disk (a simulated file system,
	// with sizes small enough that memtables flush and tables compact).
	for _, disk := range []bool{false, true} {
		mode := "memory"
		if disk {
			mode = "disk"
		}
		for _, file := range files {
			t.Run(mode+"/"+filepath.Base(file), func(t *testing.T) { runScript(t, file, disk) })
		}
	}
}

// expect is the expectations after one statement.
type expect struct {
	line     int // line of the first expectation
	rows     []string
	hasRows  bool
	errCode  string
	errMsg   string
	hasErr   bool
	warnings []string
	affected int
	hasAff   bool
}

func (e *expect) empty() bool { return !e.hasRows && !e.hasErr && !e.hasAff && len(e.warnings) == 0 }

func newScriptSession(t *testing.T, disk bool) *Session {
	t.Helper()
	s, _ := newClockedSession(t, disk)
	return s
}

// testClock is a clock tests move while background work reads it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *testClock) Add(d time.Duration) { c.Set(c.Now().Add(d)) }

// newClockedSession also returns the engine's clock, which the caller may
// move.
func newClockedSession(t *testing.T, disk bool) (*Session, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	o := engine.Options{Now: clock.Now, MaxPopWait: time.Minute,
		PasswordIterations: security.MinIterations}
	if disk {
		o.Dir, o.FS = "/data", vfs.NewMem()
		o.Storage = lsm.Options{MemtableSize: 1 << 10, TableSize: 4 << 10, LevelBaseSize: 16 << 10, BlockSize: 512}
	}
	eng, err := engine.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	s := New(eng, security.AdminUser)
	if _, err := s.Exec(context.Background(), "create database test; use test;", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, clock
}

func runScript(t *testing.T, file string, disk bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s, clock := newClockedSession(t, disk)
	lines := strings.Split(string(data), "\n")
	var out []string // the rewritten file, with -update
	var chunk []string
	chunkStart := 1
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if when, ok := strings.CutPrefix(line, "// clock "); ok {
			runChunk(t, s, file, chunkStart, strings.Join(chunk, "\n"), &expect{})
			c, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(when))
			if err != nil {
				t.Fatalf("%s:%d: %v", file, i+1, err)
			}
			clock.Set(c)
			out = append(out, lines[i])
			chunk, chunkStart = nil, i+2
			i++
			continue
		}
		if user, ok := strings.CutPrefix(line, "// as "); ok {
			runChunk(t, s, file, chunkStart, strings.Join(chunk, "\n"), &expect{})
			s = New(s.eng, strings.TrimSpace(user))
			s.ExecStmt(context.Background(), &lang.Use{Name: "test"}, nil)
			t.Cleanup(s.Close)
			out = append(out, lines[i])
			chunk, chunkStart = nil, i+2
			i++
			continue
		}
		if !isExpect(line) {
			chunk = append(chunk, lines[i])
			out = append(out, lines[i])
			i++
			continue
		}
		// An expectation block applies to the last statement of the chunk.
		exp := &expect{line: i + 1}
		j := i
		for ; j < len(lines) && isExpect(strings.TrimSpace(lines[j])); j++ {
			parseExpect(t, file, j+1, strings.TrimSpace(lines[j]), exp)
		}
		got := runChunk(t, s, file, chunkStart, strings.Join(chunk, "\n"), exp)
		if *update && exp.hasRows {
			for k := i; k < j; k++ {
				if !strings.HasPrefix(strings.TrimSpace(lines[k]), "// =>") {
					out = append(out, lines[k])
				}
			}
			indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
			if len(got) == 0 {
				out = append(out, indent+"// => (none)")
			}
			for _, r := range got {
				out = append(out, indent+"// => "+r)
			}
		} else {
			out = append(out, lines[i:j]...)
		}
		chunk, chunkStart, i = nil, j+1, j
	}
	if strings.TrimSpace(strings.Join(chunk, "\n")) != "" {
		runChunk(t, s, file, chunkStart, strings.Join(chunk, "\n"), &expect{})
	}
	if *update {
		if err := os.WriteFile(file, []byte(strings.Join(out, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func isExpect(line string) bool {
	for _, p := range []string{"// =>", "// error", "// warning:", "// affected:"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func parseExpect(t *testing.T, file string, lineNo int, line string, e *expect) {
	switch {
	case strings.HasPrefix(line, "// =>"):
		e.hasRows = true
		if r := strings.TrimSpace(strings.TrimPrefix(line, "// =>")); r != "(none)" {
			e.rows = append(e.rows, r)
		}
	case strings.HasPrefix(line, "// error"):
		e.hasErr = true
		rest := strings.TrimSpace(strings.TrimPrefix(line, "// error"))
		e.errCode, e.errMsg, _ = strings.Cut(rest, ":")
		e.errCode, e.errMsg = strings.TrimSpace(e.errCode), strings.TrimSpace(e.errMsg)
	case strings.HasPrefix(line, "// warning:"):
		e.warnings = append(e.warnings, strings.TrimSpace(strings.TrimPrefix(line, "// warning:")))
	case strings.HasPrefix(line, "// affected:"):
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "// affected:")))
		if err != nil {
			t.Fatalf("%s:%d: bad affected count", file, lineNo)
		}
		e.affected, e.hasAff = n, true
	}
}

// runChunk runs the statements of a chunk: all must succeed except that
// the last is checked against exp. It returns the last statement's rows.
func runChunk(t *testing.T, s *Session, file string, startLine int, src string, exp *expect) []string {
	t.Helper()
	stmts, err := lang.ParseScript(src)
	where := func(p lang.Pos) string { return fmt.Sprintf("%s:%d", file, startLine+p.Line-1) }
	if err != nil {
		var le *lang.Error
		if errors.As(err, &le) && exp.hasErr && le.Code == exp.errCode && strings.Contains(le.Msg, exp.errMsg) {
			return nil
		}
		t.Errorf("%s: %v", file, err)
		return nil
	}
	if len(stmts) == 0 {
		if !exp.empty() {
			t.Errorf("%s:%d: expectation without a statement", file, exp.line)
		}
		return nil
	}
	ctx := context.Background()
	for _, st := range stmts[:len(stmts)-1] {
		if _, err := s.ExecStmt(ctx, st, nil); err != nil {
			t.Errorf("%s: %s: %v", where(st.Position()), lang.Format(st), err)
		}
	}
	last := stmts[len(stmts)-1]
	res, err := s.ExecStmt(ctx, last, nil)
	at := fmt.Sprintf("%s:%d: %s", file, exp.line, lang.Format(last))
	if exp.empty() {
		at = where(last.Position()) + ": " + lang.Format(last)
	}
	if exp.hasErr {
		var le *lang.Error
		switch {
		case err == nil:
			t.Errorf("%s: succeeded, want error %s: %s", at, exp.errCode, exp.errMsg)
		case !errors.As(err, &le) || le.Code != exp.errCode || !strings.Contains(le.Msg, exp.errMsg):
			t.Errorf("%s: got error %v, want %s: %s", at, err, exp.errCode, exp.errMsg)
		}
		return nil
	}
	if err != nil {
		t.Errorf("%s: %v", at, err)
		return nil
	}
	got := render(res)
	if exp.hasRows && !*update && strings.Join(got, "\n") != strings.Join(exp.rows, "\n") {
		t.Errorf("%s:\n got: %s\nwant: %s", at, strings.Join(got, "\n      "), strings.Join(exp.rows, "\n      "))
	}
	if exp.hasAff && res.Affected != exp.affected {
		t.Errorf("%s: affected %d, want %d", at, res.Affected, exp.affected)
	}
	for _, w := range exp.warnings {
		found := false
		for _, rw := range res.Warnings {
			found = found || strings.Contains(rw.Msg, w)
		}
		if !found {
			t.Errorf("%s: no warning containing %q (got %v)", at, w, res.Warnings)
		}
	}
	return got
}

// render shows each row as its cells joined by " | ". Entities show as
// Kind{field: value, …}, relations as type{props}, paths by node names.
func render(r *query.Result) []string {
	var out []string
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = renderCell(c)
		}
		out = append(out, idPattern.ReplaceAllString(strings.Join(cells, " | "), "#ID"))
	}
	return out
}

var idPattern = regexp.MustCompile(`#[0-9A-HJKMNP-TV-Z]{26}`)

func renderCell(c any) string {
	switch c := c.(type) {
	case *query.Entity:
		var fs []string
		for _, f := range c.Fields {
			if f.Value != nil {
				fs = append(fs, f.Name+": "+f.Value.String())
			}
		}
		kind := c.Kind
		if kind == "" {
			kind = "entity"
		}
		return kind + "{" + strings.Join(fs, ", ") + "}"
	case *query.Relation:
		if c.Props.Len() == 0 {
			return c.Type
		}
		return c.Type + c.Props.String()
	case *query.Path:
		var parts []string
		for i, n := range c.Nodes {
			if i > 0 {
				parts = append(parts, "~"+c.Rels[i-1].Type+"~>")
			}
			parts = append(parts, nodeName(n))
		}
		return strings.Join(parts, " ")
	case value.Value:
		return c.String()
	}
	return fmt.Sprint(c)
}

func nodeName(n any) string {
	if e, ok := n.(*query.Entity); ok {
		for _, f := range e.Fields {
			if f.Name == "name" && f.Value != nil {
				return f.Value.String()
			}
		}
		return e.Kind
	}
	return fmt.Sprint(n)
}
