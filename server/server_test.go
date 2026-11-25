package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/authn"
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// --- a minimal client for tests ---

type testClient struct {
	t        *testing.T
	nc       net.Conn
	r        *wire.Reader
	w        *wire.Writer
	hello    *wire.HelloOK
	authOK   *wire.AuthOK
	next     uint32
	autoPong bool
	autoWin  atomic.Bool

	mu       sync.Mutex
	streams  map[uint32]chan wire.Incoming
	control  chan wire.Message
	readErr  error
	finished chan struct{}
}

type dialOpts struct {
	user, password string
	hello          wire.Hello
	unix           bool
	plain          bool // no TLS
	noAutoPong     bool
}

func dial(t *testing.T, addr string, o dialOpts) (*testClient, error) {
	t.Helper()
	network := "tcp"
	if o.unix {
		network = "unix"
	}
	nc, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	var cs *tls.ConnectionState
	if !o.unix && !o.plain {
		tc := tls.Client(nc, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{wire.ALPN}})
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		state := tc.ConnectionState()
		cs, nc = &state, tc
	}
	t.Cleanup(func() { nc.Close() })
	c := &testClient{t: t, nc: nc, r: wire.NewReader(nc), w: wire.NewWriter(nc), next: 1,
		autoPong: !o.noAutoPong,
		streams:  map[uint32]chan wire.Incoming{}, control: make(chan wire.Message, 16), finished: make(chan struct{})}
	c.autoWin.Store(true)
	if err := wire.WritePreface(nc); err != nil {
		return nil, err
	}
	h := o.hello
	if h.Versions == nil {
		h.Versions = map[int]int{1: 0}
	}
	h.ClientName, h.User = "test", o.user
	if h.Features == nil {
		h.Features = []string{"queue-wait", "blob-range"}
	}
	if err := c.w.WriteMessage(0, &h, false); err != nil {
		return nil, err
	}
	in, err := c.r.ReadMessage()
	if err != nil {
		return nil, err
	}
	switch m := in.Msg.(type) {
	case *wire.HelloOK:
		c.hello = m
	case *wire.ConnErrorMsg:
		return nil, &wire.ConnError{Code: m.Code, Msg: m.Message}
	}
	ccfg := authn.ClientConfig{User: o.user}
	if o.password != "" {
		pw := o.password
		ccfg.Password = func() (string, error) { return pw, nil }
	}
	ok, err := authn.Login(&clientTransport{c}, ccfg, c.hello.AuthMechanisms, cs)
	if err != nil {
		return nil, err
	}
	c.authOK = ok
	c.r.SetMaxFrameSize(int(c.hello.MaxFrameSize))
	go c.readLoop()
	return c, nil
}

type clientTransport struct{ c *testClient }

func (t *clientTransport) Send(m wire.Message) error { return t.c.w.WriteMessage(0, m, false) }
func (t *clientTransport) Recv() (wire.Message, error) {
	in, err := t.c.r.ReadMessage()
	return in.Msg, err
}

func (c *testClient) readLoop() {
	defer close(c.finished)
	for {
		in, err := c.r.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for _, ch := range c.streams {
				close(ch)
			}
			c.streams = map[uint32]chan wire.Incoming{}
			c.mu.Unlock()
			close(c.control)
			return
		}
		if in.Stream == 0 {
			if p, ok := in.Msg.(*wire.Ping); ok && c.autoPong {
				c.w.WriteMessage(0, &wire.Pong{Data: p.Data}, false)
				continue
			}
			c.control <- in.Msg
			continue
		}
		c.mu.Lock()
		ch := c.streams[in.Stream]
		c.mu.Unlock()
		if ch != nil {
			ch <- in
		}
		if c.autoWin.Load() && in.Size > 0 && (in.Msg.Type() == wire.TypeRows || in.Msg.Type() == wire.TypeBlobData) {
			c.w.WriteMessage(in.Stream, &wire.WindowUpdate{Increment: int64(in.Size)}, false)
			c.w.WriteMessage(0, &wire.WindowUpdate{Increment: int64(in.Size)}, false)
		}
	}
}

// open sends a request on a new stream and returns the channel of its
// responses.
func (c *testClient) open(m wire.Message) (uint32, chan wire.Incoming) {
	ch := make(chan wire.Incoming, 1024)
	c.mu.Lock()
	id := c.next
	c.next += 2
	c.streams[id] = ch
	c.mu.Unlock()
	if err := c.w.WriteMessage(id, m, false); err != nil {
		c.t.Fatal(err)
	}
	return id, ch
}

type stmtResult struct {
	header *wire.ResultHeader
	rows   [][]any
	end    *wire.ResultEnd
}

// collect reads a stream's responses until END_STREAM.
func collect(t *testing.T, ch chan wire.Incoming) ([]*stmtResult, *wire.Error, []wire.Message) {
	t.Helper()
	var out []*stmtResult
	var other []wire.Message
	timeout := time.After(10 * time.Second)
	for {
		select {
		case in, ok := <-ch:
			if !ok {
				t.Fatal("connection closed")
			}
			switch m := in.Msg.(type) {
			case *wire.ResultHeader:
				out = append(out, &stmtResult{header: m})
			case *wire.Rows:
				out[len(out)-1].rows = append(out[len(out)-1].rows, m.Rows...)
			case *wire.ResultEnd:
				if len(out) == 0 || out[len(out)-1].end != nil {
					out = append(out, &stmtResult{})
				}
				out[len(out)-1].end = m
			case *wire.Error:
				return out, m, other
			default:
				other = append(other, m)
			}
			if in.EndStream {
				return out, nil, other
			}
		case <-timeout:
			t.Fatal("timed out waiting for the response")
		}
	}
}

func (c *testClient) query(text string, params map[string]value.Value, txn *value.ID) ([]*stmtResult, *wire.Error) {
	c.t.Helper()
	_, ch := c.open(&wire.Query{Text: text, Params: params, Txn: txn})
	res, e, _ := collect(c.t, ch)
	return res, e
}

func (c *testClient) mustQuery(text string) []*stmtResult {
	c.t.Helper()
	res, e := c.query(text, nil, nil)
	if e != nil {
		c.t.Fatalf("%s: %v", text, e)
	}
	return res
}

// --- test servers ---

type testServer struct {
	srv  *Server
	eng  *engine.Engine
	addr string
}

func startServer(t *testing.T, cfg Config, network string) *testServer {
	t.Helper()
	return startServerOpts(t, cfg, network, engine.Options{})
}

// startServerOpts is startServer with an engine opened with opts.
func startServerOpts(t *testing.T, cfg Config, network string, opts engine.Options) *testServer {
	t.Helper()
	opts.PasswordIterations = security.MinIterations
	eng, err := engine.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	eng.Security().SetPassword("admin", "secret")
	if cfg.TLS == nil && network == "tls" {
		id, err := auth9.LoadFrom(filepath.Join(t.TempDir(), "identity"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.TLS = IdentityTLS(id)
	}
	cfg.Engine = eng
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var l net.Listener
	if network == "unix" {
		l, err = net.Listen("unix", filepath.Join(t.TempDir(), "polydb.sock"))
	} else {
		l, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		eng.Close()
	})
	return &testServer{srv: srv, eng: eng, addr: l.Addr().String()}
}

func admin(t *testing.T, ts *testServer) *testClient {
	t.Helper()
	c, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func str(v any) string {
	if vv, ok := v.(value.Value); ok {
		return vv.String()
	}
	return "?"
}

// --- tests ---

func TestQueryOverTLS(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	c := admin(t, ts)
	if c.authOK.User != "admin" || c.hello.AuthMechanisms[0] != authn.ScramPlus {
		t.Errorf("login: %+v %v", c.authOK, c.hello.AuthMechanisms)
	}
	res := c.mustQuery(`create database d; use d;
		define kind Person { name: text, age: int? };
		create Person @ann { name: "Ann", age: 34 };
		find Person p | show p.name, p.age, p`)
	if len(res) != 5 {
		t.Fatalf("%d results", len(res))
	}
	if ids := res[3].end.CreatedIDs; len(ids) != 1 || res[3].end.RowsAffected != 1 {
		t.Errorf("create: %+v", res[3].end)
	}
	last := res[4]
	cols := []string{}
	for _, col := range last.header.Columns {
		cols = append(cols, col.Name+":"+col.Type)
	}
	if strings.Join(cols, " ") != "p.name:text p.age:int? p:any" {
		t.Errorf("columns %v", cols)
	}
	row := last.rows[0]
	e, ok := row[2].(*wire.Entity)
	if str(row[0]) != `"Ann"` || str(row[1]) != "34" || !ok || e.Kind != "Person" || len(e.Fields) != 2 || e.Meta.Owner != "admin" {
		t.Errorf("row %v", row)
	}
	if last.end.TxnState != wire.TxnNone || last.end.RowsReturned != 1 {
		t.Errorf("end %+v", last.end)
	}

	// Errors carry the statement index and position.
	_, werr := c.query("find Person p;\nfind Persn q", nil, nil)
	if werr == nil || werr.Code != "not_found" || werr.StatementIndex != 1 || werr.Line != 2 {
		t.Errorf("error: %+v", werr)
	}
	_, werr = c.query("find p |", nil, nil)
	if werr == nil || werr.Code != "syntax" {
		t.Errorf("syntax error: %+v", werr)
	}
	// Parameters.
	res, werr = c.query(`find Person p | where p.age > $min | show count()`, map[string]value.Value{"min": value.Int(30)}, nil)
	if werr != nil || str(res[0].rows[0][0]) != "1" {
		t.Errorf("params: %v %v", res, werr)
	}
	// show sessions sees this connection.
	res = c.mustQuery(`show sessions`)
	if len(res[0].rows) != 1 || str(res[0].rows[0][1]) != `"admin"` {
		t.Errorf("sessions: %v", res[0].rows)
	}
}

func TestTransactionsOverProtocol(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	c := admin(t, ts)
	c.mustQuery(`create database d; use d; define kind K { a: int }`)
	res := c.mustQuery(`begin`)
	txn := res[0].end.Txn
	if txn == nil || res[0].end.TxnState != wire.TxnOpen {
		t.Fatalf("begin: %+v", res[0].end)
	}
	if _, e := c.query(`create K @k { a: 1 }`, nil, txn); e != nil {
		t.Fatal(e)
	}
	// Outside the transaction nothing is visible yet.
	res = c.mustQuery(`find K k | show count()`)
	if str(res[0].rows[0][0]) != "0" {
		t.Errorf("isolation: %v", res[0].rows)
	}
	// A failed statement aborts it.
	if _, e := c.query(`create K { a: "x" }`, nil, txn); e == nil || e.Code != "type" {
		t.Fatalf("bad create: %v", e)
	}
	if _, e := c.query(`find K k`, nil, txn); e == nil || e.Code != "txn_aborted" {
		t.Errorf("after failure: %v", e)
	}
	res, e := c.query(`rollback`, nil, txn)
	if e != nil || res[0].end.TxnState != wire.TxnRolledBack {
		t.Errorf("rollback: %v %v", res, e)
	}
	if _, e := c.query(`find K k`, nil, txn); e == nil || e.Code != "txn_not_found" {
		t.Errorf("finished transaction: %v", e)
	}
	// A whole transaction in one request.
	res = c.mustQuery(`begin; create K { a: 2 }; commit; find K k | show k.a`)
	if res[2].end.TxnState != wire.TxnCommitted || str(res[3].rows[0][0]) != "2" {
		t.Errorf("script transaction: %+v %v", res[2].end, res[3].rows)
	}
	// Conflicts are retryable.
	r1 := c.mustQuery(`begin; find K k | update k set a = 3`)
	r2 := c.mustQuery(`begin; find K k | update k set a = 4`)
	if _, e := c.query(`commit`, nil, r1[0].end.Txn); e != nil {
		t.Fatal(e)
	}
	if _, e := c.query(`commit`, nil, r2[0].end.Txn); e == nil || e.Code != "conflict" || !e.Retryable {
		t.Errorf("conflict: %+v", e)
	}
}

func TestFlowControl(t *testing.T) {
	ts := startServer(t, Config{StreamWindow: 4096}, "tls")
	c, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	c.mustQuery(`create database d; use d; define kind K { a: text }`)
	var script strings.Builder
	for i := range 200 {
		script.WriteString(`create K { a: "` + strings.Repeat("x", 50) + value.Int(i).String() + `" };`)
	}
	c.mustQuery(script.String())
	// Stop granting window: the server must stop after about one window.
	c.autoWin.Store(false)
	id, ch := c.open(&wire.Query{Text: `find K k | show k.a`})
	got := 0
	deadline := time.After(300 * time.Millisecond)
wait:
	for {
		select {
		case in := <-ch:
			if rows, ok := in.Msg.(*wire.Rows); ok {
				got += len(rows.Rows)
			}
		case <-deadline:
			break wait
		}
	}
	if got >= 200 {
		t.Fatalf("the server sent all %d rows without window", got)
	}
	// Grant window: the rest arrives.
	c.w.WriteMessage(id, &wire.WindowUpdate{Increment: 1 << 20}, false)
	c.w.WriteMessage(0, &wire.WindowUpdate{Increment: 1 << 20}, false)
	for got < 200 {
		select {
		case in := <-ch:
			if rows, ok := in.Msg.(*wire.Rows); ok {
				got += len(rows.Rows)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d rows after the window update", got)
		}
	}
}

func TestCancelAndLimits(t *testing.T) {
	ts := startServer(t, Config{MaxStreams: 1}, "tls")
	c := admin(t, ts)
	c.mustQuery(`create database d; use d; define kind W { jobs: queue.fifo<text> }; create W @w { }`)
	id, ch := c.open(&wire.Query{Text: `pop @w.jobs wait 5s`})
	time.Sleep(50 * time.Millisecond)
	// Only one stream at a time is allowed.
	if _, e := c.query(`find W w`, nil, nil); e == nil || e.Code != "limit" {
		t.Errorf("second stream: %v", e)
	}
	began := time.Now()
	c.w.WriteMessage(id, &wire.Cancel{}, false)
	_, e, _ := collect(t, ch)
	if e == nil || e.Code != "cancelled" || time.Since(began) > 2*time.Second {
		t.Errorf("cancel: %v after %v", e, time.Since(began))
	}
	// A pop that waits gets the item pushed meanwhile.
	_, ch = c.open(&wire.Query{Text: `pop @w.jobs wait 5s`})
	time.Sleep(50 * time.Millisecond)
	c2 := admin(t, ts)
	c2.mustQuery(`use d; find W w | push w.jobs "job1"`)
	res, e, _ := collect(t, ch)
	if e != nil || str(res[0].rows[0][0]) != `"job1"` {
		t.Errorf("waiting pop: %v %v", res, e)
	}
}

func TestBlobsAndPrepared(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	c, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret", hello: wire.Hello{InlineLimit: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if c.hello.InlineLimit != 1024 {
		t.Errorf("inline limit %d", c.hello.InlineLimit)
	}
	long := strings.Repeat("0123456789", 500)
	c.mustQuery(`create database d; use d; define kind Doc { body: text }; create Doc { body: "` + long + `" }`)
	res := c.mustQuery(`find Doc x | show x.body, x`)
	ref, ok := res[0].rows[0][0].(*wire.BlobRef)
	if !ok || ref.Type != "text" || ref.Length != 5000 {
		t.Fatalf("blob ref: %#v", res[0].rows[0][0])
	}
	if e := res[0].rows[0][1].(*wire.Entity); e.Fields[0].Value.(*wire.BlobRef).Length != 5000 {
		t.Error("entity field not sent as a blob reference")
	}
	_, ch := c.open(&wire.BlobRead{Ref: ref, Offset: 10, Length: 25})
	_, e, other := collect(t, ch)
	var data []byte
	for _, m := range other {
		data = append(data, m.(*wire.BlobData).Data...)
	}
	if e != nil || string(data) != long[10:35] {
		t.Errorf("blob read: %q %v", data, e)
	}
	_, ch = c.open(&wire.BlobRead{Ref: &wire.BlobRef{Token: []byte("forged")}})
	if _, e, _ := collect(t, ch); e == nil || e.Code != "lease_expired" {
		t.Errorf("forged token: %v", e)
	}

	// Prepared statements.
	_, ch = c.open(&wire.Prepare{Text: `create Doc { body: $b }`})
	_, e, other = collect(t, ch)
	prep, ok := other[0].(*wire.Prepared)
	if e != nil || !ok || prep.ParamTypes["b"] != "any" {
		t.Fatalf("prepare: %v %v", other, e)
	}
	for _, b := range []string{"one", "two"} {
		_, ch = c.open(&wire.Execute{StatementID: prep.StatementID, Params: map[string]value.Value{"b": value.MustText(b)}})
		if _, e, _ := collect(t, ch); e != nil {
			t.Fatal(e)
		}
	}
	if res := c.mustQuery(`find Doc x | show count()`); str(res[0].rows[0][0]) != "3" {
		t.Errorf("after executes: %v", res[0].rows)
	}
	_, ch = c.open(&wire.CloseStatement{StatementID: prep.StatementID})
	collect(t, ch)
	_, ch = c.open(&wire.Execute{StatementID: prep.StatementID})
	if _, e, _ := collect(t, ch); e == nil || e.Code != "stale_statement" {
		t.Errorf("closed statement: %v", e)
	}
}

func TestLogins(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	if _, err := dial(t, ts.addr, dialOpts{user: "admin", password: "wrong"}); !errors.Is(err, authn.ErrFailed) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := dial(t, ts.addr, dialOpts{user: "ghost", password: "x"}); !errors.Is(err, authn.ErrFailed) {
		t.Errorf("unknown user: %v", err)
	}
	_, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret", hello: wire.Hello{Versions: map[int]int{2: 0}}})
	var ce *wire.ConnError
	if !errors.As(err, &ce) || ce.Code != "version_mismatch" {
		t.Errorf("version: %v", err)
	}
	// HELLO can name the database to use.
	c := admin(t, ts)
	c.mustQuery(`create database sales`)
	c2, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret", hello: wire.Hello{Database: "sales"}})
	if err != nil || c2.authOK.Database != "sales" {
		t.Errorf("hello database: %v %+v", err, c2)
	}
}

func TestUnixSocketAndInsecure(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	ts := startServer(t, Config{PeerUsers: map[string]string{me.Username: "admin"}}, "unix")
	c, err := dial(t, ts.addr, dialOpts{user: "admin", unix: true})
	if err != nil {
		t.Fatalf("peer login: %v", err)
	}
	if c.authOK.User != "admin" || c.hello.AuthMechanisms[0] != authn.Peer {
		t.Errorf("peer: %+v %v", c.authOK, c.hello.AuthMechanisms)
	}
	c.mustQuery(`show databases`)

	// Plain TCP from loopback works when the server has no TLS.
	ts2 := startServer(t, Config{Compression: true}, "tcp")
	c2, err := dial(t, ts2.addr, dialOpts{user: "admin", password: "secret", plain: true,
		hello: wire.Hello{Features: []string{"deflate"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c2.hello.Features, ",") != "deflate" {
		t.Errorf("features %v", c2.hello.Features)
	}
	c2.mustQuery(`create database big; use big; define kind K { a: text }`)
	res := c2.mustQuery(`create K { a: "` + strings.Repeat("compress me ", 500) + `" }; find K k | show k.a`)
	if len(str(res[1].rows[0][0])) < 5000 {
		t.Errorf("compressed result: %d bytes", len(str(res[1].rows[0][0])))
	}
}

func TestShutdownAndKeepAlive(t *testing.T) {
	ts := startServer(t, Config{KeepAlive: 50 * time.Millisecond, PingTimeout: 100 * time.Millisecond}, "tls")
	// A client that answers pings stays connected.
	c := admin(t, ts)
	time.Sleep(300 * time.Millisecond)
	c.mustQuery(`show databases`)
	// One that doesn't is closed.
	c2, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret", noAutoPong: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawPing bool
	timeout := time.After(3 * time.Second)
loop:
	for {
		select {
		case m, ok := <-c2.control:
			if !ok {
				break loop
			}
			if _, isPing := m.(*wire.Ping); isPing {
				sawPing = true
			}
			if e, isErr := m.(*wire.ConnErrorMsg); isErr && e.Code != "timeout" {
				t.Errorf("closed with %s", e.Code)
			}
		case <-timeout:
			t.Fatal("silent client wasn't closed")
		}
	}
	if !sawPing {
		t.Error("no ping")
	}
	// Shutdown sends GOAWAY and closes.
	go ts.srv.Shutdown(context.Background())
	var away *wire.GoAway
	for m := range c.control {
		if g, ok := m.(*wire.GoAway); ok {
			away = g
		}
	}
	if away == nil || away.Code != "shutting_down" {
		t.Errorf("goaway: %+v", away)
	}
}

func TestPermissionsOverProtocol(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	c := admin(t, ts)
	c.mustQuery(`create database d; use d; define kind K { a: int }; create K { a: 1 };
		create user ramon password "pw"; grant read on database d to ramon`)
	r, err := dial(t, ts.addr, dialOpts{user: "ramon", password: "pw", hello: wire.Hello{Database: "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.mustQuery(`find K k | show k.a`); str(res[0].rows[0][0]) != "1" {
		t.Errorf("read: %v", res[0].rows)
	}
	if _, e := r.query(`create K { a: 2 }`, nil, nil); e == nil || e.Code != "permission" {
		t.Errorf("write: %v", e)
	}
	if _, e := r.query(`show sessions`, nil, nil); e == nil || e.Code != "permission" {
		t.Errorf("show sessions: %v", e)
	}
}

// A client that has seen a stream end may open another at once, even
// with one stream allowed: the server frees the place before the
// stream's last message.
func TestStreamPlaceFreedBeforeEnd(t *testing.T) {
	ts := startServer(t, Config{MaxStreams: 1}, "tls")
	c := admin(t, ts)
	c.mustQuery(`create database d; use d; define kind W { n: int }; create W { n: 1 }`)
	for i := range 300 {
		if _, e := c.query(`find W w | show w.n`, nil, nil); e != nil {
			t.Fatalf("request %d: %v", i, e)
		}
	}
}

// TestLeasesFromBlobStore checks that a lease on a value the blob store
// holds reads it from there instead of keeping it, and that a computed
// value is kept in memory.
func TestLeasesFromBlobStore(t *testing.T) {
	ts := startServer(t, Config{}, "tls")
	c := admin(t, ts)
	long := strings.Repeat("0123456789", 3000) // over the 16 KiB blob threshold
	c.mustQuery(`create database d; use d; define kind Doc { body: text }; create Doc { body: "` + long + `" }`)
	res := c.mustQuery(`find Doc x | show x.body, x.body ++ "!"`)
	stored, computed := res[0].rows[0][0].(*wire.BlobRef), res[0].rows[0][1].(*wire.BlobRef)
	le, ok := ts.srv.leases.get(stored.Token)
	if !ok || le.reader == nil || le.data != nil {
		t.Fatalf("stored value's lease: reader %v, %d bytes in memory", le.reader != nil, len(le.data))
	}
	if le, _ := ts.srv.leases.get(computed.Token); le.reader != nil || len(le.data) != len(long)+1 {
		t.Fatal("computed value's lease isn't in memory")
	}
	_, ch := c.open(&wire.BlobRead{Ref: stored, Offset: 29_990})
	_, e, other := collect(t, ch)
	if e != nil || len(other) != 1 || string(other[0].(*wire.BlobData).Data) != long[29_990:] {
		t.Fatalf("read: %v %v", other, e)
	}
	ts.srv.leases.close()
	if _, ok := ts.srv.leases.get(stored.Token); ok {
		t.Fatal("lease survived close")
	}
}

// Connections are limited per user and in total, past which only server
// admins get in; request options can't raise a statement's limits past
// the server's.
func TestConnectionAndRequestLimits(t *testing.T) {
	ts := startServerOpts(t, Config{}, "tls", engine.Options{Limits: engine.Limits{
		MaxConnections: 3, MaxUserConnections: 2, MaxMemory: 1 << 20}})
	a := admin(t, ts)
	a.mustQuery(`create database d; create user u password "pw"; grant read on database d to u`)
	u1, err := dial(t, ts.addr, dialOpts{user: "u", password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(t, ts.addr, dialOpts{user: "u", password: "pw"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(t, ts.addr, dialOpts{user: "u", password: "pw"}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("a third connection for one user: %v", err)
	}
	// The server is now full (admin and u twice); admins still get in.
	if _, err := dial(t, ts.addr, dialOpts{user: "admin", password: "secret"}); err != nil {
		t.Errorf("an admin past the connection limit: %v", err)
	}
	_, ch := u1.open(&wire.Query{Text: `use d`, Options: map[string]value.Value{"max_memory": value.Int(0)}})
	if _, e, _ := collect(t, ch); e == nil || e.Code != "permission" {
		t.Errorf("lifting max_memory by a request option: %v", e)
	}
	_, ch = u1.open(&wire.Query{Text: `use d`, Options: map[string]value.Value{"max_memory": value.Int(1000)}})
	if _, e, _ := collect(t, ch); e != nil {
		t.Errorf("lowering max_memory by a request option: %v", e)
	}
}
