package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/value"
)

// A security export holds the server's users, roles and grants:
//
//	{"polydb": "security", "format": 1, "created": …, "passwords": true}
//	{"user": "ann", "password": {"salt": …, "iterations": …,
//	  "stored_key": …, "server_key": …}, "keys": [{"fingerprint": …, "label": …}]}
//	{"role": "staff", "members": ["ann", …]}
//	{"grant": "read", "to": "staff", "scope": "kind", "database": "sales", "kind": "Order"}
//	{"end": true, "users": n, "roles": n, "grants": n}
//
// Passwords are their salted PBKDF2 credentials (never the passwords),
// which still let someone try to guess them offline: an export with them
// is a secret. Without passwords it can be shared. Grants name kinds by
// name, so they apply to a restored database or an import with new IDs.

// SecurityStats counts what a security export or import moved.
type SecurityStats struct {
	Users, Roles, Grants int
	Kept                 int // users that existed and were kept as they are
	Skipped              int // grants on kinds or databases that don't exist
	Bytes                int64
}

// ExportSecurity writes the engine's users, roles and grants to w,
// with the users' password credentials if passwords is set.
func ExportSecurity(ctx context.Context, eng *engine.Engine, w io.Writer, format Format, passwords bool) (*SecurityStats, error) {
	cw := &countWriter{w: w}
	rw, err := newRecordWriter(cw, format)
	if err != nil {
		return nil, err
	}
	sec := eng.Security()
	created, _ := value.NewTime(time.Now())
	if err := rw.write(rec(pair{"polydb", value.MustText("security")}, pair{"format", value.Int(exportFormat)},
		pair{"created", created}, pair{"passwords", value.Bool(passwords)})); err != nil {
		return nil, err
	}
	st := &SecurityStats{}
	for _, u := range sec.Users() {
		var pw value.Value
		if passwords && u.Creds != nil {
			c := u.Creds
			pw = rec(pair{"salt", value.BytesOf(c.Salt, "")}, pair{"iterations", value.Int(c.Iterations)},
				pair{"stored_key", value.BytesOf(c.StoredKey, "")}, pair{"server_key", value.BytesOf(c.ServerKey, "")})
		}
		var keys []value.Value
		for _, k := range u.Keys {
			var label value.Value
			if k.Label != "" {
				label = value.MustText(k.Label)
			}
			keys = append(keys, rec(pair{"fingerprint", value.MustText(k.Fingerprint)}, pair{"label", label}))
		}
		var kl value.Value
		if len(keys) > 0 {
			kl, _ = value.NewList(keys...)
		}
		if err := rw.write(rec(pair{"user", value.MustText(u.Name)}, pair{"password", pw}, pair{"keys", kl})); err != nil {
			return nil, err
		}
		st.Users++
	}
	for _, r := range sec.Roles() {
		var members []value.Value
		for _, m := range sec.Members(r) {
			members = append(members, value.MustText(m))
		}
		ml, _ := value.NewList(members...)
		if err := rw.write(rec(pair{"role", value.MustText(r)}, pair{"members", ml})); err != nil {
			return nil, err
		}
		st.Roles++
	}
	for _, g := range sec.Grants("") {
		ps := []pair{{"grant", value.MustText(g.Perm.String())}, {"to", value.MustText(g.Role)}}
		switch g.Scope.Level {
		case security.ServerLevel:
			ps = append(ps, pair{"scope", value.MustText("server")})
		case security.DatabaseLevel:
			ps = append(ps, pair{"scope", value.MustText("database")}, pair{"database", value.MustText(g.Scope.Database)})
		case security.KindLevel:
			db, err := eng.Database(g.Scope.Database)
			if err != nil {
				st.Skipped++
				continue
			}
			k, ok := db.Catalog().KindByID(g.Scope.Kind)
			if !ok {
				st.Skipped++
				continue
			}
			ps = append(ps, pair{"scope", value.MustText("kind")}, pair{"database", value.MustText(g.Scope.Database)},
				pair{"kind", value.MustText(k.Name)})
		}
		if err := rw.write(rec(ps...)); err != nil {
			return nil, err
		}
		st.Grants++
	}
	if err := rw.write(rec(pair{"end", value.Bool(true)}, pair{"users", value.Int(st.Users)},
		pair{"roles", value.Int(st.Roles)}, pair{"grants", value.Int(st.Grants)})); err != nil {
		return nil, err
	}
	if err := rw.flush(); err != nil {
		return nil, err
	}
	st.Bytes = cw.n
	return st, ctx.Err()
}

// ImportSecurity reads a security export into the engine: users and
// roles that don't exist are created, memberships and grants added. A
// user that exists keeps its password and keys unless replace is set.
// Grants on databases or kinds that don't exist are skipped and
// counted. The whole export is checked before anything changes.
func ImportSecurity(ctx context.Context, eng *engine.Engine, r io.Reader, replace bool) (*SecurityStats, error) {
	rr, err := newRecordReader(r)
	if err != nil {
		return nil, err
	}
	head, err := rr.next()
	if err != nil {
		return nil, fmt.Errorf("reading the header: %w", unexpected(err))
	}
	if what, _ := textField(head, "polydb"); what != "security" {
		return nil, errors.New("this isn't a security export")
	}
	if f, _ := intField(head, "format"); f != exportFormat {
		return nil, fmt.Errorf("an export in format %d, which this version doesn't read", f)
	}
	type user struct {
		name  string
		creds *security.Credentials
		keys  []security.Key
	}
	type grant struct {
		perm  security.Perm
		to    string
		scope security.Scope
		kind  string
	}
	var users []user
	var roles []string
	members := map[string][]string{}
	var grants []grant
	ended := false
	for !ended {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := rr.next()
		if err != nil {
			if err == io.EOF {
				return nil, errors.New("the export ends early (no end record)")
			}
			return nil, err
		}
		switch kindOf(m) {
		case "user":
			u := user{}
			u.name, _ = textField(m, "user")
			if pw, ok := field(m, "password").(value.Map); ok {
				c := &security.Credentials{}
				get := func(k string) []byte {
					b, _ := field(pw, k).(value.Bytes)
					raw, _, _ := value.Large(b)
					return raw
				}
				c.Salt, c.StoredKey, c.ServerKey = get("salt"), get("stored_key"), get("server_key")
				it, _ := intField(pw, "iterations")
				c.Iterations = int(it)
				u.creds = c
			}
			if kl, ok := field(m, "keys").(value.List); ok {
				for _, kv := range kl.Values() {
					km, ok := kv.(value.Map)
					if !ok {
						return nil, fmt.Errorf("user %s: a key isn't a map", u.name)
					}
					fp, _ := textField(km, "fingerprint")
					label, _ := textField(km, "label")
					u.keys = append(u.keys, security.Key{Fingerprint: fp, Label: label})
				}
			}
			users = append(users, u)
		case "role":
			name, _ := textField(m, "role")
			roles = append(roles, name)
			if ml, ok := field(m, "members").(value.List); ok {
				for _, v := range ml.Values() {
					if t, ok := v.(value.Text); ok {
						members[name] = append(members[name], t.Go())
					}
				}
			}
		case "grant":
			ps, _ := textField(m, "grant")
			p, err := security.ParsePerm(ps)
			if err != nil {
				return nil, err
			}
			g := grant{perm: p}
			g.to, _ = textField(m, "to")
			scope, _ := textField(m, "scope")
			g.scope.Database, _ = textField(m, "database")
			g.kind, _ = textField(m, "kind")
			switch scope {
			case "server":
				g.scope.Level = security.ServerLevel
			case "database":
				g.scope.Level = security.DatabaseLevel
			case "kind":
				g.scope.Level = security.KindLevel
			default:
				return nil, fmt.Errorf("a grant with scope %q", scope)
			}
			grants = append(grants, g)
		case "end":
			ended = true
		default:
			return nil, fmt.Errorf("unexpected %s record in a security export", kindOf(m))
		}
	}

	sec := eng.Security()
	st := &SecurityStats{}
	for _, u := range users {
		if err := value.ValidateName("user", u.name); err != nil {
			return nil, err
		}
	}
	for _, r := range roles {
		if err := value.ValidateName("role", r); err != nil {
			return nil, err
		}
		if _, isUser := sec.User(r); isUser {
			return nil, fmt.Errorf("role %s is a user here", r)
		}
	}
	for _, u := range users {
		if sec.IsRole(u.name) {
			if _, isUser := sec.User(u.name); !isUser {
				return nil, fmt.Errorf("user %s is a role here", u.name)
			}
		}
	}
	for _, u := range users {
		created, err := sec.ImportUser(u.name, u.creds, u.keys, replace)
		if err != nil {
			return st, err
		}
		if !created && !replace {
			st.Kept++
		}
		st.Users++
	}
	for _, r := range roles {
		if !sec.IsRole(r) {
			if err := sec.CreateRole(r); err != nil {
				return st, err
			}
		}
		st.Roles++
	}
	for _, r := range roles {
		for _, who := range members[r] {
			if slices.Contains(sec.MemberOf(who), r) {
				continue
			}
			if err := sec.GrantRole(r, who); err != nil {
				return st, fmt.Errorf("making %s a member of %s: %w", who, r, err)
			}
		}
	}
	for _, g := range grants {
		if g.scope.Level != security.ServerLevel {
			db, err := eng.Database(g.scope.Database)
			if err != nil {
				st.Skipped++
				continue
			}
			if g.scope.Level == security.KindLevel {
				k, ok := db.Catalog().Kind(g.kind)
				if !ok {
					st.Skipped++
					continue
				}
				g.scope.Kind = k.ID
			}
		}
		if err := sec.GrantPerm(g.to, g.perm, g.scope); err != nil {
			return st, err
		}
		st.Grants++
	}
	return st, nil
}
