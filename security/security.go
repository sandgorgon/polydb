// Package security holds PolyDB's users, roles, grants and keys (the
// system store) and decides what each user may do (spec/rhizome.md §11,
// spec/protocol.md §3).
//
// Permissions are granted to roles. Every user is also a role named after
// itself, so permissions can be granted to a user directly, and roles can
// be granted to users and to other roles. The built-in user admin has
// admin on the server and can't be dropped.
package security

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"

	"github.com/sandgorgon/polydb/value"
)

// Perm is a permission (spec/rhizome.md §11).
type Perm uint8

const (
	Read   Perm = iota + 1 // queries, describe, history
	Write                  // create, update, delete entities; queue operations
	Relate                 // create and remove relations
	Define                 // define, alter and drop definitions
	Admin                  // everything, including users, roles, grants and databases
)

var permNames = [...]string{Read: "read", Write: "write", Relate: "relate", Define: "define", Admin: "admin"}

func (p Perm) String() string {
	if int(p) < len(permNames) && permNames[p] != "" {
		return permNames[p]
	}
	return fmt.Sprintf("perm(%d)", p)
}

// ParsePerm parses a permission name.
func ParsePerm(s string) (Perm, error) {
	for p, n := range permNames {
		if n == s && n != "" {
			return Perm(p), nil
		}
	}
	return 0, fmt.Errorf("unknown permission %s", s)
}

// Level is where a grant applies.
type Level uint8

const (
	ServerLevel Level = iota + 1
	DatabaseLevel
	KindLevel
)

// Scope is what a grant covers: the whole server, one database, or one
// kind in a database. Kinds are identified by ID, so a grant survives the
// kind being renamed.
type Scope struct {
	Level    Level
	Database string
	Kind     value.ID
}

func (s Scope) covers(db string, kind value.ID) bool {
	switch s.Level {
	case ServerLevel:
		return true
	case DatabaseLevel:
		return s.Database == db
	case KindLevel:
		return s.Database == db && !kind.IsZero() && s.Kind == kind
	}
	return false
}

// Grant gives a role a permission on a scope.
type Grant struct {
	Role  string
	Perm  Perm
	Scope Scope
}

// Key is a 9auth identity registered to a user.
type Key struct {
	Fingerprint string // 64 hex digits
	Label       string
}

// User is a role that can log in.
type User struct {
	Name  string
	Creds *Credentials // nil: no password; log in with a key or peer credentials
	Keys  []Key
}

// Error is a security error with a Rhizome error code.
type Error struct {
	Code string // not_found, constraint, permission
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errorf(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf returns a security error's code, or "" for other errors.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// AdminUser is the built-in administrator.
const AdminUser = "admin"

// DefaultIterations is the PBKDF2 iteration count for new passwords
// (spec/protocol.md §3.2).
const DefaultIterations = 100_000

// MinIterations is the smallest iteration count allowed.
const MinIterations = 4096

// Store is the system store. It is safe for concurrent use.
type Store struct {
	mu         sync.RWMutex
	users      map[string]*User
	roles      map[string]bool            // roles that aren't users
	memberOf   map[string]map[string]bool // role → roles granted to it
	grants     []Grant
	keys       map[string]string // fingerprint → user
	iterations int
	save       func([]byte) error // see PersistTo
}

// New returns a store holding only the built-in admin user, with
// passwords derived using iterations PBKDF2 rounds (0 for the default).
func New(iterations int) *Store {
	if iterations == 0 {
		iterations = DefaultIterations
	}
	s := &Store{
		users:      map[string]*User{AdminUser: {Name: AdminUser}},
		roles:      map[string]bool{},
		memberOf:   map[string]map[string]bool{},
		keys:       map[string]string{},
		iterations: max(iterations, MinIterations),
	}
	s.grants = []Grant{{Role: AdminUser, Perm: Admin, Scope: Scope{Level: ServerLevel}}}
	return s
}

func (s *Store) isRole(name string) bool {
	_, user := s.users[name]
	return user || s.roles[name]
}

func (s *Store) newName(what, name string) error {
	if err := value.ValidateName(what, name); err != nil {
		return errorf("constraint", "%v", err)
	}
	if s.isRole(name) {
		return errorf("constraint", "a user or role named %s already exists", name)
	}
	return nil
}

// CreateUser adds a user, with a password unless password is "".
func (s *Store) CreateUser(name, password string) error {
	var creds *Credentials
	if password != "" {
		c, err := NewCredentials(password, s.iterationCount())
		if err != nil {
			return err
		}
		creds = &c
	}
	return s.edit(func() error {
		if err := s.newName("user", name); err != nil {
			return err
		}
		s.users[name] = &User{Name: name, Creds: creds}
		return nil
	})
}

func (s *Store) iterationCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.iterations
}

// CreateRole adds a role.
func (s *Store) CreateRole(name string) error {
	return s.edit(func() error {
		if err := s.newName("role", name); err != nil {
			return err
		}
		s.roles[name] = true
		return nil
	})
}

// SetPassword sets or replaces a user's password.
func (s *Store) SetPassword(name, password string) error {
	c, err := NewCredentials(password, s.iterationCount())
	if err != nil {
		return err
	}
	return s.edit(func() error {
		u, ok := s.users[name]
		if !ok {
			return errorf("not_found", "no user %s", name)
		}
		cp := *u
		cp.Creds = &c
		s.users[name] = &cp
		return nil
	})
}

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AddKey registers a 9auth key fingerprint to a user. A fingerprint
// belongs to at most one user.
func (s *Store) AddKey(name, fingerprint, label string) error {
	if !fingerprintRE.MatchString(fingerprint) {
		return errorf("constraint", "a key fingerprint is 64 lowercase hex digits (polydb whoami --fingerprint prints yours)")
	}
	return s.edit(func() error {
		u, ok := s.users[name]
		if !ok {
			return errorf("not_found", "no user %s", name)
		}
		if owner, taken := s.keys[fingerprint]; taken {
			if owner == name {
				return errorf("constraint", "the key is already registered to %s", name)
			}
			return errorf("constraint", "the key is registered to another user")
		}
		cp := *u
		cp.Keys = append(slices.Clone(u.Keys), Key{fingerprint, label})
		s.users[name] = &cp
		s.keys[fingerprint] = name
		return nil
	})
}

// DropKey removes a key from a user.
func (s *Store) DropKey(name, fingerprint string) error {
	return s.edit(func() error {
		u, ok := s.users[name]
		if !ok {
			return errorf("not_found", "no user %s", name)
		}
		i := slices.IndexFunc(u.Keys, func(k Key) bool { return k.Fingerprint == fingerprint })
		if i < 0 {
			return errorf("not_found", "%s has no key %s", name, fingerprint)
		}
		cp := *u
		cp.Keys = slices.Delete(slices.Clone(u.Keys), i, i+1)
		s.users[name] = &cp
		delete(s.keys, fingerprint)
		return nil
	})
}

// Drop removes a user or role, with its grants and memberships.
func (s *Store) Drop(what, name string) error {
	return s.edit(func() error {
		if name == AdminUser {
			return errorf("constraint", "the built-in admin user can't be dropped")
		}
		switch what {
		case "user":
			u, ok := s.users[name]
			if !ok {
				return errorf("not_found", "no user %s", name)
			}
			for _, k := range u.Keys {
				delete(s.keys, k.Fingerprint)
			}
			delete(s.users, name)
		default:
			if !s.roles[name] {
				if _, isUser := s.users[name]; isUser {
					return errorf("constraint", "%s is a user; use drop user", name)
				}
				return errorf("not_found", "no role %s", name)
			}
			delete(s.roles, name)
		}
		delete(s.memberOf, name)
		for _, m := range s.memberOf {
			delete(m, name)
		}
		s.grants = slices.DeleteFunc(s.grants, func(g Grant) bool { return g.Role == name })
		return nil
	})
}

// GrantPerm gives a role a permission. Granting what is already granted
// is not an error.
func (s *Store) GrantPerm(role string, p Perm, sc Scope) error {
	return s.edit(func() error {
		if !s.isRole(role) {
			return errorf("not_found", "no user or role %s", role)
		}
		g := Grant{role, p, sc}
		if !slices.Contains(s.grants, g) {
			s.grants = append(s.grants, g)
		}
		return nil
	})
}

// RevokePerm takes a permission back.
func (s *Store) RevokePerm(role string, p Perm, sc Scope) error {
	return s.edit(func() error {
		if !s.isRole(role) {
			return errorf("not_found", "no user or role %s", role)
		}
		if role == AdminUser && p == Admin && sc.Level == ServerLevel {
			return errorf("constraint", "the built-in admin user keeps admin on the server")
		}
		n := len(s.grants)
		s.grants = slices.DeleteFunc(s.grants, func(g Grant) bool { return g == Grant{role, p, sc} })
		if len(s.grants) == n {
			return errorf("not_found", "%s doesn't have that permission", role)
		}
		return nil
	})
}

// GrantRole makes who a member of role.
func (s *Store) GrantRole(role, who string) error {
	return s.edit(func() error {
		if !s.roles[role] {
			if _, isUser := s.users[role]; isUser {
				return errorf("constraint", "%s is a user; only roles can be granted", role)
			}
			return errorf("not_found", "no role %s", role)
		}
		if !s.isRole(who) {
			return errorf("not_found", "no user or role %s", who)
		}
		if role == who || s.closure(role)[who] {
			return errorf("constraint", "granting %s to %s would make a cycle", role, who)
		}
		if s.memberOf[who] == nil {
			s.memberOf[who] = map[string]bool{}
		}
		s.memberOf[who][role] = true
		return nil
	})
}

// RevokeRole removes who from role.
func (s *Store) RevokeRole(role, who string) error {
	return s.edit(func() error {
		if !s.memberOf[who][role] {
			return errorf("not_found", "%s isn't a member of %s", who, role)
		}
		delete(s.memberOf[who], role)
		return nil
	})
}

// closure returns name and every role it belongs to, directly or not.
func (s *Store) closure(name string) map[string]bool {
	out := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		for m := range s.memberOf[r] {
			if !out[m] {
				out[m] = true
				queue = append(queue, m)
			}
		}
	}
	return out
}

// Allowed reports whether user has permission p in database db, on kind
// (zero for database-level actions and kindless entities). Admin covers
// every permission in its scope.
func (s *Store) Allowed(user string, p Perm, db string, kind value.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.users[user]; !ok {
		return false
	}
	roles := s.closure(user)
	for _, g := range s.grants {
		if roles[g.Role] && (g.Perm == p || g.Perm == Admin) && g.Scope.covers(db, kind) {
			return true
		}
	}
	return false
}

// CanSee reports whether user has any permission in database db, which
// is what using it requires.
func (s *Store) CanSee(user, db string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.users[user]; !ok {
		return false
	}
	roles := s.closure(user)
	for _, g := range s.grants {
		if roles[g.Role] && (g.Scope.Level == ServerLevel || g.Scope.Database == db) {
			return true
		}
	}
	return false
}

// DropDatabase removes the grants on a database that no longer exists.
func (s *Store) DropDatabase(db string) error {
	return s.edit(func() error {
		s.grants = slices.DeleteFunc(s.grants, func(g Grant) bool { return g.Scope.Level != ServerLevel && g.Scope.Database == db })
		return nil
	})
}

// DropKind removes the grants on a kind that no longer exists.
func (s *Store) DropKind(db string, kind value.ID) error {
	return s.edit(func() error {
		s.grants = slices.DeleteFunc(s.grants, func(g Grant) bool {
			return g.Scope.Level == KindLevel && g.Scope.Database == db && g.Scope.Kind == kind
		})
		return nil
	})
}

// User returns a copy of a user.
func (s *Store) User(name string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[name]
	if !ok {
		return User{}, false
	}
	return *u, true
}

// UserByKey returns the user a key fingerprint is registered to.
func (s *Store) UserByKey(fingerprint string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.keys[fingerprint]
	return u, ok
}

// Users returns the users sorted by name.
func (s *Store) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []User
	for _, n := range slices.Sorted(maps.Keys(s.users)) {
		out = append(out, *s.users[n])
	}
	return out
}

// Roles returns the roles that aren't users, sorted.
func (s *Store) Roles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.roles))
}

// MemberOf returns the roles granted directly to a user or role, sorted.
func (s *Store) MemberOf(name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.memberOf[name]))
}

// Members returns the users and roles granted a role directly, sorted.
func (s *Store) Members(role string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for who, m := range s.memberOf {
		if m[role] {
			out = append(out, who)
		}
	}
	slices.Sort(out)
	return out
}

// Grants returns the grants to a role ("" for all), in the order made.
func (s *Store) Grants(role string) []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Grant
	for _, g := range s.grants {
		if role == "" || g.Role == role {
			out = append(out, g)
		}
	}
	return out
}

// IsRole reports whether name is a user or a role.
func (s *Store) IsRole(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isRole(name)
}

// RolesOf returns every role a user or role belongs to, directly or
// through other roles, sorted, not including itself.
func (s *Store) RolesOf(name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.closure(name)
	delete(c, name)
	return slices.Sorted(maps.Keys(c))
}
