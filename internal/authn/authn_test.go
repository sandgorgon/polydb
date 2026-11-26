package authn

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/wire"
)

// TestScramRFC7677 reproduces the SCRAM-SHA-256 example of RFC 7677 §3.
func TestScramRFC7677(t *testing.T) {
	nonces := []string{"rOprNGfwEbeRWgbNEkqO", "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"}
	newNonce = func() string { n := nonces[0]; nonces = nonces[1:]; return n }
	defer func() { newNonce = nonce }()

	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	creds, _ := security.DeriveCredentials("pencil", salt, 4096)
	c := NewScramClient("user", "pencil", nil, false)
	s := NewScramServer(Scram, func(string) *security.Credentials { return &creds }, nil, nil, false)

	first := c.First()
	if string(first) != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first %s", first)
	}
	serverFirst, err := s.Start(first)
	if err != nil || string(serverFirst) != "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096" {
		t.Fatalf("server-first %s, %v", serverFirst, err)
	}
	final, err := c.Final(serverFirst)
	if err != nil || string(final) != "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=" {
		t.Fatalf("client-final %s, %v", final, err)
	}
	serverFinal, err := s.Finish(final)
	if err != nil || string(serverFinal) != "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Fatalf("server-final %s, %v", serverFinal, err)
	}
	if err := c.Verify(serverFinal); err != nil {
		t.Error(err)
	}
	if err := c.Verify([]byte("v=AAAA")); err == nil {
		t.Error("an impostor's signature was accepted")
	}
}

func TestScramRefusals(t *testing.T) {
	creds, _ := security.DeriveCredentials("pw", []byte("salt"), 4096)
	lookup := func(string) *security.Credentials { return &creds }
	cb := []byte("binding-of-this-connection")
	run := func(c *ScramClient, s *ScramServer) error {
		sf, err := s.Start(c.First())
		if err != nil {
			return err
		}
		cf, err := c.Final(sf)
		if err != nil {
			return err
		}
		_, err = s.Finish(cf)
		return err
	}
	cases := map[string]struct {
		c    *ScramClient
		s    *ScramServer
		want bool
	}{
		"plus":             {NewScramClient("u", "pw", cb, true), NewScramServer(ScramPlus, lookup, nil, cb, true), true},
		"plain":            {NewScramClient("u", "pw", nil, false), NewScramServer(Scram, lookup, nil, nil, false), true},
		"wrong password":   {NewScramClient("u", "nope", cb, true), NewScramServer(ScramPlus, lookup, nil, cb, true), false},
		"binding mismatch": {NewScramClient("u", "pw", []byte("relayed"), true), NewScramServer(ScramPlus, lookup, nil, cb, true), false},
		"downgrade":        {NewScramClient("u", "pw", nil, true), NewScramServer(Scram, lookup, nil, nil, true), false},
		"plus without cb":  {NewScramClient("u", "pw", cb, true), NewScramServer(ScramPlus, lookup, nil, nil, true), false},
	}
	for name, c := range cases {
		if err := run(c.c, c.s); (err == nil) != c.want {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// Unknown users get stable fake credentials and fail at the end.
	cfg := &ServerConfig{Secret: []byte("secret")}
	s := NewScramServer(Scram, func(string) *security.Credentials { return nil }, cfg.fake, nil, false)
	sf1, _ := s.Start([]byte("n,,n=ghost,r=abc"))
	s2 := NewScramServer(Scram, func(string) *security.Credentials { return nil }, cfg.fake, nil, false)
	sf2, _ := s2.Start([]byte("n,,n=ghost,r=abc"))
	salt := func(b []byte) string { return strings.Split(string(b), ",")[1] }
	if salt(sf1) != salt(sf2) {
		t.Error("fake salts differ between attempts")
	}
	c := NewScramClient("ghost", "x", nil, false)
	c.nonce = "abc"
	c.First()
	cf, _ := c.Final(sf1)
	if _, err := s.Finish(cf); !errors.Is(err, ErrFailed) {
		t.Errorf("unknown user: %v", err)
	}
	// Malformed messages and weak parameters are refused.
	for _, bad := range []string{"", "x,,n=u,r=a", "n,,n=u", "n,,n=a=b,r=x", "n,,n=,r=x", "n,,n=u,r=a,m=ext"} {
		if _, err := NewScramServer(Scram, lookup, cfg.fake, nil, false).Start([]byte(bad)); err == nil {
			t.Errorf("client-first %q accepted", bad)
		}
	}
	weak := NewScramClient("u", "pw", nil, false)
	weak.First()
	if _, err := weak.Final([]byte("r=" + weak.nonce + "x,s=c2FsdA==,i=1")); err == nil {
		t.Error("1 iteration accepted")
	}
	if _, err := weak.Final([]byte("r=other,s=c2FsdA==,i=4096")); err == nil {
		t.Error("foreign nonce accepted")
	}
	if n, err := unSaslName(saslName("a=b,c")); err != nil || n != "a=b,c" {
		t.Errorf("saslname round trip: %q %v", n, err)
	}
}

// pipeTransport carries login messages over one side of a connection.
type pipeTransport struct {
	r *wire.Reader
	w *wire.Writer
}

func newTransport(c net.Conn) *pipeTransport {
	return &pipeTransport{wire.NewReader(c), wire.NewWriter(c)}
}

func (p *pipeTransport) Send(m wire.Message) error { return p.w.WriteMessage(0, m, false) }

func (p *pipeTransport) Recv() (wire.Message, error) {
	in, err := p.r.ReadMessage()
	return in.Msg, err
}

// serverSide runs the server's half of a login on t.
func serverSide(cfg *ServerConfig, t *pipeTransport, info ConnInfo) (string, error) {
	m, err := t.Recv()
	if err != nil {
		return "", err
	}
	first, ok := m.(*wire.Auth)
	if !ok {
		return "", errors.New("expected AUTH")
	}
	user, final, err := cfg.Serve(context.Background(), t, info, first)
	if err != nil {
		t.Send(&wire.ConnErrorMsg{Code: "auth_failed", Message: "login failed"})
		return "", err
	}
	return user, t.Send(&wire.AuthOK{Data: final, User: user})
}

type tlsPair struct {
	client, server *tls.Conn
}

// tlsConns makes a TLS connection over a pipe. The server asks for a
// client certificate without requiring one, as the PolyDB server does.
func tlsConns(t *testing.T, clientID *auth9.Identity) tlsPair {
	t.Helper()
	serverID, err := auth9.LoadFrom(filepath.Join(t.TempDir(), "server"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	scfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverID.TLSCertificate()},
		ClientAuth: tls.RequestClientCert}
	var ccfg *tls.Config
	if clientID != nil {
		ccfg = clientID.ClientTLSConfig(func(string) bool { return true })
	} else {
		ccfg = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	}
	p := tlsPair{tls.Client(a, ccfg), tls.Server(b, scfg)}
	errc := make(chan error, 1)
	go func() { errc <- p.server.Handshake() }()
	if err := p.client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	// Close the pipe itself: a TLS close would wait up to 5 s for the
	// peer to read its close_notify alert.
	t.Cleanup(func() { a.Close(); b.Close() })
	return p
}

func newStore(t *testing.T) *security.Store {
	sec := security.New(security.MinIterations)
	if err := sec.CreateUser("ramon", "correct horse"); err != nil {
		t.Fatal(err)
	}
	return sec
}

// login runs both sides and returns the server's user and the client's
// error.
func login(t *testing.T, cfg *ServerConfig, ccfg ClientConfig, client, server net.Conn, info ConnInfo) (string, error) {
	ct, st := newTransport(client), newTransport(server)
	type res struct {
		user string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		u, err := serverSide(cfg, st, info)
		done <- res{u, err}
	}()
	var cs *tls.ConnectionState
	if c, ok := client.(*tls.Conn); ok {
		state := c.ConnectionState()
		cs = &state
	}
	_, err := Login(ct, ccfg, cfg.Mechanisms(info), cs)
	r := <-done
	if err == nil && r.err != nil {
		t.Fatalf("client succeeded but server failed: %v", r.err)
	}
	return r.user, err
}

func password(pw string) func() (string, error) { return func() (string, error) { return pw, nil } }

func TestLoginOverTLS(t *testing.T) {
	sec := newStore(t)
	cfg := &ServerConfig{Security: sec, Secret: []byte("s")}

	// SCRAM-SHA-256-PLUS, bound to this TLS connection.
	p := tlsConns(t, nil)
	state := p.server.ConnectionState()
	info := ConnInfo{TLS: &state, Addr: "a"}
	if m := cfg.Mechanisms(info); strings.Join(m, " ") != ScramPlus {
		t.Errorf("mechanisms without a client certificate: %v", m)
	}
	if u, err := login(t, cfg, ClientConfig{User: "ramon", Password: password("correct horse")}, p.client, p.server, info); err != nil || u != "ramon" {
		t.Errorf("scram-plus: %q, %v", u, err)
	}
	p = tlsConns(t, nil)
	state = p.server.ConnectionState()
	info.TLS = &state
	if _, err := login(t, cfg, ClientConfig{User: "ramon", Password: password("wrong")}, p.client, p.server, info); !errors.Is(err, ErrFailed) {
		t.Errorf("wrong password: %v", err)
	}

	// KEY: the client's 9auth identity, registered to the user.
	id, err := auth9.LoadFrom(filepath.Join(t.TempDir(), "client"))
	if err != nil {
		t.Fatal(err)
	}
	p = tlsConns(t, id)
	state = p.server.ConnectionState()
	info.TLS = &state
	if m := cfg.Mechanisms(info); m[0] != Key {
		t.Errorf("mechanisms with a client certificate: %v", m)
	}
	if _, err := login(t, cfg, ClientConfig{User: "ramon", HasKey: true}, p.client, p.server, info); !errors.Is(err, ErrFailed) {
		t.Errorf("unregistered key: %v", err)
	}
	if err := sec.AddKey("ramon", id.Fingerprint(), "laptop"); err != nil {
		t.Fatal(err)
	}
	p = tlsConns(t, id)
	state = p.server.ConnectionState()
	info.TLS = &state
	if u, err := login(t, cfg, ClientConfig{User: "ramon", HasKey: true}, p.client, p.server, info); err != nil || u != "ramon" {
		t.Errorf("key: %q, %v", u, err)
	}
	sec.CreateUser("eve", "")
	p = tlsConns(t, id)
	state = p.server.ConnectionState()
	info.TLS = &state
	if _, err := login(t, cfg, ClientConfig{User: "eve", HasKey: true}, p.client, p.server, info); !errors.Is(err, ErrFailed) {
		t.Errorf("someone else's key: %v", err)
	}
}

func TestLoginOverUnixSocket(t *testing.T) {
	sec := newStore(t)
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	cfg := &ServerConfig{Security: sec, Secret: []byte("s"), PeerUser: func(os string) (string, bool) {
		return "ramon", os == me.Username
	}}
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	peer, err := PeerUser(server.(*net.UnixConn))
	if err != nil || peer != me.Username {
		t.Fatalf("peer user %q, %v (want %s)", peer, err, me.Username)
	}
	info := ConnInfo{PeerUser: peer, Addr: "unix"}
	if m := cfg.Mechanisms(info); strings.Join(m, " ") != Peer+" "+Scram {
		t.Errorf("mechanisms: %v", m)
	}
	if u, err := login(t, cfg, ClientConfig{User: "ramon"}, client, server, info); err != nil || u != "ramon" {
		t.Errorf("peer: %q, %v", u, err)
	}

	// SCRAM without channel binding works where there's no TLS.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if u, err := login(t, cfg, ClientConfig{User: "ramon", Password: password("correct horse")}, a, b, ConnInfo{Addr: "x"}); err != nil || u != "ramon" {
		t.Errorf("scram: %q, %v", u, err)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter()
	l.Now = func() time.Time { return now }
	if d := l.Delay("a"); d != 0 {
		t.Errorf("first try delayed %v", d)
	}
	for i, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		l.Fail("a")
		if d := l.Delay("a"); d != want {
			t.Errorf("after %d failures: %v, want %v", i+1, d, want)
		}
	}
	for range 20 {
		l.Fail("a")
	}
	if d := l.Delay("a"); d != 30*time.Second {
		t.Errorf("cap: %v", d)
	}
	now = now.Add(10 * time.Second)
	if d := l.Delay("a"); d != 20*time.Second {
		t.Errorf("after waiting: %v", d)
	}
	if l.Delay("b") != 0 {
		t.Error("another source is delayed")
	}
	l.Succeed("a")
	if l.Delay("a") != 0 || l.Failures("a") != 0 {
		t.Error("success didn't clear the delay")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Fail("c")
	if err := l.Wait(ctx, "c"); err == nil {
		t.Error("cancelled wait returned nil")
	}
}

// FuzzScramServer feeds arbitrary client messages to the server side,
// which sees them before the client is authenticated: it must not panic
// and must not accept them.
func FuzzScramServer(f *testing.F) {
	f.Add([]byte("n,,n=user,r=abc"), []byte("c=biws,r=abcxyz,p=AAAA"))
	f.Add([]byte("p=tls-exporter,,n=a=2Cb,r=x"), []byte("c=,r=,p="))
	f.Add([]byte("y,,n=u,r=1"), []byte("c=eSws,r=1,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="))
	creds, _ := security.DeriveCredentials("pw", []byte("salt"), 4096)
	cfg := &ServerConfig{Secret: []byte("s")}
	f.Fuzz(func(t *testing.T, first, final []byte) {
		for _, mech := range []string{Scram, ScramPlus} {
			s := NewScramServer(mech, func(string) *security.Credentials { return &creds }, cfg.fake, []byte("cb"), true)
			if _, err := s.Start(first); err != nil {
				continue
			}
			if _, err := s.Finish(final); err == nil {
				t.Fatalf("accepted a forged login: %q %q", first, final)
			}
		}
	})
}
