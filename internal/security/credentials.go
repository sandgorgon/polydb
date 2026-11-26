package security

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"

	"golang.org/x/text/secure/precis"
)

// Credentials are a user's SCRAM-SHA-256 verifiers (RFC 5802, RFC 7677).
// The password itself is never stored (spec/protocol.md §3.2).
type Credentials struct {
	Salt       []byte
	Iterations int
	StoredKey  []byte // H(ClientKey)
	ServerKey  []byte
}

// NormalizePassword prepares a password for hashing with the PRECIS
// OpaqueString profile (RFC 8265).
func NormalizePassword(password string) (string, error) {
	p, err := precis.OpaqueString.String(password)
	if err != nil {
		return "", errorf("constraint", "the password can't be used: %v", err)
	}
	return p, nil
}

// SaltedPassword computes Hi(Normalize(password), salt, i) from RFC 5802.
func SaltedPassword(password string, salt []byte, iterations int) ([]byte, error) {
	p, err := NormalizePassword(password)
	if err != nil {
		return nil, err
	}
	return pbkdf2.Key(sha256.New, p, salt, iterations, sha256.Size)
}

// HMAC returns HMAC-SHA-256(key, msg).
func HMAC(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// NewCredentials derives credentials for a password with a fresh random
// salt.
func NewCredentials(password string, iterations int) (Credentials, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	return DeriveCredentials(password, salt, iterations)
}

// DeriveCredentials derives credentials for a password and salt.
func DeriveCredentials(password string, salt []byte, iterations int) (Credentials, error) {
	if password == "" {
		return Credentials{}, errorf("constraint", "the password is empty")
	}
	salted, err := SaltedPassword(password, salt, iterations)
	if err != nil {
		return Credentials{}, err
	}
	clientKey := HMAC(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	return Credentials{
		Salt:       salt,
		Iterations: iterations,
		StoredKey:  stored[:],
		ServerKey:  HMAC(salted, []byte("Server Key")),
	}, nil
}

// Verify reports whether password matches the credentials. Logins use the
// SCRAM exchange instead; this is for tools and tests.
func (c *Credentials) Verify(password string) bool {
	d, err := DeriveCredentials(password, c.Salt, c.Iterations)
	return err == nil && subtle.ConstantTimeCompare(d.StoredKey, c.StoredKey) == 1
}
