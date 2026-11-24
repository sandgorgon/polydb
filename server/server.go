// Package server is the PolyDB network server (spec/protocol.md): it
// accepts TLS and Unix socket connections, runs the handshake and login,
// and serves requests on multiplexed streams with flow control,
// cancellation, prepared statements, transactions and blob references.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/authn"
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/session"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// Version is the server's version, sent in HELLO_OK.
const Version = "0.3.0-dev"

// Config configures a server. Zero values take the defaults of
// spec/protocol.md.
type Config struct {
	Engine *engine.Engine
	// TLS is used for TCP connections; see IdentityTLS and CATLS. Without
	// it, TCP connections are only accepted from loopback addresses
	// (--insecure, for development).
	TLS *tls.Config
	// PeerUsers maps OS users to PolyDB users for PEER logins on Unix
	// sockets.
	PeerUsers map[string]string

	MaxStreams   int
	MaxFrameSize int
	StreamWindow int64
	ConnWindow   int64
	// InlineLimit is the largest value sent inline; larger text, bytes
	// and images go as blob references (default 16 KiB, from 1 KiB to
	// 64 MiB). A client may ask for less.
	InlineLimit int64
	// Compression allows the deflate feature.
	Compression bool

	HandshakeTimeout time.Duration // connect to AUTH_OK (10 s)
	IdleTimeout      time.Duration // no streams and no traffic (30 min)
	KeepAlive        time.Duration // PING after this much silence (60 s)
	PingTimeout      time.Duration // close if PONG doesn't come (30 s)
	TxnIdleTimeout   time.Duration // roll back idle transactions (5 min)
	LeaseTime        time.Duration // blob references stay valid (10 min)

	Logger *slog.Logger
}

func (c *Config) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.HandshakeTimeout, 10*time.Second)
	def(&c.IdleTimeout, 30*time.Minute)
	def(&c.KeepAlive, 60*time.Second)
	def(&c.PingTimeout, 30*time.Second)
	def(&c.TxnIdleTimeout, 5*time.Minute)
	def(&c.LeaseTime, 10*time.Minute)
	if c.MaxStreams <= 0 {
		c.MaxStreams = wire.DefaultMaxStreams
	}
	if c.MaxFrameSize <= 0 {
		c.MaxFrameSize = wire.DefaultMaxFrameSize
	}
	c.MaxFrameSize = min(max(c.MaxFrameSize, 1<<10), wire.MaxFrameSizeLimit)
	if c.StreamWindow <= 0 {
		c.StreamWindow = wire.DefaultStreamWindow
	}
	if c.ConnWindow <= 0 {
		c.ConnWindow = wire.DefaultConnWindow
	}
	if c.InlineLimit <= 0 {
		c.InlineLimit = wire.DefaultInlineLimit
	}
	c.InlineLimit = min(max(c.InlineLimit, wire.MinInlineLimit), wire.MaxInlineLimit)
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// IdentityTLS returns the TLS configuration of identity mode: the
// server's 9auth certificate, TLS 1.3, and a request (not a requirement)
// for the client's certificate, so password users can connect too
// (spec/protocol.md §1).
func IdentityTLS(id *auth9.Identity) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.TLSCertificate()},
		ClientAuth:   tls.RequestClientCert,
		NextProtos:   []string{wire.ALPN},
	}
}

// CATLS returns the TLS configuration of CA mode, from a certificate and
// key in PEM files.
func CATLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		NextProtos:   []string{wire.ALPN},
	}, nil
}

// Server serves PolyDB connections.
type Server struct {
	cfg    Config
	auth   *authn.ServerConfig
	leases *leases
	ids    *value.IDGenerator

	mu        sync.Mutex
	listeners map[net.Listener]bool
	conns     map[*conn]bool
	shutdown  bool
	wg        sync.WaitGroup
	done      chan struct{}
}

// New returns a server.
func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("server: no engine")
	}
	cfg.defaults()
	secret := make([]byte, 32)
	rand.Read(secret)
	s := &Server{
		cfg:       cfg,
		leases:    newLeases(cfg.LeaseTime),
		ids:       value.NewIDGenerator(0),
		listeners: map[net.Listener]bool{},
		conns:     map[*conn]bool{},
		done:      make(chan struct{}),
	}
	s.auth = &authn.ServerConfig{
		Security: cfg.Engine.Security(),
		Secret:   secret,
		Limiter:  authn.NewLimiter(),
		PeerUser: func(os string) (string, bool) {
			u, ok := cfg.PeerUsers[os]
			return u, ok
		},
	}
	go s.janitor()
	return s, nil
}

// janitor drops expired blob leases.
func (s *Server) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.leases.expire()
		case <-s.done:
			s.leases.close() // lets the blob stores delete what leases kept
			return
		}
	}
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("server: closed")

// Serve accepts connections on l until Shutdown. Unix socket listeners
// serve without TLS; TCP listeners use Config.TLS.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return ErrServerClosed
	}
	s.listeners[l] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, l)
		s.mu.Unlock()
	}()
	if s.cfg.TLS == nil {
		if _, isUnix := l.(*net.UnixListener); !isUnix {
			s.cfg.Logger.Warn("serving TCP without TLS: only loopback connections are accepted (--insecure, for development)",
				"addr", l.Addr())
		}
	}
	for {
		nc, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.shutdown
			s.mu.Unlock()
			if closed {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(nc)
		}()
	}
}

// Shutdown stops accepting connections, sends GOAWAY to every connection,
// lets running requests finish, and closes the connections when they are
// done or ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.shutdown {
		s.shutdown = true
		close(s.done)
	}
	for l := range s.listeners {
		l.Close()
	}
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.goAway("shutting_down", "the server is shutting down")
	}
	finished := make(chan struct{})
	go func() { s.wg.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.close()
		}
		s.mu.Unlock()
		<-finished
		return ctx.Err()
	}
}

// directory lists the sessions for show sessions.
func (s *Server) directory() []session.Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.Info
	for c := range s.conns {
		if c.sess == nil {
			continue
		}
		out = append(out, session.Info{ID: c.id.String(), User: c.sess.User(), Database: c.sess.Database(),
			Remote: c.remote, Connected: c.connected, Requests: int(c.requests.Load())})
	}
	return out
}

func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
