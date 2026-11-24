// Package session runs Rhizome statements for one client connection:
// the database in use, session handles and settings, prepared statements,
// explicit transactions, and the statements that work on those rather
// than on data (spec/rhizome.md §4, §9 and §12; spec/protocol.md §5 and
// §6.3). Data statements and queries go to package query.
//
// A session is safe for concurrent use: the protocol runs requests from
// several streams at once. Each request runs in no transaction
// (auto-commit) or in one explicit transaction, identified by ID; a
// transaction runs one request at a time.
package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// Session is one client's connection to an engine.
type Session struct {
	eng  *engine.Engine
	user string
	ids  *value.IDGenerator

	mu       sync.Mutex
	db       *engine.DB
	handles  map[string]query.Handle
	settings query.Settings
	txns     map[value.ID]*Txn
	cur      *Txn // the transaction Exec and ExecStmt continue in
	prepared map[int64]lang.Stmt
	nextStmt int64

	// Directory, if set, lists the server's sessions for show sessions.
	Directory func() []Info
	// NoWait refuses pop … wait, for clients that didn't negotiate the
	// queue-wait feature.
	NoWait bool
}

// Info describes a session for show sessions.
type Info struct {
	ID        string
	User      string
	Database  string
	Remote    string
	Connected time.Time
	Requests  int
}

// Txn is an explicit transaction.
type Txn struct {
	ID      value.ID
	db      *engine.DB
	tx      *engine.Tx
	handles map[string]query.Handle // handles bound inside it
	aborted error
	busy    bool
	used    time.Time
}

// New starts a session for user, whose permissions come from the
// engine's system store; embedded programs usually run as
// security.AdminUser. No database is in use until "use".
func New(eng *engine.Engine, user string) *Session {
	settings := query.DefaultSettings
	settings.MaxLoad = max(eng.MaxLoad(), 0)
	return &Session{eng: eng, user: user, ids: value.NewIDGenerator(0), handles: map[string]query.Handle{},
		settings: settings, txns: map[value.ID]*Txn{}, prepared: map[int64]lang.Stmt{}}
}

// User returns the session's user.
func (s *Session) User() string { return s.user }

// Database returns the name of the database in use, or "".
func (s *Session) Database() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ""
	}
	return s.db.Name()
}

// InTransaction reports whether Exec and ExecStmt are inside an explicit
// transaction, and whether it has failed.
func (s *Session) InTransaction() (open, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return false, false
	}
	return true, s.cur.aborted != nil
}

// Settings returns the session's settings.
func (s *Session) Settings() query.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// Handles returns the session's handles.
func (s *Session) Handles() map[string]query.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.handles)
}

// Close ends the session, rolling back every open transaction.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.txns {
		t.tx.Rollback()
		delete(s.txns, id)
	}
	s.cur = nil
}

// ExpireIdle rolls back transactions that have been idle longer than d
// and returns their IDs (spec/protocol.md §6.3).
func (s *Session) ExpireIdle(d time.Duration) []value.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []value.ID
	for id, t := range s.txns {
		if !t.busy && time.Since(t.used) > d {
			t.tx.Rollback()
			delete(s.txns, id)
			if s.cur == t {
				s.cur = nil
			}
			out = append(out, id)
		}
	}
	return out
}

// Prepare parses one statement and keeps it for Execute.
func (s *Session) Prepare(src string) (int64, lang.Stmt, error) {
	st, err := lang.ParseStatement(src)
	if err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextStmt++
	s.prepared[s.nextStmt] = st
	return s.nextStmt, st, nil
}

// Prepared returns a prepared statement.
func (s *Session) Prepared(id int64) (lang.Stmt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.prepared[id]
	return st, ok
}

// ClosePrepared frees a prepared statement.
func (s *Session) ClosePrepared(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.prepared[id]
	delete(s.prepared, id)
	return ok
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

// Exec parses and runs a script in the session's current transaction. It
// stops at the first error and returns the results of the statements
// before it. params are the values of $parameters.
func (s *Session) Exec(ctx context.Context, src string, params map[string]value.Value) ([]*query.Result, error) {
	stmts, err := lang.ParseScript(src)
	if err != nil {
		return nil, wrapErr(err, 0)
	}
	var out []*query.Result
	for i, st := range stmts {
		res, err := s.ExecStmt(ctx, st, params)
		if err != nil {
			return out, wrapErr(err, i)
		}
		out = append(out, res)
	}
	return out, nil
}

func wrapErr(err error, i int) error {
	var le *lang.Error
	if !errors.As(err, &le) {
		le = &lang.Error{Code: string(engine.CodeOf(err)), Msg: err.Error()}
	}
	return &Error{Pos: le.Pos, Code: le.Code, Msg: le.Msg, Index: i}
}

func sessionErr(st lang.Node, code, format string, args ...any) *lang.Error {
	return &lang.Error{Pos: st.Position(), Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ExecStmt runs one parsed statement in the session's current
// transaction.
func (s *Session) ExecStmt(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	s.mu.Lock()
	var id *value.ID
	if s.cur != nil {
		id = &s.cur.ID
	}
	s.mu.Unlock()
	r, err := s.Request(id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	res, err := r.Exec(ctx, st, params)
	s.mu.Lock()
	s.cur = r.txn
	s.mu.Unlock()
	return res, err
}

// Request is one client request: statements that start in a transaction
// (or none) and may begin, commit or roll back one.
type Request struct {
	s     *Session
	txn   *Txn
	state string
	// Settings, if set, replace the session's settings for this request
	// (QUERY options).
	Settings *query.Settings
	// LazyResults leaves large values in results as value.Lazy, for a
	// caller that sends or reads them a piece at a time (query.Context).
	LazyResults bool
	closed      bool
}

// Request starts a request in transaction txn (nil for none). A
// transaction runs one request at a time.
func (s *Session) Request(txn *value.ID) (*Request, error) {
	r := &Request{s: s, state: "none"}
	if txn == nil {
		return r, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[*txn]
	switch {
	case !ok:
		return nil, &lang.Error{Code: "txn_not_found", Msg: "no open transaction " + txn.String()}
	case t.busy:
		return nil, &lang.Error{Code: "txn_busy", Msg: "the transaction is running another request"}
	}
	t.busy, t.used = true, time.Now()
	r.txn, r.state = t, "open"
	if t.aborted != nil {
		r.state = "failed"
	}
	return r, nil
}

// Close ends the request, releasing its transaction for the next one.
// Calls after the first do nothing.
func (r *Request) Close() {
	if r.txn == nil || r.closed {
		return
	}
	r.closed = true
	r.s.mu.Lock()
	r.txn.busy, r.txn.used = false, time.Now()
	r.s.mu.Unlock()
}

// Txn returns the transaction the request is in after its last
// statement, and its state: none, open, failed, committed or
// rolled_back.
func (r *Request) Txn() (*value.ID, string) {
	if r.txn == nil {
		return nil, r.state
	}
	return &r.txn.ID, r.state
}

// Exec runs one statement.
func (r *Request) Exec(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	if t := r.txn; t != nil && t.aborted != nil {
		switch st.(type) {
		case *lang.Rollback:
		case *lang.Commit:
			err := t.aborted
			r.end(false)
			return nil, sessionErr(st, "txn_aborted", "the transaction was rolled back because a statement failed: %v", err)
		default:
			return nil, sessionErr(st, "txn_aborted", "the transaction failed earlier (%v); roll it back to continue", t.aborted)
		}
	}
	res, err := r.exec(ctx, st, params)
	if err != nil && r.txn != nil {
		// Any error inside an explicit transaction aborts it, so that the
		// statements after it can't run as if it had worked.
		r.s.mu.Lock()
		r.txn.aborted = err
		r.s.mu.Unlock()
		r.state = "failed"
	}
	return res, err
}

// end ends the request's transaction. A rollback drops the handles bound
// inside it, since they name entities that no longer exist; a commit
// makes them session handles.
func (r *Request) end(committed bool) {
	s, t := r.s, r.txn
	if !committed {
		t.tx.Rollback()
	}
	s.mu.Lock()
	if committed && s.db == t.db {
		maps.Copy(s.handles, t.handles)
	}
	delete(s.txns, t.ID)
	s.mu.Unlock()
	r.txn = nil
	r.state = "rolled_back"
	if committed {
		r.state = "committed"
	}
}

func (r *Request) noTx(st lang.Stmt, what string) error {
	if r.txn != nil {
		return sessionErr(st, "constraint", "%s isn't allowed inside a transaction", what)
	}
	return nil
}

// db returns the database a statement works on: its transaction's, or
// the session's.
func (r *Request) db(st lang.Node) (*engine.DB, error) {
	if r.txn != nil {
		return r.txn.db, nil
	}
	r.s.mu.Lock()
	db := r.s.db
	r.s.mu.Unlock()
	if db == nil {
		return nil, sessionErr(st, "not_found", "no database in use: run use <database> first")
	}
	return db, nil
}

func (r *Request) exec(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	switch st := st.(type) {
	case *lang.Backup:
		return r.backup(ctx, st, params)
	case *lang.Restore:
		return r.restore(ctx, st, params)
	case *lang.CreateDatabase:
		if err := r.noTx(st, "create database"); err != nil {
			return nil, err
		}
		if err := s.needAdmin(st, "create database"); err != nil {
			return nil, err
		}
		_, err := s.eng.CreateDatabase(st.Name)
		return &query.Result{}, fail(st, err)
	case *lang.DropDatabase:
		if err := r.noTx(st, "drop database"); err != nil {
			return nil, err
		}
		if err := s.needAdmin(st, "drop database"); err != nil {
			return nil, err
		}
		if err := s.eng.DropDatabase(st.Name); err != nil {
			return nil, fail(st, err)
		}
		s.mu.Lock()
		if s.db != nil && s.db.Name() == st.Name {
			s.db, s.handles = nil, map[string]query.Handle{}
		}
		s.mu.Unlock()
		return &query.Result{}, nil
	case *lang.Use:
		if err := r.noTx(st, "use"); err != nil {
			return nil, err
		}
		db, err := s.eng.Database(st.Name)
		if err != nil {
			return nil, fail(st, err)
		}
		if !s.eng.Security().CanSee(s.user, st.Name) {
			return nil, denied(st, "you don't have permission on database %s", st.Name)
		}
		s.mu.Lock()
		if s.db != db {
			s.handles = map[string]query.Handle{} // handles name entities of one database
		}
		s.db = db
		s.mu.Unlock()
		return &query.Result{}, nil
	case *lang.ShowStmt:
		if st.What == "databases" {
			res := &query.Result{Columns: []query.Column{textCol("name")}}
			for _, n := range s.eng.Databases() {
				if s.eng.Security().CanSee(s.user, n) {
					res.Rows = append(res.Rows, []any{value.MustText(n)})
				}
			}
			return res, nil
		}
		switch st.What {
		case "users", "roles", "grants", "keys", "sessions":
			return r.env(nil).showSecurity(st)
		}
	case *lang.SetSetting:
		return s.set(st, params)
	case *lang.Begin:
		if r.txn != nil {
			return nil, sessionErr(st, "constraint", "a transaction is already open; commit or roll back first")
		}
		db, err := r.db(st)
		if err != nil {
			return nil, err
		}
		t := &Txn{ID: s.ids.New(), db: db, tx: db.Begin(engine.TxOptions{ReadOnly: st.ReadOnly, User: s.user}),
			handles: map[string]query.Handle{}, busy: true, used: time.Now()}
		s.mu.Lock()
		s.txns[t.ID] = t
		s.mu.Unlock()
		r.txn, r.state = t, "open"
		return &query.Result{}, nil
	case *lang.Commit:
		if r.txn == nil {
			return nil, sessionErr(st, "constraint", "no transaction is open")
		}
		err := r.txn.tx.Commit()
		r.end(err == nil)
		return &query.Result{}, fail(st, err)
	case *lang.Rollback:
		if r.txn == nil {
			return nil, sessionErr(st, "constraint", "no transaction is open")
		}
		r.end(false)
		return &query.Result{}, nil
	case *lang.CreateUser, *lang.CreateRole, *lang.Grant, *lang.AlterUser:
		if err := r.noSecurityTx(st); err != nil {
			return nil, err
		}
		return r.env(nil).security(st, params)
	case *lang.Drop:
		if st.What == "user" || st.What == "role" {
			if err := r.noSecurityTx(st); err != nil {
				return nil, err
			}
			return r.env(nil).security(st, params)
		}
	}
	return r.inTx(ctx, st, params)
}

func (r *Request) noSecurityTx(st lang.Stmt) error {
	if r.txn != nil {
		return sessionErr(st, "constraint", "users, roles and grants can't change inside a transaction")
	}
	return nil
}

// env returns the view of the session a statement runs with: its user
// and the database it works on (nil: the session's).
func (r *Request) env(db *engine.DB) *env {
	if db == nil {
		r.s.mu.Lock()
		db = r.s.db
		r.s.mu.Unlock()
	}
	return &env{Session: r.s, db: db}
}

// env is a session seen from one statement: the database it works on is
// fixed for the statement, whatever other requests do meanwhile.
type env struct {
	*Session
	db *engine.DB
}

// Database returns the name of the statement's database, or "".
func (e *env) Database() string {
	if e.db == nil {
		return ""
	}
	return e.db.Name()
}

func fail(st lang.Node, err error) error {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	switch st.Name {
	case "timeout":
		d, _ := v.(value.Duration)
		if d < 0 {
			return nil, sessionErr(st, "constraint", "timeout can't be negative; 0s means no timeout")
		}
		s.settings.Timeout = time.Duration(d)
	case "max_load":
		n, _ := v.(value.Int)
		if n < 0 {
			return nil, sessionErr(st, "constraint", "max_load can't be negative; 0 means no limit")
		}
		s.settings.MaxLoad = int64(n)
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

// inTx runs a statement that works on the database: in the request's
// transaction, or in its own (auto-commit).
func (r *Request) inTx(ctx context.Context, st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	db, err := r.db(st)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	settings := s.settings
	base := maps.Clone(s.handles)
	noWait := s.NoWait
	s.mu.Unlock()
	if r.Settings != nil {
		settings = *r.Settings
	}
	if settings.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settings.Timeout)
		defer cancel()
	}
	// The statement sees the session's handles plus those bound in its
	// transaction; the ones it binds are kept if it commits.
	var tx *engine.Tx
	auto := r.txn == nil
	if auto {
		tx = db.Begin(engine.TxOptions{ReadOnly: readOnly(st), User: s.user})
		defer tx.Rollback()
	} else {
		tx = r.txn.tx
		s.mu.Lock()
		maps.Copy(base, r.txn.handles)
		s.mu.Unlock()
	}
	view := maps.Clone(base)
	e := r.env(db)
	qc := &query.Context{
		Tx: tx, Handles: view, Params: params, Settings: settings,
		Interrupt:   func() error { return interrupted(ctx, settings.Timeout) },
		Access:      e.access(),
		LazyResults: r.LazyResults,
	}
	if auto && !noWait {
		qc.Wait = func(id value.ID, field string, wait time.Duration) (value.Value, error) {
			return db.PopWait(ctx, id, field, wait, engine.TxOptions{User: s.user})
		}
	}
	res, err := e.run(ctx, qc, st)
	if err != nil {
		return nil, err
	}
	bound := map[string]query.Handle{}
	for k, v := range view {
		if old, ok := base[k]; !ok || old != v {
			bound[k] = v
		}
	}
	if auto {
		if err := tx.Commit(); err != nil {
			return nil, fail(st, err)
		}
		s.mu.Lock()
		if s.db == db {
			maps.Copy(s.handles, bound)
		}
		s.mu.Unlock()
	} else {
		s.mu.Lock()
		maps.Copy(r.txn.handles, bound)
		s.mu.Unlock()
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
func (e *env) run(ctx context.Context, qc *query.Context, st lang.Stmt) (*query.Result, error) {
	if err := e.authorize(qc, st); err != nil {
		return nil, err
	}
	switch st := st.(type) {
	case *lang.Describe:
		return e.describe(qc, st)
	case *lang.ShowStmt:
		return e.show(qc, st)
	case *lang.Analyze:
		return e.analyzeKind(qc, st)
	case *lang.Explain:
		return e.explain(ctx, qc, st)
	}
	return query.Exec(qc, st)
}
