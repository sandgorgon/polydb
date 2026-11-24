package polydb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/session"
	"github.com/sandgorgon/polydb/value"
)

// Embed returns a DB that runs statements on an engine in this program,
// as user (usually security.AdminUser), using database if it isn't "".
func Embed(ctx context.Context, eng *engine.Engine, user, database string) (DB, error) {
	db := &embedded{s: session.New(eng, user)}
	if database != "" {
		if _, err := db.s.ExecStmt(ctx, &lang.Use{Name: database}, nil); err != nil {
			return nil, convertErr(err, -1)
		}
	}
	return db, nil
}

type embedded struct {
	s *session.Session
}

func (db *embedded) Close() error {
	db.s.Close()
	return nil
}

// run executes a script in a request starting in txn.
func (db *embedded) run(ctx context.Context, txn *value.ID, stmts []lang.Stmt, params Params) ([]*query.Result, *value.ID, error) {
	vals, err := ParamValues(params)
	if err != nil {
		return nil, nil, &Error{Code: "type", Message: err.Error(), StatementIndex: -1}
	}
	r, err := db.s.Request(txn)
	if err != nil {
		return nil, nil, convertErr(err, -1)
	}
	defer r.Close()
	r.LazyResults = true // large values become *Blob, read when the application reads them
	var out []*query.Result
	for i, st := range stmts {
		res, err := r.Exec(ctx, st, vals)
		if err != nil {
			return out, nil, convertErr(err, i)
		}
		out = append(out, res)
	}
	id, _ := r.Txn()
	return out, id, nil
}

func parse(text string) ([]lang.Stmt, error) {
	stmts, err := lang.ParseScript(text)
	if err != nil {
		return nil, convertErr(err, 0)
	}
	return stmts, nil
}

func (db *embedded) Query(ctx context.Context, text string, params Params) (Rows, error) {
	return db.query(ctx, nil, text, params)
}

func (db *embedded) Exec(ctx context.Context, text string, params Params) (Result, error) {
	return db.exec(ctx, nil, text, params)
}

func (db *embedded) query(ctx context.Context, txn *value.ID, text string, params Params) (Rows, error) {
	stmts, err := parse(text)
	if err != nil {
		return nil, err
	}
	res, _, err := db.run(ctx, txn, stmts, params)
	if err != nil && len(res) == 0 {
		return nil, err
	}
	// Like a streaming client: the results before a failed statement
	// can be read, and the error appears when Rows reaches it.
	r := newMemRows(res)
	r.err = err
	return r, nil
}

func (db *embedded) exec(ctx context.Context, txn *value.ID, text string, params Params) (Result, error) {
	stmts, err := parse(text)
	if err != nil {
		return Result{}, err
	}
	res, _, err := db.run(ctx, txn, stmts, params)
	return sumResults(res), err
}

func sumResults(res []*query.Result) Result {
	var out Result
	for _, r := range res {
		c := resultOf(r)
		out.RowsAffected += c.RowsAffected
		out.CreatedIDs = append(out.CreatedIDs, c.CreatedIDs...)
		out.Warnings = append(out.Warnings, c.Warnings...)
	}
	return out
}

func resultOf(r *query.Result) Result {
	out := Result{RowsAffected: int64(r.Affected)}
	if len(r.Columns) == 1 && r.Columns[0].Name == "id" && r.Affected > 0 {
		for _, row := range r.Rows {
			if id, ok := row[0].(value.ID); ok {
				out.CreatedIDs = append(out.CreatedIDs, id)
			}
		}
	}
	for _, w := range r.Warnings {
		out.Warnings = append(out.Warnings, w.Msg)
	}
	return out
}

func (db *embedded) Begin(ctx context.Context, opts TxOptions) (Tx, error) {
	_, id, err := db.run(ctx, nil, []lang.Stmt{&lang.Begin{ReadOnly: opts.ReadOnly}}, nil)
	if err != nil {
		return nil, err
	}
	return &embeddedTx{db: db, id: *id}, nil
}

type embeddedTx struct {
	db *embedded
	id value.ID
}

func (t *embeddedTx) Query(ctx context.Context, text string, params Params) (Rows, error) {
	return t.db.query(ctx, &t.id, text, params)
}

func (t *embeddedTx) Exec(ctx context.Context, text string, params Params) (Result, error) {
	return t.db.exec(ctx, &t.id, text, params)
}

func (t *embeddedTx) Commit(ctx context.Context) error {
	_, _, err := t.db.run(ctx, &t.id, []lang.Stmt{&lang.Commit{}}, nil)
	return err
}

func (t *embeddedTx) Rollback(ctx context.Context) error {
	_, _, err := t.db.run(ctx, &t.id, []lang.Stmt{&lang.Rollback{}}, nil)
	return err
}

func (db *embedded) Prepare(ctx context.Context, text string) (Stmt, error) {
	id, _, err := db.s.Prepare(text)
	if err != nil {
		return nil, convertErr(err, 0)
	}
	return &embeddedStmt{db: db, id: id}, nil
}

type embeddedStmt struct {
	db *embedded
	id int64
}

func (s *embeddedStmt) stmt() (lang.Stmt, error) {
	st, ok := s.db.s.Prepared(s.id)
	if !ok {
		return nil, &Error{Code: "stale_statement", Message: "the statement was closed", StatementIndex: -1}
	}
	return st, nil
}

func (s *embeddedStmt) Query(ctx context.Context, params Params) (Rows, error) {
	st, err := s.stmt()
	if err != nil {
		return nil, err
	}
	res, _, err := s.db.run(ctx, nil, []lang.Stmt{st}, params)
	if err != nil {
		return nil, err
	}
	return newMemRows(res), nil
}

func (s *embeddedStmt) Exec(ctx context.Context, params Params) (Result, error) {
	st, err := s.stmt()
	if err != nil {
		return Result{}, err
	}
	res, _, err := s.db.run(ctx, nil, []lang.Stmt{st}, params)
	return sumResults(res), err
}

func (s *embeddedStmt) Close() error {
	s.db.s.ClosePrepared(s.id)
	return nil
}

// convertErr turns a Rhizome or session error into an *Error.
func convertErr(err error, index int) error {
	e := &Error{Code: "internal", Message: err.Error(), StatementIndex: index}
	var le *lang.Error
	var se *session.Error
	switch {
	case errors.As(err, &le):
		e.Code, e.Message, e.Line, e.Column = le.Code, le.Msg, le.Pos.Line, le.Pos.Col
	case errors.As(err, &se):
		e.Code, e.Message, e.Line, e.Column = se.Code, se.Msg, se.Pos.Line, se.Pos.Col
	default:
		e.Code = string(engine.CodeOf(err))
	}
	e.Retryable = e.Code == "conflict" || e.Code == "txn_busy"
	return e
}

// memRows iterates over results held in memory.
type memRows struct {
	mu      sync.Mutex
	res     []*query.Result
	set     int // current result set
	row     int // current row, -1 before the first
	cur     []any
	closed  bool
	err     error // the statement after the last result failed
	reached bool  // NextResultSet moved past the last result
}

func newMemRows(res []*query.Result) *memRows { return &memRows{res: res, row: -1} }

func (r *memRows) Next() bool {
	if r.closed || r.set >= len(r.res) {
		return false
	}
	rs := r.res[r.set]
	if r.row+1 >= len(rs.Rows) {
		return false
	}
	r.row++
	r.cur = make([]any, len(rs.Rows[r.row]))
	for i, c := range rs.Rows[r.row] {
		r.cur[i] = fromQuery(c)
	}
	return true
}

func (r *memRows) Scan(dest ...any) error {
	if r.cur == nil {
		return errors.New("polydb: Scan called without a row; call Next first")
	}
	return Scan(context.Background(), r.cur, dest...)
}

func (r *memRows) Values() []any { return r.cur }

func (r *memRows) Columns() []Column {
	if r.set >= len(r.res) {
		return nil
	}
	var out []Column
	for _, c := range r.res[r.set].Columns {
		out = append(out, Column{Name: c.Name, Type: typeName(c.Type)})
	}
	return out
}

func typeName(t lang.Static) string {
	if t.Form != lang.FormValue || t.Type == nil {
		return "any"
	}
	return t.Type.String()
}

func (r *memRows) NextResultSet() bool {
	if r.set+1 >= len(r.res) {
		r.set = len(r.res)
		r.reached = true
		return false
	}
	r.set++
	r.row, r.cur = -1, nil
	return true
}

func (r *memRows) Result() Result {
	if r.set >= len(r.res) {
		return Result{}
	}
	return resultOf(r.res[r.set])
}

// Err returns the error of the statement after the last result, once
// NextResultSet has moved past the results.
func (r *memRows) Err() error {
	if r.reached {
		return r.err
	}
	return nil
}

func (r *memRows) Close() error {
	r.closed = true
	return nil
}

// fromQuery converts an executor cell to the application form.
func fromQuery(c any) any {
	switch x := c.(type) {
	case *query.Entity:
		e := &Entity{ID: x.ID, Kind: x.Kind, Meta: meta(x.Meta.Created, x.Meta.Updated, int64(x.Meta.Version),
			x.Meta.Labels, x.Meta.Owner, x.Meta.Props)}
		for _, f := range x.Fields {
			pf := &Field{ID: f.ID, Name: f.Name, Type: f.Type.String(), Doc: f.Doc, Declared: f.Declared}
			if f.Value != nil {
				pf.Value = fromQuery(f.Value)
			}
			e.Fields = append(e.Fields, pf)
		}
		return e
	case *query.Relation:
		return &Relation{ID: x.ID, Type: x.Type, From: x.From, To: x.To, Props: props(x.Props)}
	case *query.Path:
		p := &Path{}
		for _, n := range x.Nodes {
			p.Nodes = append(p.Nodes, fromQuery(n))
		}
		for _, r := range x.Rels {
			p.Rels = append(p.Rels, fromQuery(r).(*Relation))
		}
		return p
	case value.Lazy:
		return lazyBlob(x)
	}
	return c
}

// lazyBlob returns a large value still in the blob store as a Blob that
// reads it when the application does.
func lazyBlob(l value.Lazy) *Blob {
	typ, media := l.Code().String(), l.MediaType()
	switch l.Code() {
	case value.CodeText:
		media = "text/plain; charset=utf-8"
	case value.CodeImage:
		if format, err := value.ImageFormat(io.NewSectionReader(l.Src, 0, l.Len())); err == nil {
			media = "image/" + format
		}
	}
	b := NewBlob(typ, l.Len(), media, func(ctx context.Context, off, n int64) ([]byte, error) {
		if off < 0 || n < 0 || off+n > l.Len() {
			return nil, fmt.Errorf("polydb: blob range %d+%d outside %d bytes", off, n, l.Len())
		}
		out := make([]byte, n)
		if _, err := l.Src.ReadAt(out, off); err != nil {
			return nil, err
		}
		return out, nil
	})
	b.lazy = &l
	return b
}

// meta converts metadata values.
func meta(created, updated value.Time, version int64, labels value.Set, owner string, m value.Map) Meta {
	out := Meta{Created: created.Go(), Updated: updated.Go(), Version: version, Owner: owner, Props: props(m)}
	for _, l := range labels.Values() {
		if t, ok := l.(value.Text); ok {
			out.Labels = append(out.Labels, t.Go())
		}
	}
	return out
}

// props converts a map with text keys.
func props(m value.Map) map[string]value.Value {
	if m.Len() == 0 {
		return nil
	}
	out := map[string]value.Value{}
	for k, v := range m.All() {
		if t, ok := k.(value.Text); ok {
			out[t.Go()] = v
		}
	}
	return out
}

// MetaOf converts wire-level metadata; clients use it.
func MetaOf(created, updated value.Time, version int64, labels value.Set, owner string, m value.Map) Meta {
	return meta(created, updated, version, labels, owner, m)
}

// PropsOf converts a map with text keys; clients use it.
func PropsOf(m value.Map) map[string]value.Value { return props(m) }
