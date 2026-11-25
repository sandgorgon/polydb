package security

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// persistVersion is the format version of a saved store.
const persistVersion = 1

// saved is a store's saved form. Credentials are the SCRAM verifiers,
// never passwords.
type saved struct {
	Version  int                 `json:"version"`
	Users    []User              `json:"users"`
	Roles    []string            `json:"roles"`
	MemberOf map[string][]string `json:"member_of"`
	Grants   []Grant             `json:"grants"`
}

// PersistTo makes every later change call save with the whole store's
// saved form before the change takes effect; if save fails, the change
// is undone and its error returned. The engine saves to its system
// store.
func (s *Store) PersistTo(save func(data []byte) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.save = save
}

// Marshal returns the store's saved form.
func (s *Store) Marshal() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.marshal()
}

func (s *Store) marshal() []byte {
	v := saved{Version: persistVersion, MemberOf: map[string][]string{}, Grants: s.grants}
	for _, name := range slices.Sorted(maps.Keys(s.users)) {
		v.Users = append(v.Users, *s.users[name])
	}
	v.Roles = slices.Sorted(maps.Keys(s.roles))
	for who, roles := range s.memberOf {
		if len(roles) > 0 {
			v.MemberOf[who] = slices.Sorted(maps.Keys(roles))
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic("security: marshaling the store: " + err.Error()) // only plain data
	}
	return b
}

// Restore replaces the store's contents with a saved form.
func (s *Store) Restore(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restore(data)
}

func (s *Store) restore(data []byte) error {
	var v saved
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("security: reading the saved store: %w", err)
	}
	if v.Version != persistVersion {
		return fmt.Errorf("security: saved store format %d is not supported", v.Version)
	}
	users := map[string]*User{}
	keys := map[string]string{}
	for _, u := range v.Users {
		users[u.Name] = &u
		for _, k := range u.Keys {
			keys[k.Fingerprint] = u.Name
		}
	}
	if users[AdminUser] == nil {
		return fmt.Errorf("security: the saved store has no %s user", AdminUser)
	}
	roles := map[string]bool{}
	for _, r := range v.Roles {
		roles[r] = true
	}
	memberOf := map[string]map[string]bool{}
	for who, rs := range v.MemberOf {
		memberOf[who] = map[string]bool{}
		for _, r := range rs {
			memberOf[who][r] = true
		}
	}
	s.users, s.keys, s.roles, s.memberOf, s.grants = users, keys, roles, memberOf, v.Grants
	adminGrant := Grant{Role: AdminUser, Perm: Admin, Scope: Scope{Level: ServerLevel}}
	if !slices.Contains(s.grants, adminGrant) {
		s.grants = append(s.grants, adminGrant)
	}
	return nil
}

// edit runs a change under the write lock and saves the result. fn must
// check everything before it changes anything.
func (s *Store) edit(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.save == nil {
		return fn()
	}
	before := s.marshal()
	if err := fn(); err != nil {
		return err
	}
	if err := s.save(s.marshal()); err != nil {
		if rerr := s.restore(before); rerr != nil {
			panic("security: undoing a change: " + rerr.Error())
		}
		return errorf("internal", "saving the change failed: %v", err)
	}
	return nil
}
