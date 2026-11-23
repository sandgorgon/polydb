// Package authn implements PolyDB's login mechanisms (spec/protocol.md
// §3): SCRAM-SHA-256 with and without TLS channel binding, KEY (a 9auth
// identity presented during the TLS handshake), PEER (Unix socket peer
// credentials), and the slowdown of repeated failed logins.
package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sandgorgon/polydb/security"
)

// Mechanism names.
const (
	ScramPlus = "SCRAM-SHA-256-PLUS"
	Scram     = "SCRAM-SHA-256"
	Key       = "KEY"
	Peer      = "PEER"
)

// ErrFailed is the only error a failed login reports to the client: it
// never says whether the user exists (spec/protocol.md §3.3).
var ErrFailed = errors.New("auth_failed")

// cbName is the channel binding type (RFC 9266).
const cbName = "tls-exporter"

// ChannelBinding returns the tls-exporter channel binding data of a TLS
// connection (RFC 9266), or nil if there is none.
func ChannelBinding(cs *tls.ConnectionState) []byte {
	if cs == nil {
		return nil
	}
	b, err := cs.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil
	}
	return b
}

// newNonce makes nonces; tests replace it to reproduce RFC examples.
var newNonce = nonce

func nonce() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// saslName escapes a user name for a SCRAM message (RFC 5802 §5.1).
func saslName(s string) string {
	return strings.NewReplacer("=", "=3D", ",", "=2C").Replace(s)
}

func unSaslName(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == ',':
			return "", errors.New("unescaped comma in user name")
		case s[i] != '=':
			b.WriteByte(s[i])
		case strings.HasPrefix(s[i:], "=2C"):
			b.WriteByte(',')
			i += 2
		case strings.HasPrefix(s[i:], "=3D"):
			b.WriteByte('=')
			i += 2
		default:
			return "", errors.New("bad escape in user name")
		}
	}
	return b.String(), nil
}

// attrs splits a SCRAM message into its attributes, which must have the
// given names in order (extensions are refused).
func attrs(msg string, names ...string) ([]string, error) {
	parts := strings.Split(msg, ",")
	if len(parts) != len(names) {
		return nil, fmt.Errorf("want %d attributes, got %d", len(names), len(parts))
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k != names[i] {
			return nil, fmt.Errorf("want attribute %s", names[i])
		}
		out[i] = v
	}
	return out, nil
}

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// ScramClient runs the client side of SCRAM-SHA-256.
type ScramClient struct {
	user, password string
	cb             []byte // channel binding data; nil without -PLUS
	serverPlus     bool   // the server offered -PLUS (but we can't use it)
	nonce          string
	gs2            string
	firstBare      string
	authMessage    string
	serverKey      []byte
}

// NewScramClient starts a login. cb is the channel binding data for
// SCRAM-SHA-256-PLUS (nil for plain SCRAM-SHA-256); serverPlus says
// whether the server offered -PLUS, which a client without channel
// binding must report so a downgrade can be detected.
func NewScramClient(user, password string, cb []byte, serverPlus bool) *ScramClient {
	return &ScramClient{user: user, password: password, cb: cb, serverPlus: serverPlus, nonce: newNonce()}
}

// Mechanism returns the mechanism name the client uses.
func (c *ScramClient) Mechanism() string {
	if c.cb != nil {
		return ScramPlus
	}
	return Scram
}

// First returns the client-first message.
func (c *ScramClient) First() []byte {
	switch {
	case c.cb != nil:
		c.gs2 = "p=" + cbName + ",,"
	case c.serverPlus:
		c.gs2 = "y,,"
	default:
		c.gs2 = "n,,"
	}
	c.firstBare = "n=" + saslName(c.user) + ",r=" + c.nonce
	return []byte(c.gs2 + c.firstBare)
}

// Final answers the server-first message with the client-final message.
func (c *ScramClient) Final(serverFirst []byte) ([]byte, error) {
	a, err := attrs(string(serverFirst), "r", "s", "i")
	if err != nil {
		return nil, fmt.Errorf("scram: server-first: %w", err)
	}
	if !strings.HasPrefix(a[0], c.nonce) || len(a[0]) == len(c.nonce) {
		return nil, errors.New("scram: the server's nonce doesn't extend ours")
	}
	salt, err := base64.StdEncoding.DecodeString(a[1])
	if err != nil {
		return nil, errors.New("scram: bad salt")
	}
	iter, err := strconv.Atoi(a[2])
	if err != nil || iter < security.MinIterations || iter > 10_000_000 {
		return nil, fmt.Errorf("scram: refusing %s iterations", a[2])
	}
	salted, err := security.SaltedPassword(c.password, salt, iter)
	if err != nil {
		return nil, err
	}
	clientKey := security.HMAC(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	c.serverKey = security.HMAC(salted, []byte("Server Key"))
	withoutProof := "c=" + base64.StdEncoding.EncodeToString(append([]byte(c.gs2), c.cb...)) + ",r=" + a[0]
	c.authMessage = c.firstBare + "," + string(serverFirst) + "," + withoutProof
	proof := xor(clientKey, security.HMAC(stored[:], []byte(c.authMessage)))
	return []byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)), nil
}

// Verify checks the server-final message. A server that can't prove it
// knows the credentials is an impostor (spec/protocol.md §3.2).
func (c *ScramClient) Verify(serverFinal []byte) error {
	a, err := attrs(string(serverFinal), "v")
	if err != nil {
		return errors.New("scram: the server's final message is malformed")
	}
	sig, err := base64.StdEncoding.DecodeString(a[0])
	want := security.HMAC(c.serverKey, []byte(c.authMessage))
	if err != nil || !hmac.Equal(sig, want) {
		return errors.New("scram: the server couldn't prove it knows the password; it may be an impostor")
	}
	return nil
}

// ScramServer runs the server side of SCRAM-SHA-256.
type ScramServer struct {
	lookup      func(user string) *security.Credentials
	fake        func(user string) security.Credentials
	cb          []byte // channel binding data of this connection; nil without TLS
	plus        bool   // the client chose SCRAM-SHA-256-PLUS
	offeredPlus bool   // this connection offers -PLUS

	user        string
	creds       security.Credentials
	known       bool
	gs2         string
	nonce       string
	authMessage string
}

// NewScramServer starts the server side for the mechanism the client
// chose. lookup returns a user's credentials (nil if none); fake returns
// believable credentials for users that don't exist, so the exchange
// looks the same either way. cb is the connection's channel binding data
// and offeredPlus whether -PLUS was offered.
func NewScramServer(mechanism string, lookup func(string) *security.Credentials, fake func(string) security.Credentials,
	cb []byte, offeredPlus bool) *ScramServer {
	return &ScramServer{lookup: lookup, fake: fake, cb: cb, plus: mechanism == ScramPlus, offeredPlus: offeredPlus}
}

// User returns the user named in the client-first message.
func (s *ScramServer) User() string { return s.user }

// Start reads the client-first message and returns the server-first
// message.
func (s *ScramServer) Start(clientFirst []byte) ([]byte, error) {
	msg := string(clientFirst)
	var bare string
	switch {
	case strings.HasPrefix(msg, "p="+cbName+",,"):
		if !s.plus || s.cb == nil {
			return nil, ErrFailed
		}
		s.gs2, bare = "p="+cbName+",,", msg[len("p="+cbName+",,"):]
	case strings.HasPrefix(msg, "y,,"):
		// The client could bind but thought we couldn't: a downgrade.
		if s.plus || s.offeredPlus {
			return nil, ErrFailed
		}
		s.gs2, bare = "y,,", msg[3:]
	case strings.HasPrefix(msg, "n,,"):
		if s.plus {
			return nil, ErrFailed
		}
		s.gs2, bare = "n,,", msg[3:]
	default:
		return nil, ErrFailed
	}
	a, err := attrs(bare, "n", "r")
	if err != nil || a[1] == "" {
		return nil, ErrFailed
	}
	if s.user, err = unSaslName(a[0]); err != nil || s.user == "" {
		return nil, ErrFailed
	}
	if c := s.lookup(s.user); c != nil {
		s.creds, s.known = *c, true
	} else {
		s.creds = s.fake(s.user)
	}
	s.nonce = a[1] + newNonce()
	first := "r=" + s.nonce + ",s=" + base64.StdEncoding.EncodeToString(s.creds.Salt) + ",i=" + strconv.Itoa(s.creds.Iterations)
	s.authMessage = bare + "," + first
	return []byte(first), nil
}

// Finish checks the client-final message and returns the server-final
// message.
func (s *ScramServer) Finish(clientFinal []byte) ([]byte, error) {
	a, err := attrs(string(clientFinal), "c", "r", "p")
	if err != nil {
		return nil, ErrFailed
	}
	wantCB := base64.StdEncoding.EncodeToString(append([]byte(s.gs2), s.cbData()...))
	if subtle.ConstantTimeCompare([]byte(a[0]), []byte(wantCB)) != 1 || a[1] != s.nonce {
		return nil, ErrFailed
	}
	proof, err := base64.StdEncoding.DecodeString(a[2])
	if err != nil || len(proof) != sha256.Size {
		return nil, ErrFailed
	}
	s.authMessage += "," + string(clientFinal[:strings.LastIndex(string(clientFinal), ",p=")])
	clientKey := xor(proof, security.HMAC(s.creds.StoredKey, []byte(s.authMessage)))
	sum := sha256.Sum256(clientKey)
	if subtle.ConstantTimeCompare(sum[:], s.creds.StoredKey) != 1 || !s.known {
		return nil, ErrFailed
	}
	return []byte("v=" + base64.StdEncoding.EncodeToString(security.HMAC(s.creds.ServerKey, []byte(s.authMessage)))), nil
}

func (s *ScramServer) cbData() []byte {
	if s.gs2 == "p="+cbName+",," {
		return s.cb
	}
	return nil
}
