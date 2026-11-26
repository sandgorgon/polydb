package engine

import (
	"slices"
	"time"
)

// The slow-query log (spec/rhizome.md §12): statements that take at
// least Options.SlowQuery, with their plan. The engine keeps the latest
// slowQueryKeep in memory (show slow queries) and hands each to
// Options.OnSlowQuery, which the server writes to its log.

// DefaultSlowQuery is Options.SlowQuery's default.
const DefaultSlowQuery = time.Second

// slowQueryKeep is how many slow statements the engine remembers.
const slowQueryKeep = 256

// SlowQuery is one slow statement.
type SlowQuery struct {
	Time      time.Time // when it finished
	Duration  time.Duration
	User      string
	Database  string // "" if none was in use
	Statement string // as Rhizome text; parameters by name, never their values
	Rows      int    // returned
	Affected  int
	Plan      string // how a query's source ran, "" for other statements
	Err       string // if it failed
}

// SlowQueryThreshold returns how long a statement takes to be logged as
// slow, or 0 when the log is off.
func (e *Engine) SlowQueryThreshold() time.Duration { return max(e.opts.SlowQuery, 0) }

// NoteSlowQuery records a slow statement.
func (e *Engine) NoteSlowQuery(q SlowQuery) {
	e.slowMu.Lock()
	if len(e.slow) == slowQueryKeep {
		e.slow = slices.Delete(e.slow, 0, 1)
	}
	e.slow = append(e.slow, q)
	e.slowMu.Unlock()
	if e.opts.OnSlowQuery != nil {
		e.opts.OnSlowQuery(q)
	}
}

// SlowQueries returns the slow statements the engine remembers, newest
// first.
func (e *Engine) SlowQueries() []SlowQuery {
	e.slowMu.Lock()
	defer e.slowMu.Unlock()
	out := slices.Clone(e.slow)
	slices.Reverse(out)
	return out
}
