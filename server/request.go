package server

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/session"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// batchSize is the encoded size the server aims for per ROWS message.
const batchSize = 64 << 10

// blobChunk is the most data one BLOB_DATA message carries.
const blobChunk = 256 << 10

// serve runs one request on its stream.
func (c *conn) serve(st *stream, m wire.Message) {
	var err error
	switch m := m.(type) {
	case *wire.Query:
		err = c.query(st, m.Text, nil, m.Params, m.Txn, m.Options)
	case *wire.Execute:
		stmt, ok := c.sess.Prepared(m.StatementID)
		if !ok {
			err = &wire.Error{Code: "stale_statement", Message: "no prepared statement with that id; prepare it again", StatementIndex: -1, Retryable: true}
			break
		}
		err = c.query(st, "", []lang.Stmt{stmt}, m.Params, m.Txn, m.Options)
	case *wire.Prepare:
		err = c.prepare(st, m.Text)
	case *wire.CloseStatement:
		c.sess.ClosePrepared(m.StatementID)
		err = c.send(st, &wire.ResultEnd{TxnState: wire.TxnNone}, true)
	case *wire.BlobRead:
		err = c.blobRead(st, m)
	}
	if err == nil {
		return
	}
	var we *wire.Error
	if !errors.As(err, &we) {
		if st.ctx.Err() != nil {
			return // the connection is gone
		}
		we = &wire.Error{Code: "internal", Message: err.Error(), StatementIndex: -1}
	}
	c.send(st, we, true)
}

// send writes a message on a stream. The stream's last message frees
// its place first, so a client that has seen a stream end can always
// open another.
func (c *conn) send(st *stream, m wire.Message, end bool) error {
	if end {
		c.release(st)
	}
	return c.w.WriteMessage(st.id, m, end)
}

// sendFlow writes a flow-controlled message (ROWS, BLOB_DATA).
func (c *conn) sendFlow(st *stream, m wire.Message, end bool) error {
	if end {
		c.release(st)
	}
	return c.w.WriteBody(st.id, m.Type(), wire.Encode(m), end, func(want int) (int, error) {
		return wire.TakeBoth(st.ctx, st.win, c.connWin, want)
	})
}

// toError turns a statement error into an ERROR message.
func toError(err error, index int) *wire.Error {
	we := &wire.Error{Code: "internal", Message: err.Error(), StatementIndex: int64(index)}
	var le *lang.Error
	var se *session.Error
	switch {
	case errors.As(err, &le):
		we.Code, we.Message, we.Line, we.Column = le.Code, le.Msg, int64(le.Pos.Line), int64(le.Pos.Col)
	case errors.As(err, &se):
		we.Code, we.Message, we.Line, we.Column = se.Code, se.Msg, int64(se.Pos.Line), int64(se.Pos.Col)
	}
	we.Retryable = we.Code == "conflict" || we.Code == "txn_busy"
	return we
}

// settings applies a request's options (timeout, max_rows) to the
// session's settings.
func (c *conn) settings(opts map[string]value.Value) (*query.Settings, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	s := c.sess.Settings()
	for k, v := range opts {
		switch k {
		case "timeout":
			d, ok := v.(value.Duration)
			if !ok || d < 0 {
				return nil, &wire.Error{Code: "type", Message: "the timeout option is a duration", StatementIndex: -1}
			}
			s.Timeout = time.Duration(d)
		case "max_rows":
			n, ok := v.(value.Int)
			if !ok || n < 0 {
				return nil, &wire.Error{Code: "type", Message: "the max_rows option is an int ≥ 0", StatementIndex: -1}
			}
			s.MaxRows = int(n)
		}
	}
	return &s, nil
}

// query runs a script (text, or parsed statements) and streams results.
func (c *conn) query(st *stream, text string, stmts []lang.Stmt, params map[string]value.Value, txn *value.ID,
	opts map[string]value.Value) error {
	if stmts == nil {
		var err error
		if stmts, err = lang.ParseScript(text); err != nil {
			return toError(err, 0)
		}
	}
	settings, err := c.settings(opts)
	if err != nil {
		return err
	}
	r, err := c.sess.Request(txn)
	if err != nil {
		return toError(err, -1)
	}
	defer r.Close()
	r.Settings = settings
	if len(stmts) == 0 {
		id, state := r.Txn()
		return c.send(st, &wire.ResultEnd{Txn: id, TxnState: wire.TxnState(state)}, true)
	}
	cancelled := func(i int) error {
		return &wire.Error{Code: "cancelled", Message: "the request was cancelled", StatementIndex: int64(i)}
	}
	for i, stmt := range stmts {
		// A cancel stops the statements that haven't run; one that
		// finished before the cancel arrived still reports its result.
		if st.cancelled.Load() {
			return cancelled(i)
		}
		began := time.Now()
		res, err := r.Exec(st.ctx, stmt, params)
		if err != nil {
			if st.cancelled.Load() {
				return cancelled(i)
			}
			return toError(err, i)
		}
		if err := c.result(st, i, res, r, began, i == len(stmts)-1); err != nil {
			return err
		}
	}
	return nil
}

// result streams one statement's result: header, row batches and end.
func (c *conn) result(st *stream, index int, res *query.Result, r *session.Request, began time.Time, last bool) error {
	cols := make([]wire.Column, len(res.Columns))
	for i, col := range res.Columns {
		cols[i] = wire.Column{Name: col.Name, Type: typeName(col.Type)}
	}
	if err := c.send(st, &wire.ResultHeader{StatementIndex: int64(index), Columns: cols}, false); err != nil {
		return err
	}
	var batch [][]any
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := c.sendFlow(st, &wire.Rows{Rows: batch}, false)
		batch, size = nil, 0
		return err
	}
	var created []value.ID
	for _, row := range res.Rows {
		cells := make([]any, len(row))
		for i, cell := range row {
			cells[i] = c.cell(cell)
		}
		batch = append(batch, cells)
		size += len(wire.AppendAny(nil, cells))
		if size >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if len(res.Columns) == 1 && res.Columns[0].Name == "id" && res.Affected > 0 {
		for _, row := range res.Rows {
			if id, ok := row[0].(value.ID); ok {
				created = append(created, id)
			}
		}
	}
	end := &wire.ResultEnd{StatementIndex: int64(index), RowsReturned: int64(len(res.Rows)),
		RowsAffected: int64(res.Affected), CreatedIDs: created, Elapsed: time.Since(began)}
	id, state := r.Txn()
	end.Txn, end.TxnState = id, wire.TxnState(state)
	for _, w := range res.Warnings {
		end.Warnings = append(end.Warnings, w.Msg)
	}
	return c.send(st, end, last)
}

// typeName is a column's type as a type expression, or any for whole
// entities, relations and paths.
func typeName(t lang.Static) string {
	if t.Form != lang.FormValue || t.Type == nil {
		return "any"
	}
	return t.Type.String()
}

// cell converts a result cell to its wire form, sending large text,
// bytes and images as blob references.
func (c *conn) cell(v any) any {
	switch x := v.(type) {
	case *query.Entity:
		e := &wire.Entity{ID: x.ID, Kind: x.Kind, Meta: wire.Meta{Created: x.Meta.Created, Updated: x.Meta.Updated,
			Version: int64(x.Meta.Version), Labels: x.Meta.Labels, Owner: x.Meta.Owner, Props: x.Meta.Props}}
		for _, f := range x.Fields {
			wf := &wire.Field{ID: f.ID, Name: f.Name, Type: f.Type.String(), Doc: f.Doc, Declared: f.Declared}
			if f.Value != nil {
				wf.Value = c.cell(f.Value)
			}
			e.Fields = append(e.Fields, wf)
		}
		return e
	case *query.Relation:
		return &wire.Relation{ID: x.ID, Type: x.Type, From: x.From, To: x.To, Props: x.Props}
	case *query.Path:
		p := &wire.Path{}
		for _, n := range x.Nodes {
			p.Nodes = append(p.Nodes, c.cell(n))
		}
		for _, r := range x.Rels {
			p.Rels = append(p.Rels, c.cell(r).(*wire.Relation))
		}
		return p
	case value.Text:
		if int64(len(x.Go())) > c.inline {
			return c.lease("text", []byte(x.Go()), "text/plain; charset=utf-8")
		}
	case value.Bytes:
		if int64(x.Len()) > c.inline {
			return c.lease("bytes", x.Go(), x.MediaType())
		}
	case value.Image:
		if int64(x.Len()) > c.inline {
			return c.lease("image", x.Data(), "image/"+x.Format())
		}
	}
	return v
}

func (c *conn) lease(typ string, data []byte, media string) *wire.BlobRef {
	h := sha256.Sum256(data)
	return &wire.BlobRef{Type: typ, Hash: h[:], Length: int64(len(data)), MediaType: media, Token: c.srv.leases.add(data)}
}

// prepare parses a statement for later execution.
func (c *conn) prepare(st *stream, text string) error {
	id, stmt, err := c.sess.Prepare(text)
	if err != nil {
		return toError(err, 0)
	}
	types := map[string]string{}
	for _, p := range params(stmt) {
		types[p] = "any"
	}
	return c.send(st, &wire.Prepared{StatementID: id, ParamTypes: types}, true)
}

// params lists the $parameters a statement uses.
func params(st lang.Stmt) []string {
	var out []string
	text := lang.Format(st)
	toks, err := lang.Lex(text)
	if err != nil {
		return nil
	}
	for _, t := range toks {
		if t.Kind == lang.Param && !slices.Contains(out, t.Text) {
			out = append(out, t.Text)
		}
	}
	slices.Sort(out)
	return out
}

// blobRead sends part of a leased value.
func (c *conn) blobRead(st *stream, m *wire.BlobRead) error {
	if m.Ref == nil {
		return &wire.Error{Code: "type", Message: "BLOB_READ needs a blob reference", StatementIndex: -1}
	}
	data, ok := c.srv.leases.get(m.Ref.Token)
	if !ok {
		return &wire.Error{Code: "lease_expired", Message: "the blob reference has expired; run the query again", StatementIndex: -1}
	}
	if m.Offset < 0 || m.Offset > int64(len(data)) || m.Length < 0 {
		return &wire.Error{Code: "constraint", Message: "the range is outside the blob", StatementIndex: -1}
	}
	end := int64(len(data))
	if m.Length > 0 {
		end = min(end, m.Offset+m.Length)
	}
	off := m.Offset
	for {
		n := min(end-off, blobChunk)
		last := off+n == end
		if err := c.sendFlow(st, &wire.BlobData{Offset: off, Data: data[off : off+n]}, last); err != nil {
			return err
		}
		off += n
		if last {
			return nil
		}
	}
}

// leases holds values sent as blob references until they expire
// (spec/protocol.md §7).
type leases struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*lease
}

type lease struct {
	data    []byte
	expires time.Time
}

func newLeases(ttl time.Duration) *leases { return &leases{ttl: ttl, m: map[string]*lease{}} }

func (l *leases) add(data []byte) []byte {
	tok := make([]byte, 16)
	rand.Read(tok)
	l.mu.Lock()
	l.m[string(tok)] = &lease{data, time.Now().Add(l.ttl)}
	l.mu.Unlock()
	return tok
}

func (l *leases) get(tok []byte) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.m[string(tok)]
	if !ok || time.Now().After(le.expires) {
		return nil, false
	}
	return le.data, true
}

func (l *leases) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, le := range l.m {
		if time.Now().After(le.expires) {
			delete(l.m, k)
		}
	}
}
