// Package client is the Go client for a PolyDB server (spec/protocol.md
// §13). Dial returns a polydb.DB, the same interface an embedded engine
// implements.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/authn"
	"github.com/sandgorgon/polydb/wire"
)

// Version is this client's version, sent in HELLO.
const Version = "0.3.0-dev"

// Option configures Dial.
type Option func(*options)

type options struct {
	identity      *auth9.Identity
	password      func() (string, error)
	passwordGiven bool
	fingerprint   string
	knownPeers    string
	confirmPeer   func(addr, fingerprint string) bool
	systemCAs     bool
	tlsConfig     *tls.Config
	insecure      bool
	inlineLimit   int64
	compression   bool
	handshakeTime time.Duration
	trace         func(dir string, stream uint32, m wire.Message)
}

// WithIdentity presents a 9auth identity in the TLS handshake, so the
// client can log in with KEY when the identity is registered to the user.
func WithIdentity(id *auth9.Identity) Option { return func(o *options) { o.identity = id } }

// WithPassword logs in with a password (SCRAM).
func WithPassword(pw string) Option {
	return func(o *options) {
		o.password = func() (string, error) { return pw, nil }
		o.passwordGiven = true
	}
}

// WithPasswordFunc asks for a password only when one is needed: PEER
// login on a Unix socket is tried first when the server offers it.
func WithPasswordFunc(f func() (string, error)) Option { return func(o *options) { o.password = f } }

// WithServerFingerprint trusts the server only if its 9auth identity has
// this fingerprint.
func WithServerFingerprint(fp string) Option {
	return func(o *options) { o.fingerprint = strings.ToLower(fp) }
}

// WithKnownPeers trusts servers by fingerprint as recorded in a 9auth
// known-peers file (auth9.KnownPeersPath()), like SSH's known_hosts: a
// server seen before must present the same fingerprint, and a new one is
// recorded on first use (after WithConfirmPeer, if given, agrees).
func WithKnownPeers(path string) Option { return func(o *options) { o.knownPeers = path } }

// WithConfirmPeer asks before trusting a server that isn't in the
// known-peers file yet.
func WithConfirmPeer(f func(addr, fingerprint string) bool) Option {
	return func(o *options) { o.confirmPeer = f }
}

// WithSystemCAs verifies the server's certificate against the system's
// certificate authorities (the server's CA mode).
func WithSystemCAs() Option { return func(o *options) { o.systemCAs = true } }

// WithTLSConfig uses a TLS configuration as given, for custom trust.
func WithTLSConfig(c *tls.Config) Option { return func(o *options) { o.tlsConfig = c } }

// WithInsecure connects over TCP without TLS, which servers only accept
// from loopback addresses (development).
func WithInsecure() Option { return func(o *options) { o.insecure = true } }

// WithInlineLimit asks the server to send values larger than n bytes as
// blob references, which are fetched only when read.
func WithInlineLimit(n int64) Option { return func(o *options) { o.inlineLimit = n } }

// WithCompression asks for deflate compression of messages.
func WithCompression() Option { return func(o *options) { o.compression = true } }

// WithTrace calls f for every message sent ("send") and received
// ("recv"), for debugging. Passwords and keys travel inside AUTH
// messages; f should redact them.
func WithTrace(f func(dir string, stream uint32, m wire.Message)) Option {
	return func(o *options) { o.trace = f }
}

// Target is a parsed connection URL.
type Target struct {
	Network  string // "tcp" or "unix"
	Address  string // host:port or socket path
	Host     string // for TLS server names and known-peers entries
	User     string
	Password string // from the URL; discouraged
	Database string
}

// ParseURL parses polydb://user@host:port/database or
// polydb+unix:///path/to.sock?db=database&user=name.
func ParseURL(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, err
	}
	var t Target
	if u.User != nil {
		t.User = u.User.Username()
		t.Password, _ = u.User.Password()
	}
	q := u.Query()
	if v := q.Get("user"); v != "" {
		t.User = v
	}
	switch u.Scheme {
	case "polydb":
		t.Network, t.Host = "tcp", u.Hostname()
		if t.Host == "" {
			return Target{}, errors.New("client: the URL has no host")
		}
		port := u.Port()
		if port == "" {
			port = strconv.Itoa(wire.DefaultPort)
		}
		t.Address = net.JoinHostPort(t.Host, port)
		t.Database = strings.TrimPrefix(u.Path, "/")
	case "polydb+unix":
		t.Network, t.Address = "unix", u.Path
		if t.Address == "" {
			return Target{}, errors.New("client: the URL has no socket path")
		}
		t.Database = q.Get("db")
	default:
		return Target{}, fmt.Errorf("client: unknown URL scheme %q (use polydb:// or polydb+unix://)", u.Scheme)
	}
	return t, nil
}

// Dial connects to a server and logs in.
func Dial(ctx context.Context, rawURL string, opts ...Option) (polydb.DB, error) {
	t, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	o := &options{handshakeTime: 10 * time.Second}
	for _, opt := range opts {
		opt(o)
	}
	if o.password == nil && t.Password != "" {
		pw := t.Password
		o.password = func() (string, error) { return pw, nil }
		o.passwordGiven = true
	}
	return dial(ctx, t, o)
}

// dial connects. If a KEY login fails (the identity isn't registered to
// the user) and a password is available, it connects again and logs in
// with the password: the server closes a connection after a failed login.
func dial(ctx context.Context, t Target, o *options) (*Conn, error) {
	c, err := dialOnce(ctx, t, o, false)
	if polydb.CodeOf(err) == "auth_failed" && o.identity != nil && o.password != nil {
		return dialOnce(ctx, t, o, true)
	}
	return c, err
}

func dialOnce(ctx context.Context, t Target, o *options, skipKey bool) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, o.handshakeTime)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, t.Network, t.Address)
	if err != nil {
		return nil, err
	}
	var tlsState *tls.ConnectionState
	if t.Network == "tcp" && !o.insecure {
		cfg, err := o.tls(t)
		if err != nil {
			nc.Close()
			return nil, err
		}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		state := tc.ConnectionState()
		tlsState, nc = &state, tc
	}
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	c, err := handshake(nc, t, o, tlsState, skipKey)
	if err != nil {
		nc.Close()
		return nil, err
	}
	nc.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

// tls builds the TLS configuration from the trust options.
func (o *options) tls(t Target) (*tls.Config, error) {
	if o.tlsConfig != nil {
		cfg := o.tlsConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = t.Host
		}
		cfg.NextProtos = []string{wire.ALPN}
		return cfg, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: t.Host, NextProtos: []string{wire.ALPN}}
	if o.identity != nil {
		cfg.Certificates = []tls.Certificate{o.identity.TLSCertificate()}
	}
	switch {
	case o.systemCAs:
		return cfg, nil
	case o.fingerprint != "" || o.knownPeers != "":
		// 9auth identities aren't signed by a CA: trust is by fingerprint.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return o.verifyPeer(t.Address, cs.PeerCertificates) }
		return cfg, nil
	}
	return nil, errors.New("client: no way to trust the server: use WithServerFingerprint, WithKnownPeers or WithSystemCAs")
}

// verifyPeer checks the server's fingerprint against a pinned one or the
// known-peers file.
func (o *options) verifyPeer(addr string, certs []*x509.Certificate) error {
	if len(certs) == 0 {
		return errors.New("client: the server presented no certificate")
	}
	fp, err := auth9.FingerprintOf(certs[0])
	if err != nil {
		return fmt.Errorf("client: the server's certificate isn't a 9auth identity: %w", err)
	}
	if o.fingerprint != "" {
		if fp != o.fingerprint {
			return fmt.Errorf("client: the server's fingerprint is %s, not the pinned %s", fp, o.fingerprint)
		}
		return nil
	}
	known, err := auth9.LoadKnownPeers(o.knownPeers)
	if err != nil {
		return err
	}
	if want, ok := known[addr]; ok {
		if want != fp {
			return fmt.Errorf("client: WARNING: the server at %s now presents fingerprint %s, but %s was recorded before. "+
				"It may be an impostor; refusing to connect. If the server's identity really changed, remove its line from %s",
				addr, fp, want, o.knownPeers)
		}
		return nil
	}
	if o.confirmPeer != nil && !o.confirmPeer(addr, fp) {
		return fmt.Errorf("client: the server's fingerprint %s wasn't accepted", fp)
	}
	return auth9.RememberPeer(o.knownPeers, addr, fp)
}

// handshake sends the preface and HELLO and logs in.
func handshake(nc net.Conn, t Target, o *options, tlsState *tls.ConnectionState, skipKey bool) (*Conn, error) {
	c := newConn(nc, o.trace)
	if err := wire.WritePreface(nc); err != nil {
		return nil, err
	}
	features := []string{"blob-range", "blob-write", "queue-wait"}
	if o.compression && t.Network != "unix" {
		features = append(features, "deflate")
	}
	hello := &wire.Hello{Versions: map[int]int{wire.Major: wire.Minor}, Features: features, ClientName: "polydb-go",
		ClientVersion: Version, Database: t.Database, User: t.User, InlineLimit: o.inlineLimit}
	if err := c.send(0, hello, false); err != nil {
		return nil, err
	}
	m, err := c.recv0()
	if err != nil {
		return nil, err
	}
	ok, isOK := m.(*wire.HelloOK)
	if !isOK {
		return nil, unexpected(m)
	}
	c.hello = ok
	c.r.SetMaxFrameSize(int(ok.MaxFrameSize))
	c.w.SetMaxFrameSize(int(ok.MaxFrameSize))
	cfg := authn.ClientConfig{User: t.User, Password: o.password, PasswordGiven: o.passwordGiven,
		HasKey: !skipKey && o.identity != nil && tlsState != nil && len(tlsState.PeerCertificates) > 0}
	authOK, err := authn.Login(&loginTransport{c}, cfg, ok.AuthMechanisms, tlsState)
	if err != nil {
		if errors.Is(err, authn.ErrFailed) {
			return nil, &polydb.Error{Code: "auth_failed", Message: "login failed", StatementIndex: -1}
		}
		return nil, err
	}
	c.auth = authOK
	if contains(ok.Features, "deflate") {
		c.w.SetCompression(true)
	}
	c.streamWindow, c.connCredit = ok.StreamWindow, wire.NewCredit(ok.ConnWindow)
	return c, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

type loginTransport struct{ c *Conn }

func (t *loginTransport) Send(m wire.Message) error   { return t.c.send(0, m, false) }
func (t *loginTransport) Recv() (wire.Message, error) { return t.c.recv0() }

// unexpected turns an unexpected handshake message into an error.
func unexpected(m wire.Message) error {
	if e, ok := m.(*wire.ConnErrorMsg); ok {
		return &polydb.Error{Code: e.Code, Message: e.Message, StatementIndex: -1}
	}
	return fmt.Errorf("client: unexpected %T", m)
}
