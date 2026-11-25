package authn

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/wire"
)

// Transport carries the login messages on stream 0.
type Transport interface {
	Send(wire.Message) error
	Recv() (wire.Message, error)
}

// ConnInfo is what the server knows about a connection.
type ConnInfo struct {
	// TLS is the connection's TLS state, nil for Unix sockets and
	// --insecure TCP.
	TLS *tls.ConnectionState
	// PeerUser is the OS user of a Unix socket peer ("" if unknown or not
	// a Unix socket).
	PeerUser string
	// Addr identifies the source for slowing down repeated failures.
	Addr string
}

// ServerConfig configures logins on the server.
type ServerConfig struct {
	Security *security.Store
	// Secret makes the fake credentials of unknown users stable, so
	// probing gives the same salt every time.
	Secret []byte
	// PeerUser maps an OS user to a PolyDB user for PEER logins.
	PeerUser func(osUser string) (string, bool)
	// Limiter slows down repeated failures; nil for none.
	Limiter *Limiter
}

// Mechanisms returns the mechanisms a connection allows, in order of
// preference (spec/protocol.md §3.1).
func (cfg *ServerConfig) Mechanisms(info ConnInfo) []string {
	var out []string
	if info.TLS != nil {
		if len(info.TLS.PeerCertificates) > 0 {
			out = append(out, Key)
		}
		out = append(out, ScramPlus)
		return out
	}
	if info.PeerUser != "" && cfg.PeerUser != nil {
		if _, ok := cfg.PeerUser(info.PeerUser); ok {
			out = append(out, Peer)
		}
	}
	return append(out, Scram)
}

// Serve runs a login that starts with first, the client's first AUTH
// message. It returns the user and the data for AUTH_OK. Any failure is
// ErrFailed, after the limiter's delay for the source.
func (cfg *ServerConfig) Serve(ctx context.Context, t Transport, info ConnInfo, first *wire.Auth) (user string, final []byte, err error) {
	if cfg.Limiter != nil {
		if err := cfg.Limiter.Wait(ctx, info.Addr); err != nil {
			return "", nil, err
		}
	}
	user, final, err = cfg.serve(t, info, first)
	if cfg.Limiter != nil {
		if err != nil {
			cfg.Limiter.Fail(info.Addr)
		} else {
			cfg.Limiter.Succeed(info.Addr)
		}
	}
	if err != nil {
		return "", nil, ErrFailed
	}
	return user, final, nil
}

func (cfg *ServerConfig) serve(t Transport, info ConnInfo, first *wire.Auth) (string, []byte, error) {
	mechs := cfg.Mechanisms(info)
	if !slices.Contains(mechs, first.Mechanism) {
		return "", nil, ErrFailed
	}
	sec := cfg.Security
	switch first.Mechanism {
	case Key:
		fp, err := auth9.FingerprintOf(info.TLS.PeerCertificates[0])
		if err != nil {
			return "", nil, ErrFailed
		}
		owner, ok := sec.UserByKey(fp)
		if !ok || owner != first.User {
			return "", nil, ErrFailed
		}
		return owner, nil, nil
	case Peer:
		user, ok := cfg.PeerUser(info.PeerUser)
		if !ok || (first.User != "" && first.User != user) {
			return "", nil, ErrFailed
		}
		if _, exists := sec.User(user); !exists {
			return "", nil, ErrFailed
		}
		return user, nil, nil
	}
	lookup := func(name string) *security.Credentials {
		u, ok := sec.User(name)
		if !ok || u.Creds == nil {
			return nil
		}
		return u.Creds
	}
	s := NewScramServer(first.Mechanism, lookup, cfg.fake, ChannelBinding(info.TLS), slices.Contains(mechs, ScramPlus))
	serverFirst, err := s.Start(first.Data)
	if err != nil {
		return "", nil, err
	}
	if err := t.Send(&wire.AuthContinue{Data: serverFirst}); err != nil {
		return "", nil, err
	}
	m, err := t.Recv()
	if err != nil {
		return "", nil, err
	}
	final, ok := m.(*wire.Auth)
	if !ok {
		return "", nil, ErrFailed
	}
	serverFinal, err := s.Finish(final.Data)
	if err != nil {
		return "", nil, err
	}
	return s.User(), serverFinal, nil
}

// fake returns stable, believable credentials for a user that doesn't
// exist or has no password.
func (cfg *ServerConfig) fake(user string) security.Credentials {
	salt := security.HMAC(cfg.Secret, []byte("salt:"+user))[:16]
	key := sha256.Sum256(security.HMAC(cfg.Secret, []byte("key:"+user)))
	return security.Credentials{Salt: salt, Iterations: security.DefaultIterations, StoredKey: key[:], ServerKey: key[:]}
}

// ClientConfig configures a client's login.
type ClientConfig struct {
	User     string
	Password func() (string, error) // nil: no password available
	// PasswordGiven says the password was supplied up front, not asked for
	// on demand; without it, PEER is preferred when offered.
	PasswordGiven bool
	// HasKey reports that the client presented its 9auth identity in the
	// TLS handshake, so KEY can be tried.
	HasKey bool
}

// Login runs the client side of a login, choosing a mechanism from those
// the server offered: KEY when the client has a key, PEER when it has no
// password given up front, then SCRAM (with channel binding over TLS). It
// returns the server's AUTH_OK.
func Login(t Transport, cfg ClientConfig, offered []string, tlsState *tls.ConnectionState) (*wire.AuthOK, error) {
	has := func(m string) bool { return slices.Contains(offered, m) }
	switch {
	case cfg.HasKey && has(Key):
		return finish(t, &wire.Auth{Mechanism: Key, User: cfg.User}, nil)
	case !cfg.PasswordGiven && has(Peer):
		return finish(t, &wire.Auth{Mechanism: Peer, User: cfg.User}, nil)
	}
	if cfg.Password == nil {
		return nil, fmt.Errorf("authn: the server offers %v, and no password or key is available", offered)
	}
	var cb []byte
	if has(ScramPlus) {
		if cb = ChannelBinding(tlsState); cb == nil {
			return nil, errors.New("authn: the server wants channel binding, but this connection has no TLS")
		}
	} else if !has(Scram) {
		return nil, fmt.Errorf("authn: no usable login mechanism among %v", offered)
	}
	pw, err := cfg.Password()
	if err != nil {
		return nil, err
	}
	c := NewScramClient(cfg.User, pw, cb, has(ScramPlus))
	if err := t.Send(&wire.Auth{Mechanism: c.Mechanism(), Data: c.First()}); err != nil {
		return nil, err
	}
	m, err := t.Recv()
	if err != nil {
		return nil, err
	}
	cont, ok := m.(*wire.AuthContinue)
	if !ok {
		return nil, unexpected(m)
	}
	final, err := c.Final(cont.Data)
	if err != nil {
		return nil, err
	}
	return finish(t, &wire.Auth{Data: final}, c)
}

// finish sends the last AUTH and reads AUTH_OK, checking the SCRAM
// server signature if there is one.
func finish(t Transport, last *wire.Auth, c *ScramClient) (*wire.AuthOK, error) {
	if err := t.Send(last); err != nil {
		return nil, err
	}
	m, err := t.Recv()
	if err != nil {
		return nil, err
	}
	ok, isOK := m.(*wire.AuthOK)
	if !isOK {
		return nil, unexpected(m)
	}
	if c != nil {
		if err := c.Verify(ok.Data); err != nil {
			return nil, err
		}
	}
	return ok, nil
}

func unexpected(m wire.Message) error {
	if e, ok := m.(*wire.ConnErrorMsg); ok {
		if e.Code == "auth_failed" {
			return ErrFailed
		}
		return &wire.ConnError{Code: e.Code, Msg: e.Message}
	}
	return fmt.Errorf("authn: unexpected %T during login", m)
}
