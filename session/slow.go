package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// maxSlowText bounds a statement's text in the slow-query log.
const maxSlowText = 4096

// waits reports whether a statement waits by design (pop … wait, alone
// or as a pipeline step), so its time says nothing about its speed.
func waits(st lang.Stmt) bool {
	switch s := st.(type) {
	case *lang.Pop:
		return s.Wait != nil
	case *lang.Pipeline:
		for _, step := range s.Steps {
			if d, ok := step.(*lang.DataStep); ok && waits(d.Stmt) {
				return true
			}
		}
	}
	return false
}

// logText is a statement as the slow-query log shows it: passwords
// replaced.
func logText(st lang.Stmt) string {
	hidden := &lang.Lit{Val: value.MustText("(hidden)")}
	switch s := st.(type) {
	case *lang.CreateUser:
		cp := *s
		cp.Password = hidden
		st = &cp
	case *lang.AlterUser:
		if s.Password != nil {
			cp := *s
			cp.Password = hidden
			st = &cp
		}
	}
	return lang.Format(st)
}

// noteSlow records a slow statement in the engine's slow-query log.
func (r *Request) noteSlow(st lang.Stmt, d time.Duration, res *query.Result, err error) {
	text := logText(st)
	if len(text) > maxSlowText {
		text = text[:maxSlowText] + "…"
	}
	q := engine.SlowQuery{Time: time.Now(), Duration: d, User: r.s.user, Database: r.s.Database(), Statement: text,
		Plan: planText(r.plan)}
	if res != nil {
		q.Rows, q.Affected = len(res.Rows), res.Affected
	}
	if err != nil {
		q.Err = err.Error()
	}
	r.s.eng.NoteSlowQuery(q)
}

// planText describes how a query's source ran: each variable's access
// path and the rows it bound.
func planText(sp *query.SourcePlan) string {
	if sp == nil {
		return ""
	}
	var parts []string
	for _, v := range sp.Vars {
		parts = append(parts, fmt.Sprintf("%s (%d rows)", v.Detail, v.Rows))
	}
	if sp.Top != "" {
		parts = append(parts, sp.Top)
	}
	parts = append(parts, sp.Warnings...)
	return strings.Join(parts, "; ")
}

// showSlow runs show slow queries. Statements can hold literal values,
// so only server admins see them.
func (s *Session) showSlow(st *lang.ShowStmt) (*query.Result, error) {
	if err := s.needAdmin(st, "show slow queries"); err != nil {
		return nil, err
	}
	r := table("time", "duration", "user", "database", "statement", "rows", "affected", "plan", "error")
	for _, q := range s.eng.SlowQueries() {
		t, _ := value.NewTime(q.Time)
		add(r, t, value.Duration(q.Duration), q.User, orEmpty(q.Database), q.Statement, q.Rows, q.Affected,
			orEmpty(q.Plan), orEmpty(q.Err))
	}
	return r, nil
}

func orEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// showLimits runs show limits: this session's settings, and the
// server's limits they live within.
func (s *Session) showLimits() *query.Result {
	set := s.Settings()
	lim := s.eng.Limits()
	r := table("limit", "value", "applies to")
	none := func(n int64) any {
		if n <= 0 {
			return "none"
		}
		return value.Int(n)
	}
	dur := func(d time.Duration) any {
		if d <= 0 {
			return "none"
		}
		return value.Duration(d)
	}
	add(r, "timeout", dur(set.Timeout), "each statement of this session")
	add(r, "max_memory", none(set.MaxMemory), "each statement of this session, bytes")
	add(r, "max_rows", none(int64(set.MaxRows)), "each result of this session")
	add(r, "max_depth", set.MaxDepth, "open-ended patterns of this session")
	add(r, "server statement timeout", dur(lim.StatementTimeout), "the most a session may set without admin")
	add(r, "server max_memory", none(lim.MaxMemory), "the most a session may set without admin, bytes")
	add(r, "max connections", none(int64(lim.MaxConnections)), "the server (admins may go past it)")
	add(r, "max connections per user", none(int64(lim.MaxUserConnections)), "each user")
	add(r, "max transactions", none(int64(lim.MaxTransactions)), "open at once in a session")
	add(r, "max upload", none(lim.MaxUpload), "one upload, bytes")
	add(r, "upload quota", none(lim.UploadQuota), "uploads waiting to be used, per user, bytes")
	return r
}
