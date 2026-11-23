package client

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// Conn is a connection to a server. It implements polydb.DB and is safe
// for concurrent use: each request runs on its own stream.
type Conn struct {
	nc    net.Conn
	r     *wire.Reader
	w     *wire.Writer
	trace func(dir string, stream uint32, m wire.Message)
	hello *wire.HelloOK
	auth  *wire.AuthOK

	streamWindow int64
	connCredit   *wire.Credit

	mu        sync.Mutex
	streams   map[uint32]*stream
	next      uint32
	err       error // why the connection ended
	goingAway bool
	done      chan struct{}
}

func newConn(nc net.Conn, trace func(string, uint32, wire.Message)) *Conn {
	return &Conn{nc: nc, r: wire.NewReader(nc), w: wire.NewWriter(nc), trace: trace,
		streams: map[uint32]*stream{}, next: 1, done: make(chan struct{})}
}

// User returns the user the connection logged in as.
func (c *Conn) User() string { return c.auth.User }

// Database returns the database in use when the connection was made.
func (c *Conn) Database() string { return c.auth.Database }

// ServerVersion returns the server's version.
func (c *Conn) ServerVersion() string { return c.hello.ServerVersion }

func (c *Conn) send(stream uint32, m wire.Message, end bool) error {
	if c.trace != nil {
		c.trace("send", stream, m)
	}
	return c.w.WriteMessage(stream, m, end)
}

// recv0 reads a stream 0 message during the handshake.
func (c *Conn) recv0() (wire.Message, error) {
	in, err := c.r.ReadMessage()
	if err != nil {
		return nil, err
	}
	if c.trace != nil {
		c.trace("recv", in.Stream, in.Msg)
	}
	return in.Msg, nil
}

// stream is one request's responses.
type stream struct {
	id     uint32
	c      *Conn
	credit *wire.Credit

	mu     sync.Mutex
	queue  []wire.Incoming
	wake   chan struct{}
	ended  bool  // END_STREAM received
	err    error // the connection failed
	doneCh chan struct{}
}

func (s *stream) push(in wire.Incoming) {
	s.mu.Lock()
	s.queue = append(s.queue, in)
	if in.EndStream {
		s.ended = true
	}
	s.signal()
	s.mu.Unlock()
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.signal()
	s.mu.Unlock()
}

func (s *stream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// recv returns the stream's next message, granting flow-control window
// for the ROWS and BLOB_DATA it consumes.
func (s *stream) recv(ctx context.Context) (wire.Incoming, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			in := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			if t := in.Msg.Type(); t == wire.TypeRows || t == wire.TypeBlobData {
				s.c.grant(s, in.Size)
			}
			return in, nil
		}
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return wire.Incoming{}, err
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return wire.Incoming{}, ctx.Err()
		}
	}
}

// grant returns window for consumed bytes.
func (c *Conn) grant(s *stream, n int) {
	if inc := s.credit.Consume(n); inc > 0 {
		c.send(s.id, &wire.WindowUpdate{Increment: inc}, false)
	}
	if inc := c.connCredit.Consume(n); inc > 0 {
		c.send(0, &wire.WindowUpdate{Increment: inc}, false)
	}
}

// open starts a request on a new stream. If ctx ends before the request
// does, the client sends CANCEL.
func (c *Conn) open(ctx context.Context, m wire.Message) (*stream, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	if c.goingAway {
		c.mu.Unlock()
		return nil, &polydb.Error{Code: "connection_lost", Message: "the server is closing the connection", StatementIndex: -1}
	}
	s := &stream{id: c.next, c: c, credit: wire.NewCredit(c.streamWindow), wake: make(chan struct{}, 1)}
	c.next += 2
	c.streams[s.id] = s
	// Stream IDs must reach the server in increasing order, so the
	// opening message is sent before another stream can be allocated.
	err := c.send(s.id, m, false)
	c.mu.Unlock()
	if err != nil {
		c.closeStream(s)
		return nil, err
	}
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.mu.Lock()
				ended := s.ended
				s.mu.Unlock()
				if !ended {
					c.send(s.id, &wire.Cancel{}, false)
				}
			case <-s.endedSignal():
			}
		}()
	}
	return s, nil
}

// endedSignal returns a channel closed when the stream is removed.
func (s *stream) endedSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doneCh == nil {
		s.doneCh = make(chan struct{})
	}
	return s.doneCh
}

func (c *Conn) closeStream(s *stream) {
	c.mu.Lock()
	delete(c.streams, s.id)
	c.mu.Unlock()
	s.mu.Lock()
	if s.doneCh == nil {
		s.doneCh = make(chan struct{})
	}
	select {
	case <-s.doneCh:
	default:
		close(s.doneCh)
	}
	s.mu.Unlock()
}

// readLoop reads messages and hands them to their streams.
func (c *Conn) readLoop() {
	defer close(c.done)
	for {
		in, err := c.r.ReadMessage()
		if err != nil {
			c.shut(&polydb.Error{Code: "connection_lost", Message: err.Error(), StatementIndex: -1})
			return
		}
		if c.trace != nil {
			c.trace("recv", in.Stream, in.Msg)
		}
		if t := in.Msg.Type(); t == wire.TypeRows || t == wire.TypeBlobData {
			if err := c.connCredit.Receive(in.Size); err != nil {
				c.shut(&polydb.Error{Code: "flow_control", Message: err.Error(), StatementIndex: -1})
				return
			}
		}
		if in.Stream == 0 {
			switch m := in.Msg.(type) {
			case *wire.Ping:
				c.send(0, &wire.Pong{Data: m.Data}, false)
			case *wire.GoAway:
				c.mu.Lock()
				c.goingAway = true
				c.mu.Unlock()
			case *wire.ConnErrorMsg:
				c.shut(&polydb.Error{Code: m.Code, Message: m.Message, StatementIndex: -1})
				return
			}
			continue
		}
		c.mu.Lock()
		s := c.streams[in.Stream]
		c.mu.Unlock()
		if s == nil {
			continue // a stream we stopped reading
		}
		if t := in.Msg.Type(); t == wire.TypeRows || t == wire.TypeBlobData {
			if err := s.credit.Receive(in.Size); err != nil {
				c.shut(&polydb.Error{Code: "flow_control", Message: err.Error(), StatementIndex: -1})
				return
			}
		}
		s.push(in)
	}
}

// shut fails every stream and closes the connection.
func (c *Conn) shut(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	streams := c.streams
	c.streams = map[uint32]*stream{}
	c.mu.Unlock()
	for _, s := range streams {
		s.fail(err)
	}
	c.nc.Close()
}

// Close sends GOAWAY and closes the connection; the server rolls back
// open transactions.
func (c *Conn) Close() error {
	c.mu.Lock()
	last := c.next - 2
	c.mu.Unlock()
	c.send(0, &wire.GoAway{LastStream: int64(last), Code: "client_closing"}, false)
	c.shut(&polydb.Error{Code: "connection_lost", Message: "the connection is closed", StatementIndex: -1})
	<-c.done
	return nil
}

// toError converts an ERROR message.
func toError(e *wire.Error) error {
	return &polydb.Error{Code: e.Code, Message: e.Message, Line: int(e.Line), Column: int(e.Column),
		StatementIndex: int(e.StatementIndex), Retryable: e.Retryable}
}

// --- results ---

// Query runs a script and streams its results.
func (c *Conn) Query(ctx context.Context, text string, params polydb.Params) (polydb.Rows, error) {
	return c.query(ctx, text, params, nil)
}

func (c *Conn) query(ctx context.Context, text string, params polydb.Params, txn *value.ID) (*Rows, error) {
	vals, err := polydb.ParamValues(params)
	if err != nil {
		return nil, &polydb.Error{Code: "type", Message: err.Error(), StatementIndex: -1}
	}
	return c.request(ctx, &wire.Query{Text: text, Params: vals, Txn: txn})
}

// request sends a QUERY or EXECUTE and reads up to the first result.
func (c *Conn) request(ctx context.Context, m wire.Message) (*Rows, error) {
	s, err := c.open(ctx, m)
	if err != nil {
		return nil, err
	}
	r := &Rows{c: c, s: s, ctx: ctx}
	if err := r.advance(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Exec runs a script and returns what its statements changed.
func (c *Conn) Exec(ctx context.Context, text string, params polydb.Params) (polydb.Result, error) {
	r, err := c.query(ctx, text, params, nil)
	if err != nil {
		return polydb.Result{}, err
	}
	return r.drain()
}

// Rows streams a request's results.
type Rows struct {
	c   *Conn
	s   *stream
	ctx context.Context

	cols    []polydb.Column
	batch   [][]any
	cur     []any
	result  polydb.Result
	setDone bool // the current result set's RESULT_END arrived
	next    *wire.ResultHeader
	ended   bool // the stream ended
	err     error
	txn     *value.ID
	state   wire.TxnState
	closed  bool
}

// advance reads until the next result set starts (or the stream ends).
func (r *Rows) advance() error {
	for !r.ended {
		in, err := r.s.recv(r.ctx)
		if err != nil {
			return r.fail(err)
		}
		if in.EndStream {
			r.ended = true
			r.c.closeStream(r.s)
		}
		switch m := in.Msg.(type) {
		case *wire.ResultHeader:
			r.startSet(m)
			return nil
		case *wire.ResultEnd:
			r.end(m)
			r.setDone = true
		case *wire.Error:
			return r.fail(toError(m))
		}
	}
	return nil
}

func (r *Rows) startSet(h *wire.ResultHeader) {
	r.cols = r.cols[:0]
	for _, col := range h.Columns {
		r.cols = append(r.cols, polydb.Column{Name: col.Name, Type: col.Type})
	}
	r.batch, r.cur, r.setDone, r.result = nil, nil, false, polydb.Result{}
}

func (r *Rows) end(m *wire.ResultEnd) {
	r.result = polydb.Result{RowsAffected: m.RowsAffected, CreatedIDs: m.CreatedIDs, Warnings: m.Warnings}
	r.txn, r.state = m.Txn, m.TxnState
}

func (r *Rows) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	r.ended = true
	r.c.closeStream(r.s)
	return r.err
}

// Next advances to the next row of the current result set.
func (r *Rows) Next() bool {
	for len(r.batch) == 0 {
		if r.setDone || r.ended || r.err != nil || r.closed {
			r.cur = nil
			return false
		}
		in, err := r.s.recv(r.ctx)
		if err != nil {
			r.fail(err)
			return false
		}
		if in.EndStream {
			r.ended = true
			r.c.closeStream(r.s)
		}
		switch m := in.Msg.(type) {
		case *wire.Rows:
			r.batch = m.Rows
		case *wire.ResultEnd:
			r.end(m)
			r.setDone = true
		case *wire.ResultHeader:
			r.next, r.setDone = m, true
		case *wire.Error:
			r.fail(toError(m))
			return false
		}
	}
	row := r.batch[0]
	r.batch = r.batch[1:]
	r.cur = make([]any, len(row))
	for i, cell := range row {
		r.cur[i] = r.c.cell(cell)
	}
	return true
}

// NextResultSet moves to the next statement's result set: the rest of
// the current one is skipped. Every statement's result starts with a
// header, so there is another set exactly when another header arrives.
func (r *Rows) NextResultSet() bool {
	for r.Next() {
	}
	if r.err != nil || r.closed {
		return false
	}
	if r.next != nil {
		r.startSet(r.next)
		r.next = nil
		return true
	}
	if err := r.advance(); err != nil {
		return false
	}
	return !r.setDone // advance found a header (startSet clears setDone)
}

func (r *Rows) Scan(dest ...any) error {
	if r.cur == nil {
		return errors.New("client: Scan called without a row; call Next first")
	}
	return polydb.Scan(r.ctx, r.cur, dest...)
}

func (r *Rows) Values() []any            { return r.cur }
func (r *Rows) Columns() []polydb.Column { return r.cols }
func (r *Rows) Result() polydb.Result    { return r.result }
func (r *Rows) Err() error               { return r.err }

// Close discards the remaining results.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if !r.ended {
		r.c.send(r.s.id, &wire.Cancel{}, false)
		r.c.closeStream(r.s)
	}
	return nil
}

// drain reads every result set and sums what they changed.
func (r *Rows) drain() (polydb.Result, error) {
	var total polydb.Result
	for {
		for r.Next() {
		}
		if r.err != nil {
			return total, r.err
		}
		total.RowsAffected += r.result.RowsAffected
		total.CreatedIDs = append(total.CreatedIDs, r.result.CreatedIDs...)
		total.Warnings = append(total.Warnings, r.result.Warnings...)
		if !r.NextResultSet() {
			return total, r.err
		}
	}
}

// cell converts a result cell to the application form.
func (c *Conn) cell(v any) any {
	switch x := v.(type) {
	case *wire.Entity:
		e := &polydb.Entity{ID: x.ID, Kind: x.Kind,
			Meta: polydb.MetaOf(x.Meta.Created, x.Meta.Updated, x.Meta.Version, x.Meta.Labels, x.Meta.Owner, x.Meta.Props)}
		for _, f := range x.Fields {
			pf := &polydb.Field{ID: f.ID, Name: f.Name, Type: f.Type, Doc: f.Doc, Declared: f.Declared}
			if f.Value != nil {
				pf.Value = c.cell(f.Value)
			}
			e.Fields = append(e.Fields, pf)
		}
		return e
	case *wire.Relation:
		return &polydb.Relation{ID: x.ID, Type: x.Type, From: x.From, To: x.To, Props: polydb.PropsOf(x.Props)}
	case *wire.Path:
		p := &polydb.Path{}
		for _, n := range x.Nodes {
			p.Nodes = append(p.Nodes, c.cell(n))
		}
		for _, rel := range x.Rels {
			p.Rels = append(p.Rels, c.cell(rel).(*polydb.Relation))
		}
		return p
	case *wire.BlobRef:
		ref := x
		return polydb.NewBlob(x.Type, x.Length, x.MediaType, func(ctx context.Context, off, n int64) ([]byte, error) {
			return c.readBlob(ctx, ref, off, n)
		})
	}
	return v
}

// readBlob fetches part of a blob with BLOB_READ.
func (c *Conn) readBlob(ctx context.Context, ref *wire.BlobRef, off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	s, err := c.open(ctx, &wire.BlobRead{Ref: ref, Offset: off, Length: n})
	if err != nil {
		return nil, err
	}
	defer c.closeStream(s)
	var data []byte
	for {
		in, err := s.recv(ctx)
		if err != nil {
			return nil, err
		}
		switch m := in.Msg.(type) {
		case *wire.BlobData:
			data = append(data, m.Data...)
		case *wire.Error:
			return nil, toError(m)
		}
		if in.EndStream {
			return data, nil
		}
	}
}

// --- transactions ---

// Begin starts an explicit transaction.
func (c *Conn) Begin(ctx context.Context, opts polydb.TxOptions) (polydb.Tx, error) {
	text := "begin"
	if opts.ReadOnly {
		text = "begin read only"
	}
	r, err := c.query(ctx, text, nil, nil)
	if err != nil {
		return nil, err
	}
	if _, err := r.drain(); err != nil {
		return nil, err
	}
	if r.txn == nil {
		return nil, errors.New("client: the server didn't return a transaction id")
	}
	return &Tx{c: c, id: *r.txn}, nil
}

// Tx is an explicit transaction.
type Tx struct {
	c  *Conn
	id value.ID
}

// ID returns the transaction's ID.
func (t *Tx) ID() value.ID { return t.id }

func (t *Tx) Query(ctx context.Context, text string, params polydb.Params) (polydb.Rows, error) {
	return t.c.query(ctx, text, params, &t.id)
}

func (t *Tx) Exec(ctx context.Context, text string, params polydb.Params) (polydb.Result, error) {
	r, err := t.c.query(ctx, text, params, &t.id)
	if err != nil {
		return polydb.Result{}, err
	}
	return r.drain()
}

func (t *Tx) Commit(ctx context.Context) error {
	_, err := t.Exec(ctx, "commit", nil)
	return err
}

func (t *Tx) Rollback(ctx context.Context) error {
	_, err := t.Exec(ctx, "rollback", nil)
	return err
}

// --- prepared statements ---

// Prepare prepares one statement.
func (c *Conn) Prepare(ctx context.Context, text string) (polydb.Stmt, error) {
	st := &Stmt{c: c, text: text}
	return st, st.prepare(ctx)
}

// Stmt is a prepared statement. If the server reports it stale, it is
// prepared again once.
type Stmt struct {
	c    *Conn
	text string
	mu   sync.Mutex
	id   int64
}

func (s *Stmt) prepare(ctx context.Context) error {
	st, err := s.c.open(ctx, &wire.Prepare{Text: s.text})
	if err != nil {
		return err
	}
	defer s.c.closeStream(st)
	in, err := st.recv(ctx)
	if err != nil {
		return err
	}
	switch m := in.Msg.(type) {
	case *wire.Prepared:
		s.mu.Lock()
		s.id = m.StatementID
		s.mu.Unlock()
		return nil
	case *wire.Error:
		return toError(m)
	}
	return errors.New("client: unexpected reply to PREPARE")
}

func (s *Stmt) execute(ctx context.Context, params polydb.Params) (*Rows, error) {
	vals, err := polydb.ParamValues(params)
	if err != nil {
		return nil, &polydb.Error{Code: "type", Message: err.Error(), StatementIndex: -1}
	}
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		id := s.id
		s.mu.Unlock()
		r, err := s.c.request(ctx, &wire.Execute{StatementID: id, Params: vals})
		if polydb.CodeOf(err) == "stale_statement" && attempt == 0 {
			if err := s.prepare(ctx); err != nil {
				return nil, err
			}
			continue
		}
		return r, err
	}
}

func (s *Stmt) Query(ctx context.Context, params polydb.Params) (polydb.Rows, error) {
	return s.execute(ctx, params)
}

func (s *Stmt) Exec(ctx context.Context, params polydb.Params) (polydb.Result, error) {
	r, err := s.execute(ctx, params)
	if err != nil {
		return polydb.Result{}, err
	}
	return r.drain()
}

func (s *Stmt) Close() error {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	st, err := s.c.open(context.Background(), &wire.CloseStatement{StatementID: id})
	if err != nil {
		return err
	}
	defer s.c.closeStream(st)
	_, err = st.recv(context.Background())
	return err
}
