package engine

import (
	"fmt"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/value"
)

// DatabaseMeta is a database's metadata (spec/rhizome.md §4, Database
// metadata). It is stored in the database itself, so backups carry it.
type DatabaseMeta struct {
	// Created and Owner are set when the database is created; Created is
	// null and Owner "" for a database made before metadata existed.
	Created value.Value
	Owner   string
	// Doc, Labels and Props are set by admins of the database.
	Doc    string
	Labels value.Set
	Props  value.Map
}

// dbMetaKey holds a database's metadata, as a map with text keys.
var dbMetaKey = keys.Meta("database")

// DatabaseName returns the name of the transaction's database.
func (tx *Tx) DatabaseName() string { return tx.db.Name() }

// DatabaseMeta reads the database's metadata.
func (tx *Tx) DatabaseMeta() (DatabaseMeta, error) {
	m := DatabaseMeta{Created: value.Null}
	m.Labels, _ = value.NewSet()
	m.Props, _ = value.MapOfText(nil)
	b, ok, err := tx.get(dbMetaKey)
	if err != nil || !ok {
		return m, err
	}
	v, err := value.Decode(b)
	if err != nil {
		return m, wrap(CodeInternal, fmt.Errorf("database metadata: %w", err))
	}
	rec, ok := v.(value.Map)
	if !ok {
		return m, errorf(CodeInternal, "database metadata is a %s, not a map", v.Code())
	}
	get := func(k string) value.Value {
		x, _ := rec.Get(value.MustText(k))
		return x
	}
	if t, ok := get("created").(value.Time); ok {
		m.Created = t
	}
	if t, ok := get("owner").(value.Text); ok {
		m.Owner = t.Go()
	}
	if t, ok := get("doc").(value.Text); ok {
		m.Doc = t.Go()
	}
	if s, ok := get("labels").(value.Set); ok {
		m.Labels = s
	}
	if p, ok := get("props").(value.Map); ok {
		m.Props = p
	}
	return m, nil
}

// SetDatabaseMeta replaces the database's metadata. Labels must be a set
// of text, and Props a map with text keys.
func (tx *Tx) SetDatabaseMeta(m DatabaseMeta) error {
	if tx.ReadOnly() {
		return errorf(CodeConstraint, "read-only transaction")
	}
	for _, l := range m.Labels.Values() {
		if _, ok := l.(value.Text); !ok {
			return errorf(CodeType, "database labels are text, not %s", l.Code())
		}
	}
	for k := range m.Props.All() {
		if _, ok := k.(value.Text); !ok {
			return errorf(CodeType, "database props have text keys, not %s", k.Code())
		}
	}
	rec := map[string]value.Value{"labels": m.Labels, "props": m.Props}
	if m.Created != nil && !value.IsNull(m.Created) {
		rec["created"] = m.Created
	}
	if m.Owner != "" {
		rec["owner"] = value.MustText(m.Owner)
	}
	if m.Doc != "" {
		d, err := value.NewText(m.Doc)
		if err != nil {
			return wrap(CodeType, err)
		}
		rec["doc"] = d
	}
	v, err := value.MapOfText(rec)
	if err != nil {
		return wrap(CodeInternal, err)
	}
	tx.t.Set(dbMetaKey, value.Encode(v))
	return nil
}

// CreateDatabaseAs is CreateDatabase, recording owner and the time as the
// new database's metadata.
func (e *Engine) CreateDatabaseAs(name, owner string) (*DB, error) {
	db, err := e.CreateDatabase(name)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *Tx) error {
		created, err := value.NewTime(tx.Now())
		if err != nil {
			return wrap(CodeInternal, err)
		}
		m, err := tx.DatabaseMeta()
		if err != nil {
			return err
		}
		m.Created, m.Owner = created, owner
		return tx.SetDatabaseMeta(m)
	})
	return db, err
}
