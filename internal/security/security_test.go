package security

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/value"
)

// TestCredentialsRFC7677 checks key derivation against the SCRAM-SHA-256
// example in RFC 7677 §3.
func TestCredentialsRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	c, err := DeriveCredentials("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	authMessage := "n=user,r=rOprNGfwEbeRWgbNEkqO," +
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096," +
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	sig := base64.StdEncoding.EncodeToString(HMAC(c.ServerKey, []byte(authMessage)))
	if sig != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Errorf("server signature %s", sig)
	}
	if !c.Verify("pencil") || c.Verify("pencils") {
		t.Error("Verify")
	}
	// PRECIS OpaqueString maps non-ASCII spaces to ASCII space.
	n, err := NormalizePassword("a b")
	if err != nil || n != "a b" {
		t.Errorf("normalize: %q, %v", n, err)
	}
	if _, err := DeriveCredentials("", salt, 4096); err == nil {
		t.Error("empty password accepted")
	}
}

func TestPermissions(t *testing.T) {
	s := New(MinIterations)
	k1, k2 := value.ID{1}, value.ID{2}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateUser("ramon", "pw"))
	must(s.CreateUser("eve", ""))
	must(s.CreateRole("analyst"))
	must(s.CreateRole("staff"))
	must(s.GrantPerm("analyst", Read, Scope{Level: DatabaseLevel, Database: "sales"}))
	must(s.GrantPerm("analyst", Write, Scope{Level: KindLevel, Database: "sales", Kind: k1}))
	must(s.GrantRole("analyst", "staff"))
	must(s.GrantRole("staff", "ramon"))

	cases := []struct {
		user string
		p    Perm
		db   string
		kind value.ID
		want bool
	}{
		{"admin", Admin, "anything", value.ID{}, true},
		{"admin", Write, "sales", k1, true},
		{"ramon", Read, "sales", k1, true}, // database grant covers kinds
		{"ramon", Read, "sales", value.ID{}, true},
		{"ramon", Write, "sales", k1, true}, // through staff → analyst
		{"ramon", Write, "sales", k2, false},
		{"ramon", Write, "sales", value.ID{}, false}, // kindless needs a database grant
		{"ramon", Read, "hr", k1, false},
		{"ramon", Define, "sales", value.ID{}, false},
		{"eve", Read, "sales", value.ID{}, false},
		{"nobody", Read, "sales", value.ID{}, false},
	}
	for _, c := range cases {
		if got := s.Allowed(c.user, c.p, c.db, c.kind); got != c.want {
			t.Errorf("Allowed(%s, %s, %s, %v) = %v", c.user, c.p, c.db, c.kind, got)
		}
	}
	if !s.CanSee("ramon", "sales") || s.CanSee("ramon", "hr") || !s.CanSee("admin", "hr") {
		t.Error("CanSee")
	}
	// ramon writes kind k1 in sales through staff → analyst; eve has nothing.
	if !s.CanWriteIn("ramon", "sales") || s.CanWriteIn("ramon", "hr") ||
		!s.CanWriteIn("admin", "hr") || s.CanWriteIn("eve", "sales") || s.CanWriteIn("nobody", "sales") {
		t.Error("CanWriteIn")
	}

	// Database admin covers that database only.
	must(s.GrantPerm("eve", Admin, Scope{Level: DatabaseLevel, Database: "hr"}))
	if !s.Allowed("eve", Define, "hr", value.ID{}) || s.Allowed("eve", Read, "sales", value.ID{}) {
		t.Error("database admin")
	}

	// Revoking, and dropping a role, take permissions away.
	must(s.RevokeRole("staff", "ramon"))
	if s.Allowed("ramon", Read, "sales", value.ID{}) {
		t.Error("revoke role")
	}
	must(s.GrantRole("staff", "ramon"))
	must(s.Drop("role", "analyst"))
	if s.Allowed("ramon", Read, "sales", value.ID{}) || len(s.Grants("analyst")) != 0 {
		t.Error("drop role")
	}

	// Errors.
	for _, c := range []struct {
		err  error
		want string
	}{
		{s.CreateUser("ramon", "x"), "already exists"},
		{s.CreateRole("1bad"), "role name"},
		{s.GrantRole("staff", "staff"), "cycle"},
		{s.GrantRole("ramon", "staff"), "is a user"},
		{s.GrantPerm("ghost", Read, Scope{Level: ServerLevel}), "no user or role ghost"},
		{s.RevokePerm("admin", Admin, Scope{Level: ServerLevel}), "keeps admin"},
		{s.Drop("user", "admin"), "can't be dropped"},
		{s.Drop("role", "ramon"), "is a user"},
		{s.AddKey("ramon", "XYZ", ""), "64 lowercase hex"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("got %v, want %q", c.err, c.want)
		}
	}
	must(s.CreateRole("a"))
	must(s.CreateRole("b"))
	must(s.GrantRole("a", "b"))
	if err := s.GrantRole("b", "a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("indirect cycle: %v", err)
	}
}

func TestKeys(t *testing.T) {
	s := New(MinIterations)
	s.CreateUser("ramon", "")
	s.CreateUser("eve", "")
	fp := strings.Repeat("ab", 32)
	if err := s.AddKey("ramon", fp, "laptop"); err != nil {
		t.Fatal(err)
	}
	if u, ok := s.UserByKey(fp); !ok || u != "ramon" {
		t.Errorf("UserByKey: %s %v", u, ok)
	}
	if err := s.AddKey("eve", fp, ""); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("shared key: %v", err)
	}
	if err := s.DropKey("ramon", fp); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.UserByKey(fp); ok {
		t.Error("key still registered")
	}
	s.AddKey("ramon", fp, "")
	s.Drop("user", "ramon")
	if _, ok := s.UserByKey(fp); ok {
		t.Error("key outlived its user")
	}
	if err := s.SetPassword("eve", "secret"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.User("eve"); u.Creds == nil || !u.Creds.Verify("secret") {
		t.Error("SetPassword")
	}
}
