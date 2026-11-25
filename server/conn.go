package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandgorgon/polydb/authn"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/session"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// conn is one client connection.
type conn struct {
	srv       *Server
	nc        net.Conn
	r         *wire.Reader
	w         *wire.Writer
	sess      *session.Session
	id        value.ID
	remote    string
	connected time.Time
	inline    int64
	features  []string
	connWin   *wire.Window
	requests  atomic.Int64
	activity  atomic.Int64 // unix nanos of the last frame read
	ctx       context.Context
	cancel    context.CancelFunc

	mu         sync.Mutex
	streams    map[uint32]*stream
	open       int // streams counting against MaxStreams
	lastStream uint32
	goingAway  bool
	closed     bool
	pong       chan struct{}
}

// stream is one request in progress.
type stream struct {
	id     uint32
	win    *wire.Window
	ctx    context.Context
	cancel context.CancelFunc
	// cancelled is set by CANCEL, so the request reports "cancelled".
	cancelled atomic.Bool
	released  bool // its place counts no more against MaxStreams (on conn.mu)
	// upload takes the content of an upload stream's later BLOB_WRITEs.
	upload *io.PipeWriter
}

func (s *Server) handle(nc net.Conn) {
	c := &conn{srv: s, nc: nc, id: s.ids.New(), remote: nc.RemoteAddr().String(), connected: time.Now(),
		streams: map[uint32]*stream{}, pong: make(chan struct{}, 1)}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	defer c.close()
	log := s.cfg.Logger.With("conn", c.id.String(), "remote", c.remote)

	info := authn.ConnInfo{Addr: c.remote}
	nc.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	switch x := nc.(type) {
	case *net.UnixConn:
		info.Addr = "unix:" + c.remote
		if u, err := authn.PeerUser(x); err == nil {
			info.PeerUser = u
		}
	default:
		if s.cfg.TLS != nil {
			tc := tls.Server(nc, s.cfg.TLS)
			if err := tc.HandshakeContext(c.ctx); err != nil {
				log.Info("TLS handshake failed", "err", err)
				return
			}
			state := tc.ConnectionState()
			info.TLS = &state
			c.nc = tc
		} else if host, _, err := net.SplitHostPort(c.remote); err == nil {
			info.Addr = host
		}
	}
	if h, _, err := net.SplitHostPort(c.remote); err == nil && info.TLS != nil {
		info.Addr = h
	}
	c.r, c.w = wire.NewReader(c.nc), wire.NewWriter(c.nc)

	major, err := wire.ReadPreface(c.nc)
	if err != nil {
		log.Info("bad preface", "err", err)
		return
	}
	if _, isUnix := nc.(*net.UnixConn); !isUnix && s.cfg.TLS == nil && !isLoopback(nc.RemoteAddr()) {
		log.Warn("refused a non-TLS connection from a non-loopback address")
		c.connError("tls_required", "this server only accepts TLS connections from other machines")
		return
	}
	if major < wire.Major {
		c.connError("version_mismatch", "the server speaks protocol 1.0")
		return
	}
	// Connections still logging in are bounded too, so a flood of them
	// can't use up the server before any user is known.
	lim := s.cfg.Engine.Limits()
	s.mu.Lock()
	if lim.MaxConnections > 0 && s.handshaking >= lim.MaxConnections {
		s.mu.Unlock()
		log.Warn("refused a connection: too many logging in")
		c.connError("limit", "the server has too many connections logging in; try again shortly")
		return
	}
	s.handshaking++
	s.mu.Unlock()
	err = c.handshake(info, log)
	s.mu.Lock()
	s.handshaking--
	s.mu.Unlock()
	if c.sess != nil { // admitted (admitUser), even if the handshake failed after
		defer s.releaseUser(c)
	}
	if err != nil {
		var ce *wire.ConnError
		if errors.As(err, &ce) {
			c.connError(ce.Code, ce.Msg)
		}
		log.Info("handshake failed", "err", err)
		return
	}
	nc.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		c.connError("shutting_down", "the server is shutting down")
		return
	}
	s.conns[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.sess.Close()
	}()
	if info.TLS == nil {
		if _, isUnix := nc.(*net.UnixConn); !isUnix {
			log.Warn("insecure connection (no TLS)", "user", c.sess.User())
		}
	}
	log.Info("connected", "user", c.sess.User())
	go c.housekeeping()
	c.loop(log)
	log.Info("disconnected")
}

// handshake negotiates HELLO and runs the login.
func (c *conn) handshake(info authn.ConnInfo, log interface{ Info(string, ...any) }) error {
	s := c.srv
	in, err := c.r.ReadMessage()
	if err != nil {
		return err
	}
	hello, ok := in.Msg.(*wire.Hello)
	if !ok || in.Stream != 0 {
		return &wire.ConnError{Code: "protocol_error", Msg: "expected HELLO"}
	}
	if _, ok := hello.Versions[wire.Major]; !ok {
		return &wire.ConnError{Code: "version_mismatch", Msg: "the server speaks protocol 1.0"}
	}
	offered := []string{"blob-range", "blob-write", "queue-wait"}
	if s.cfg.Compression {
		offered = append(offered, "deflate")
	}
	for _, f := range hello.Features {
		if slices.Contains(offered, f) {
			c.features = append(c.features, f)
		}
	}
	slices.Sort(c.features)
	c.inline = s.cfg.InlineLimit
	if hello.InlineLimit > 0 {
		c.inline = min(c.inline, max(hello.InlineLimit, wire.MinInlineLimit))
	}
	c.connWin = wire.NewWindow(s.cfg.ConnWindow)
	mechs := s.auth.Mechanisms(info)
	ok2 := &wire.HelloOK{Major: wire.Major, Minor: wire.Minor, Features: c.features, ServerVersion: Version,
		AuthMechanisms: mechs, MaxFrameSize: int64(s.cfg.MaxFrameSize), MaxStreams: int64(s.cfg.MaxStreams),
		StreamWindow: s.cfg.StreamWindow, ConnWindow: s.cfg.ConnWindow, InlineLimit: c.inline}
	if err := c.w.WriteMessage(0, ok2, false); err != nil {
		return err
	}
	c.r.SetMaxFrameSize(s.cfg.MaxFrameSize)
	c.w.SetMaxFrameSize(s.cfg.MaxFrameSize)

	in, err = c.r.ReadMessage()
	if err != nil {
		return err
	}
	first, ok := in.Msg.(*wire.Auth)
	if !ok || in.Stream != 0 {
		return &wire.ConnError{Code: "protocol_error", Msg: "expected AUTH"}
	}
	if first.User == "" {
		first.User = hello.User
	}
	user, final, err := s.auth.Serve(c.ctx, &loginTransport{c}, info, first)
	if err != nil {
		return &wire.ConnError{Code: "auth_failed", Msg: "login failed"}
	}
	if err := s.admitUser(user); err != nil {
		return err
	}
	c.sess = session.New(s.cfg.Engine, user)
	c.sess.Directory = s.directory
	c.sess.NoWait = !slices.Contains(c.features, "queue-wait")
	db := ""
	if hello.Database != "" {
		st := &lang.Use{Name: hello.Database}
		if _, err := c.sess.ExecStmt(c.ctx, st, nil); err == nil {
			db = hello.Database
		} else {
			log.Info("couldn't use the requested database", "database", hello.Database, "err", err)
		}
	}
	if slices.Contains(c.features, "deflate") {
		c.w.SetCompression(true)
	}
	return c.w.WriteMessage(0, &wire.AuthOK{Data: final, SessionID: c.id, User: user, Database: db}, false)
}

// loginTransport carries the login exchange on stream 0.
type loginTransport struct{ c *conn }

func (t *loginTransport) Send(m wire.Message) error { return t.c.w.WriteMessage(0, m, false) }

func (t *loginTransport) Recv() (wire.Message, error) {
	in, err := t.c.r.ReadMessage()
	if err != nil {
		return nil, err
	}
	if in.Stream != 0 {
		return nil, &wire.ConnError{Code: "protocol_error", Msg: "requests before login"}
	}
	return in.Msg, nil
}

func (c *conn) connError(code, msg string) {
	if c.w == nil {
		c.w = wire.NewWriter(c.nc)
	}
	c.w.WriteMessage(0, &wire.ConnErrorMsg{Code: code, Message: msg}, false)
}

func (c *conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	if c.connWin != nil {
		c.connWin.Close(errors.New("connection closed"))
	}
	c.nc.Close()
}

// loop reads messages and dispatches them until the connection ends.
func (c *conn) loop(log interface {
	Info(string, ...any)
	Warn(string, ...any)
}) {
	for {
		in, err := c.r.ReadMessage()
		if err != nil {
			var ce *wire.ConnError
			if errors.As(err, &ce) {
				log.Warn("protocol error", "err", err)
				c.connError(ce.Code, ce.Msg)
			}
			return
		}
		c.activity.Store(time.Now().UnixNano())
		if in.Stream == 0 {
			if !c.control(in.Msg) {
				return
			}
			continue
		}
		switch m := in.Msg.(type) {
		case *wire.WindowUpdate:
			if st := c.stream(in.Stream); st != nil {
				if err := st.win.Add(m.Increment); err != nil {
					c.connError("flow_control", err.Error())
					return
				}
			}
		case *wire.Cancel:
			if st := c.stream(in.Stream); st != nil {
				st.cancelled.Store(true)
				st.cancel()
			}
		case *wire.BlobWrite:
			if st := c.stream(in.Stream); st != nil && st.upload != nil {
				// The next piece of an upload. Writing blocks until the
				// upload has stored the previous one; errors mean it
				// ended early and has answered already.
				if _, err := st.upload.Write(m.Data); err == nil && m.Last {
					st.upload.Close()
				}
				continue
			}
			c.mu.Lock()
			late := in.Stream <= c.lastStream
			c.mu.Unlock()
			if late {
				continue // a piece of an upload that already ended with an error
			}
			if !slices.Contains(c.features, "blob-write") {
				c.connError("protocol_error", "BLOB_WRITE without the blob-write feature")
				return
			}
			if err := c.start(in.Stream, in.Msg); err != nil {
				c.connError("protocol_error", err.Error())
				return
			}
		case *wire.Query, *wire.Prepare, *wire.Execute, *wire.CloseStatement, *wire.BlobRead:
			if err := c.start(in.Stream, in.Msg); err != nil {
				c.connError("protocol_error", err.Error())
				return
			}
		default:
			c.connError("protocol_error", "unexpected message from the client")
			return
		}
	}
}

// control handles a stream 0 message; it returns false to close.
func (c *conn) control(m wire.Message) bool {
	switch m := m.(type) {
	case *wire.Ping:
		c.w.WriteMessage(0, &wire.Pong{Data: m.Data}, false)
	case *wire.Pong:
		select {
		case c.pong <- struct{}{}:
		default:
		}
	case *wire.WindowUpdate:
		if err := c.connWin.Add(m.Increment); err != nil {
			c.connError("flow_control", err.Error())
			return false
		}
	case *wire.GoAway:
		// The client is leaving: finish what's running, then close.
		c.mu.Lock()
		c.goingAway = true
		idle := len(c.streams) == 0
		c.mu.Unlock()
		return !idle
	case *wire.ConnErrorMsg:
		return false
	default:
		c.connError("protocol_error", "unexpected message on stream 0")
		return false
	}
	return true
}

func (c *conn) stream(id uint32) *stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

// start opens a stream for a request.
func (c *conn) start(id uint32, m wire.Message) error {
	c.mu.Lock()
	if id%2 == 0 || id <= c.lastStream {
		c.mu.Unlock()
		return errors.New("stream IDs from the client must be odd and increasing")
	}
	c.lastStream = id
	if c.goingAway {
		c.mu.Unlock()
		c.w.WriteMessage(id, &wire.Error{Code: "shutting_down", Message: "the connection is closing", StatementIndex: -1}, true)
		return nil
	}
	if c.open >= c.srv.cfg.MaxStreams {
		c.mu.Unlock()
		c.w.WriteMessage(id, &wire.Error{Code: "limit", Message: "too many requests at once on this connection", StatementIndex: -1}, true)
		return nil
	}
	st := &stream{id: id, win: wire.NewWindow(c.srv.cfg.StreamWindow)}
	st.ctx, st.cancel = context.WithCancel(c.ctx)
	var body io.Reader
	if bw, ok := m.(*wire.BlobWrite); ok {
		// Later pieces come through a pipe from the read loop.
		pr, pw := io.Pipe()
		st.upload = pw
		body = io.MultiReader(bytes.NewReader(bw.Data), pr)
		if bw.Last {
			pw.Close()
		}
		context.AfterFunc(st.ctx, func() { pr.CloseWithError(context.Canceled) })
	}
	c.streams[id] = st
	c.open++
	c.mu.Unlock()
	c.requests.Add(1)
	c.srv.wg.Add(1)
	go func() {
		defer c.srv.wg.Done()
		defer c.finish(st)
		if body != nil {
			c.serveUpload(st, m.(*wire.BlobWrite), body)
			return
		}
		c.serve(st, m)
	}()
	return nil
}

// release frees a stream's place under MaxStreams. The stream stays
// open (for going away and idle timeouts) until finish.
func (c *conn) release(st *stream) {
	c.mu.Lock()
	if !st.released {
		st.released = true
		c.open--
	}
	c.mu.Unlock()
}

// finish removes a finished stream; the last one closes a connection
// that is going away.
func (c *conn) finish(st *stream) {
	st.cancel()
	c.release(st)
	c.mu.Lock()
	delete(c.streams, st.id)
	done := c.goingAway && len(c.streams) == 0
	c.mu.Unlock()
	if done {
		c.close()
	}
}

// goAway tells the client the connection is closing after the streams it
// has already opened.
func (c *conn) goAway(code, msg string) {
	c.mu.Lock()
	c.goingAway = true
	last, idle := c.lastStream, len(c.streams) == 0
	c.mu.Unlock()
	c.w.WriteMessage(0, &wire.GoAway{LastStream: int64(last), Code: code, Message: msg}, false)
	if idle {
		c.close()
	}
}

// housekeeping sends keepalive pings, closes idle connections and rolls
// back idle transactions.
func (c *conn) housekeeping() {
	cfg := c.srv.cfg
	tick := min(cfg.KeepAlive, cfg.TxnIdleTimeout, cfg.IdleTimeout) / 4
	t := time.NewTicker(max(tick, 10*time.Millisecond))
	defer t.Stop()
	var pinged time.Time
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.pong:
			pinged = time.Time{}
		case <-t.C:
			last := time.Unix(0, c.activity.Load())
			if c.activity.Load() == 0 {
				last = c.connected
			}
			silent := time.Since(last)
			c.mu.Lock()
			busy := len(c.streams) > 0
			c.mu.Unlock()
			switch {
			case !pinged.IsZero() && time.Since(pinged) > cfg.PingTimeout:
				c.connError("timeout", "no answer to PING")
				c.close()
				return
			case !busy && silent > cfg.IdleTimeout:
				c.goAway("idle", "the connection was idle")
				return
			case silent > cfg.KeepAlive && pinged.IsZero():
				data := make([]byte, 8)
				rand.Read(data)
				pinged = time.Now()
				c.w.WriteMessage(0, &wire.Ping{Data: data}, false)
			}
			c.sess.ExpireIdle(cfg.TxnIdleTimeout)
		}
	}
}
