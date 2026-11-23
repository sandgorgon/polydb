// Package session runs Rhizome scripts for one client: the current
// database, session handles and settings, explicit transactions, and the
// statements that work on those rather than on data (spec/rhizome.md §4,
// §9 and §12). Data statements and queries go to package query.
package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// Session is one client's connection to an engine. It isn't safe for
// concurrent use.
type Session struct {
	eng      *engine.Engine
	user     string
	db       *engine.DB
	handles  map[string]query.Handle
	settings query.Settings

	tx      *engine.Tx
	txSaved map[string]query.Handle // handles at begin, restored on rollback
	aborted error                   // the error that aborted the open transaction
}

// New starts a session for user. No database is in use until "use".
func New(eng *engine.Engine, user string) *Session {
	return &Session{eng: eng, user: user, handles: map[string]query.Handle{}, settings: query.DefaultSettings}
}

// Database returns the name of the database in use, or "".
func (s *Session) Database() string {
	if s.db == nil {
		return ""
	}
	return s.db.Name()
}

// InTransaction reports whether an explicit transaction is open.
func (s *Session) InTransaction() bool { return s.tx != nil }

// Settings returns the session's settings.
func (s *Session) Settings() query.Settings { return s.settings }

// Handles returns the session's handles.
func (s *Session) Handles() map[string]query.Handle { return maps.Clone(s.handles) }

// Close ends the session, rolling back an open transaction.
func (s *Session) Close() {
	if s.tx != nil {
		s.tx.Rollback()
		s.tx = nil
	}
}

// Error is a statement error, with the index of the statement in the
// script that failed (spec/protocol.md §6.4).
type Error struct {
	Pos   lang.Pos
	Code  string
	Msg   string
	Index int
}

func (e *Error) Error() string {
	if e.Pos.Line == 0 {
		return e.Code + ": " + e.Msg
	}
	return fmt.Sprintf("%s: %s: %s", e.Pos, e.Code, e.Msg)
}

// Exec parses and runs a script. It stops at the first error and returns
// the results of the statements before it. params are the values of
// $parameters.
func (s *Session) Exec(ctx context.Context, src string, params map[string]value.Value) ([]*query.Result, error) {
	stmts, err := lang.ParseScript(src)
	if err != nil {
		return nil, s.wrapErr(err, 0)
	}
	var out []*query.Result
	for i, st := range stmts {
		res, err := s.ExecStmt(ctx, st, params)
		if err != nil {
			return out, s.wrapErr(err, i)
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Session) wrapErr(err error, i int) error {
	var le *lang.Error
	if !errors.As(err, &le) {
		le = &lang.Error{Code: string(engine.CodeOf(err)), Msg: err.Error()}
	}
	return &Error{Pos: le.Pos, Code: le.Code, Msg: le.Msg, Index: i}
}

func sessionErr(st lang.Node, code, format string, args ...any) *lang.Error {
	return &lang.Error{Pos: st.Position(), Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ExecStmt runs one parsed statement.
func (s *Session) ExecStmt(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	if s.aborted != nil {
		switch st.(type) {
		case *lang.Rollback:
		case *lang.Commit:
			err := s.aborted
			s.endTx(false)
			return nil, sessionErr(st, "conflict", "the transaction was rolled back because a statement failed: %v", err)
		default:
			return nil, sessionErr(st, "constraint", "the transaction failed earlier (%v); rollback to continue", s.aborted)
		}
	}
	res, err := s.exec(ctx, st, params)
	if err != nil && s.tx != nil {
		// Any error inside an explicit transaction aborts it, so that the
		// statements after it can't run as if it had worked.
		s.aborted = err
	}
	return res, err
}

func (s *Session) exec(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	switch st := st.(type) {
	case *lang.CreateDatabase:
		if err := s.noTx(st, "create database"); err != nil {
			return nil, err
		}
		_, err := s.eng.CreateDatabase(st.Name)
		return &query.Result{}, s.fail(st, err)
	case *lang.DropDatabase:
		if err := s.noTx(st, "drop database"); err != nil {
			return nil, err
		}
		if err := s.eng.DropDatabase(st.Name); err != nil {
			return nil, s.fail(st, err)
		}
		if s.db != nil && s.db.Name() == st.Name {
			s.db, s.handles = nil, map[string]query.Handle{}
		}
		return &query.Result{}, nil
	case *lang.Use:
		if err := s.noTx(st, "use"); err != nil {
			return nil, err
		}
		db, err := s.eng.Database(st.Name)
		if err != nil {
			return nil, s.fail(st, err)
		}
		if s.db != db {
			s.handles = map[string]query.Handle{} // handles name entities of one database
		}
		s.db = db
		return &query.Result{}, nil
	case *lang.ShowStmt:
		if st.What == "databases" {
			res := &query.Result{Columns: []query.Column{textCol("name")}}
			for _, n := range s.eng.Databases() {
				res.Rows = append(res.Rows, []any{value.MustText(n)})
			}
			return res, nil
		}
	case *lang.SetSetting:
		return s.set(st, params)
	case *lang.Begin:
		if s.tx != nil {
			return nil, sessionErr(st, "constraint", "a transaction is already open; commit or rollback first")
		}
		db, err := s.useDB(st)
		if err != nil {
			return nil, err
		}
		s.tx = db.Begin(engine.TxOptions{ReadOnly: st.ReadOnly, User: s.user})
		s.txSaved = maps.Clone(s.handles)
		return &query.Result{}, nil
	case *lang.Commit:
		if s.tx == nil {
			return nil, sessionErr(st, "constraint", "no transaction is open")
		}
		err := s.tx.Commit()
		s.endTx(err == nil)
		return &query.Result{}, s.fail(st, err)
	case *lang.Rollback:
		if s.tx == nil {
			return nil, sessionErr(st, "constraint", "no transaction is open")
		}
		s.endTx(false)
		return &query.Result{}, nil
	case *lang.CreateUser, *lang.CreateRole, *lang.Grant, *lang.AlterUser:
		return nil, sessionErr(st, "unsupported", "users, roles and grants arrive with security in a later phase")
	case *lang.Drop:
		if st.What == "user" || st.What == "role" {
			return nil, sessionErr(st, "unsupported", "users, roles and grants arrive with security in a later phase")
		}
	case *lang.History:
		return nil, sessionErr(st, "unsupported", "history arrives in a later phase")
	}
	return s.inTx(ctx, st, params)
}

func (s *Session) noTx(st lang.Stmt, what string) error {
	if s.tx != nil {
		return sessionErr(st, "constraint", "%s isn't allowed inside a transaction", what)
	}
	return nil
}

func (s *Session) useDB(st lang.Node) (*engine.DB, error) {
	if s.db == nil {
		return nil, sessionErr(st, "not_found", "no database in use: run use <database> first")
	}
	return s.db, nil
}

// endTx ends the open transaction. A rollback restores the handles bound
// inside it, since they name entities that no longer exist.
func (s *Session) endTx(committed bool) {
	if !committed {
		s.tx.Rollback()
		s.handles = s.txSaved
	}
	s.tx, s.txSaved, s.aborted = nil, nil, nil
}

func (s *Session) fail(st lang.Node, err error) error {
	if err == nil {
		return nil
	}
	var le *lang.Error
	if errors.As(err, &le) {
		return le
	}
	return &lang.Error{Pos: st.Position(), Code: string(engine.CodeOf(err)), Msg: errMsg(err)}
}

func errMsg(err error) string {
	var ee *engine.Error
	if errors.As(err, &ee) {
		return ee.Msg
	}
	return err.Error()
}

func textCol(name string) query.Column {
	return query.Column{Name: name, Type: lang.Static{Type: value.TypeText}}
}

// set changes a session setting.
func (s *Session) set(st *lang.SetSetting, params map[string]value.Value) (*query.Result, error) {
	info, err := lang.Check(&lang.Env{Params: params}, st)
	if err != nil {
		return nil, err
	}
	ev := &lang.Evaluator{Info: info, Params: params, Now: time.Now()}
	v, err := ev.Eval(st.Value, nil)
	if err != nil {
		return nil, err
	}
	switch st.Name {
	case "timeout":
		d, _ := v.(value.Duration)
		if d < 0 {
			return nil, sessionErr(st, "constraint", "timeout can't be negative; 0s means no timeout")
		}
		s.settings.Timeout = time.Duration(d)
	case "max_rows", "max_depth":
		n, _ := v.(value.Int)
		if n < 0 || st.Name == "max_depth" && n == 0 {
			return nil, sessionErr(st, "constraint", "%s must be positive", st.Name)
		}
		if st.Name == "max_rows" {
			s.settings.MaxRows = int(n)
		} else {
			s.settings.MaxDepth = int(min(n, 1000))
		}
	}
	return &query.Result{}, nil
}

// inTx runs a statement that works on the database: in the open
// transaction, or in its own (auto-commit).
func (s *Session) inTx(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	db, err := s.useDB(st)
	if err != nil {
		return nil, err
	}
	if s.settings.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.settings.Timeout)
		defer cancel()
	}
	tx, auto := s.tx, s.tx == nil
	handles := s.handles
	if auto {
		tx = db.Begin(engine.TxOptions{ReadOnly: readOnly(st), User: s.user})
		defer tx.Rollback()
		handles = maps.Clone(s.handles) // kept only if the statement commits
	}
	qc := &query.Context{
		Tx: tx, Handles: handles, Params: params, Settings: s.settings,
		Interrupt: func() error { return interrupted(ctx, s.settings.Timeout) },
	}
	if auto {
		qc.Wait = func(id value.ID, field string, wait time.Duration) (value.Value, error) {
			return db.PopWait(ctx, id, field, wait, engine.TxOptions{User: s.user})
		}
	}
	res, err := s.run(ctx, qc, st)
	if err != nil {
		return nil, err
	}
	if auto {
		if err := tx.Commit(); err != nil {
			return nil, s.fail(st, err)
		}
		s.handles = handles
	}
	return res, nil
}

// interrupted reports a timeout or cancellation as a limit error.
func interrupted(ctx context.Context, timeout time.Duration) error {
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return &engine.Error{Code: engine.CodeLimit, Msg: fmt.Sprintf("the statement took longer than the timeout (%s)", timeout)}
	case err != nil:
		return &engine.Error{Code: engine.CodeLimit, Msg: "the statement was cancelled"}
	}
	return nil
}

// readOnly reports whether a statement only reads, so it can run in a
// read-only transaction.
func readOnly(st lang.Stmt) bool {
	switch st := st.(type) {
	case *lang.Pipeline:
		for _, step := range st.Steps {
			if _, ok := step.(*lang.DataStep); ok {
				return false
			}
		}
		return true
	case *lang.Describe, *lang.ShowStmt:
		return true
	case *lang.Explain:
		return !st.Analyze && readOnly(st.Stmt)
	}
	return false
}

// run dispatches database statements: introspection here, the rest to
// the executor.
func (s *Session) run(ctx context.Context, qc *query.Context, st lang.Stmt) (*query.Result, error) {
	switch st := st.(type) {
	case *lang.Describe:
		return s.describe(qc, st)
	case *lang.ShowStmt:
		return s.show(qc, st)
	case *lang.Explain:
		return s.explain(ctx, qc, st)
	}
	return query.Exec(qc, st)
}
