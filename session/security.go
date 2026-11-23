package session

import (
	"slices"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// access implements query.Access for the session's user in its database.
type access struct {
	sec  *security.Store
	user string
	db   string
}

func (a access) Can(p security.Perm, kind *model.Kind) bool {
	var id value.ID
	if kind != nil {
		id = kind.ID
	}
	return a.sec.Allowed(a.user, p, a.db, id)
}

func (s *Session) access() access {
	return access{sec: s.eng.Security(), user: s.user, db: s.Database()}
}

func (s *Session) isAdmin() bool {
	return s.eng.Security().Allowed(s.user, security.Admin, "", value.ID{})
}

func denied(st lang.Node, format string, args ...any) *lang.Error {
	return sessionErr(st, "permission", format, args...)
}

// needAdmin requires admin on the server.
func (s *Session) needAdmin(st lang.Node, what string) error {
	if s.isAdmin() {
		return nil
	}
	return denied(st, "%s needs admin permission on the server", what)
}

// need requires p on a kind of the current database (nil: database
// level).
func (s *Session) need(st lang.Node, p security.Perm, kind *model.Kind) error {
	if s.access().Can(p, kind) {
		return nil
	}
	if kind == nil {
		return denied(st, "you don't have %s permission on database %s", p, s.Database())
	}
	return denied(st, "you don't have %s permission on kind %s", p, kind.Name)
}

// authorize checks the permissions a statement needs before it runs.
// Data statements and queries are checked by the executor as they touch
// entities; definitions and introspection are checked here.
func (s *Session) authorize(qc *query.Context, st lang.Stmt) error {
	cat := qc.Tx.Catalog()
	kind := func(name string) *model.Kind {
		k, _ := cat.Kind(name)
		return k
	}
	switch st := st.(type) {
	case *lang.DefineKind, *lang.DefineRelation, *lang.AlterRelation, *lang.DefineFormat:
		return s.need(st, security.Define, nil)
	case *lang.AlterKind:
		return s.need(st, security.Define, kind(st.Name))
	case *lang.DefineIndex:
		return s.need(st, security.Define, kind(st.Kind))
	case *lang.Drop:
		switch st.What {
		case "kind":
			return s.need(st, security.Define, kind(st.Name))
		case "index":
			var k *model.Kind
			if ix, ok := cat.Index(st.Name); ok {
				k, _ = cat.KindByID(ix.Kind)
			}
			return s.need(st, security.Define, k)
		}
		return s.need(st, security.Define, nil)
	case *lang.Describe:
		if st.What == "kind" {
			return s.need(st, security.Read, kind(st.Name))
		}
		if st.Target == nil && !s.eng.Security().CanSee(s.user, s.Database()) {
			return denied(st, "you don't have permission on database %s", s.Database())
		}
	case *lang.Explain:
		if pl, ok := st.Stmt.(*lang.Pipeline); ok && !st.Analyze {
			for _, kv := range pl.Source.Vars {
				if k := kind(kv.Kind); k != nil {
					if err := s.need(st, security.Read, k); err != nil {
						return err
					}
				}
			}
		}
		return s.authorize(qc, st.Stmt)
	case *lang.ShowStmt:
		if !s.eng.Security().CanSee(s.user, s.Database()) {
			return denied(st, "you don't have permission on database %s", s.Database())
		}
	}
	return nil
}

// constant checks and evaluates an expression that uses no rows, such as
// a password.
func (s *Session) constant(e lang.Expr, params map[string]value.Value) (value.Value, error) {
	info, _, err := lang.CheckExpr(&lang.Env{Params: params}, e, nil)
	if err != nil {
		return nil, err
	}
	ev := &lang.Evaluator{Info: info, Params: params, Now: time.Now()}
	return ev.Eval(e, nil)
}

func (s *Session) password(e lang.Expr, params map[string]value.Value) (string, error) {
	v, err := s.constant(e, params)
	if err != nil {
		return "", err
	}
	t, ok := v.(value.Text)
	if !ok {
		return "", sessionErr(e, "type", "a password is text, not %s", v.Code())
	}
	return t.Go(), nil
}

func (s *Session) secErr(st lang.Node, err error) error {
	if err == nil {
		return nil
	}
	if code := security.CodeOf(err); code != "" {
		msg := err.Error()[len(code)+2:]
		return sessionErr(st, code, "%s", msg)
	}
	return s.fail(st, err)
}

// security runs users, roles, grants and keys statements. They change
// the system store at once and can't run inside a transaction.
func (s *Session) security(st lang.Stmt, params map[string]value.Value) (*query.Result, error) {
	if s.tx != nil {
		return nil, sessionErr(st, "constraint", "users, roles and grants can't change inside a transaction")
	}
	sec := s.eng.Security()
	var err error
	switch st := st.(type) {
	case *lang.CreateUser:
		if err := s.needAdmin(st, "create user"); err != nil {
			return nil, err
		}
		pw, err := s.password(st.Password, params)
		if err != nil {
			return nil, err
		}
		return &query.Result{}, s.secErr(st, sec.CreateUser(st.Name, pw))
	case *lang.CreateRole:
		if err := s.needAdmin(st, "create role"); err != nil {
			return nil, err
		}
		err = sec.CreateRole(st.Name)
	case *lang.Drop:
		if err := s.needAdmin(st, "drop "+st.What); err != nil {
			return nil, err
		}
		err = sec.Drop(st.What, st.Name)
	case *lang.AlterUser:
		if st.Name != s.user && !s.isAdmin() {
			return nil, denied(st, "only admins can change another user")
		}
		switch {
		case st.Password != nil:
			pw, perr := s.password(st.Password, params)
			if perr != nil {
				return nil, perr
			}
			err = sec.SetPassword(st.Name, pw)
		case st.AddKey != nil:
			label := ""
			if st.KeyLabel != nil {
				label = *st.KeyLabel
			}
			err = sec.AddKey(st.Name, strings.ToLower(*st.AddKey), label)
		case st.DropKey != nil:
			err = sec.DropKey(st.Name, strings.ToLower(*st.DropKey))
		}
	case *lang.Grant:
		err = s.grant(st)
	}
	if err != nil {
		return nil, s.secErr(st, err)
	}
	return &query.Result{}, nil
}

func (s *Session) grant(st *lang.Grant) error {
	sec := s.eng.Security()
	if st.Role != "" {
		if err := s.needAdmin(st, "granting roles"); err != nil {
			return err
		}
		if st.Revoke {
			return sec.RevokeRole(st.Role, st.Who)
		}
		return sec.GrantRole(st.Role, st.Who)
	}
	scope, err := s.scope(st)
	if err != nil {
		return err
	}
	// Admins of a database may grant on it; the server needs a server admin.
	if !sec.Allowed(s.user, security.Admin, scope.Database, value.ID{}) || scope.Level == security.ServerLevel && !s.isAdmin() {
		return denied(st, "granting permissions needs admin permission on what they cover")
	}
	for _, name := range st.Perms {
		p, err := security.ParsePerm(name)
		if err != nil {
			return sessionErr(st, "syntax", "%v", err)
		}
		if st.Revoke {
			err = sec.RevokePerm(st.Who, p, scope)
		} else {
			err = sec.GrantPerm(st.Who, p, scope)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// scope resolves a grant's scope. A kind is given as db.Kind, or Kind in
// the current database.
func (s *Session) scope(st *lang.Grant) (security.Scope, error) {
	switch st.Scope.What {
	case "server":
		return security.Scope{Level: security.ServerLevel}, nil
	case "database":
		if _, err := s.eng.Database(st.Scope.Database); err != nil {
			return security.Scope{}, s.fail(st, err)
		}
		return security.Scope{Level: security.DatabaseLevel, Database: st.Scope.Database}, nil
	}
	dbName := st.Scope.Database
	if dbName == "" {
		dbName = s.Database()
		if dbName == "" {
			return security.Scope{}, sessionErr(st, "not_found", "no database in use: write kind <database>.<Kind>")
		}
	}
	db, err := s.eng.Database(dbName)
	if err != nil {
		return security.Scope{}, s.fail(st, err)
	}
	k, ok := db.Catalog().Kind(st.Scope.Kind)
	if !ok {
		return security.Scope{}, sessionErr(st, "not_found", "no kind %s in database %s", st.Scope.Kind, dbName)
	}
	return security.Scope{Level: security.KindLevel, Database: dbName, Kind: k.ID}, nil
}

// scopeText describes a scope, naming kinds by their current name.
func (s *Session) scopeText(sc security.Scope) string {
	switch sc.Level {
	case security.ServerLevel:
		return "server"
	case security.DatabaseLevel:
		return "database " + sc.Database
	}
	name := "(dropped kind)"
	if db, err := s.eng.Database(sc.Database); err == nil {
		if k, ok := db.Catalog().KindByID(sc.Kind); ok {
			name = k.Name
		}
	}
	return "kind " + sc.Database + "." + name
}

// showSecurity runs show users, roles, grants and keys.
func (s *Session) showSecurity(st *lang.ShowStmt) (*query.Result, error) {
	sec := s.eng.Security()
	switch st.What {
	case "users":
		if err := s.needAdmin(st, "show users"); err != nil {
			return nil, err
		}
		r := table("name", "password", "keys", "roles")
		for _, u := range sec.Users() {
			add(r, u.Name, value.Bool(u.Creds != nil), len(u.Keys), strings.Join(sec.MemberOf(u.Name), ", "))
		}
		return r, nil
	case "roles":
		if err := s.needAdmin(st, "show roles"); err != nil {
			return nil, err
		}
		r := table("name", "members")
		for _, role := range sec.Roles() {
			add(r, role, strings.Join(sec.Members(role), ", "))
		}
		return r, nil
	case "grants":
		who := st.For
		if who == "" && !s.isAdmin() {
			who = s.user
		}
		if who != s.user && !s.isAdmin() {
			return nil, denied(st, "only admins can see another user's grants")
		}
		if who != "" && !sec.IsRole(who) {
			return nil, sessionErr(st, "not_found", "no user or role %s", who)
		}
		// A user's grants include those of the roles it belongs to; the
		// role column says which role holds each.
		r := table("role", "permission", "scope")
		roles := []string{""}
		if who != "" {
			roles = append([]string{who}, sec.RolesOf(who)...)
		}
		for _, role := range roles {
			for _, g := range sec.Grants(role) {
				add(r, g.Role, g.Perm.String(), s.scopeText(g.Scope))
			}
		}
		return r, nil
	case "keys":
		who := st.For
		if who == "" {
			who = s.user
		}
		if who != s.user && !s.isAdmin() {
			return nil, denied(st, "only admins can see another user's keys")
		}
		u, ok := sec.User(who)
		if !ok {
			return nil, sessionErr(st, "not_found", "no user %s", who)
		}
		r := table("fingerprint", "label")
		keys := slices.Clone(u.Keys)
		for _, k := range keys {
			add(r, k.Fingerprint, k.Label)
		}
		return r, nil
	}
	return nil, sessionErr(st, "unsupported", "show %s arrives with the server", st.What)
}
