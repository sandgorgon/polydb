package engine

import (
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Imports (package transfer) write entities and relations as an export
// recorded them: with their IDs, field IDs and metadata. A ref field's
// target may come later in the import, so ref targets aren't checked;
// everything else is, as for a create.

// ImportedEntity is an entity as an export recorded it.
type ImportedEntity struct {
	ID     value.ID
	Kind   string // "" for kindless
	Meta   model.Meta
	Fields []ImportedField
}

// ImportedField is one of its fields: declared by the kind when Def is
// nil, otherwise added by the entity.
type ImportedField struct {
	ID    value.ID
	Name  string
	Value value.Value
	Def   *model.FieldDef
}

// NewID returns a new ID, for imports that give entities new ones.
func (db *DB) NewID() value.ID { return db.eng.ids.New() }

// unusedID fails if id already names a node.
func (tx *Tx) unusedID(id value.ID, what string) error {
	if _, ok, err := tx.node(id, true); err != nil {
		return err
	} else if ok {
		return errorf(CodeConstraint, "%s %s already exists", what, id)
	}
	return nil
}

// ImportEntity writes an entity with the ID, metadata and field IDs
// given.
func (tx *Tx) ImportEntity(ie ImportedEntity) error {
	if err := tx.writable(); err != nil {
		return err
	}
	if err := tx.unusedID(ie.ID, "entity"); err != nil {
		return err
	}
	// imported: save keeps the version and times given.
	st := &entState{e: &model.Entity{ID: ie.ID, Meta: ie.Meta}, fields: map[string]*model.Field{}, isNew: true, imported: true}
	if ie.Kind != "" {
		k, ok := tx.Catalog().Kind(ie.Kind)
		if !ok {
			return tx.unknownKind(ie.Kind)
		}
		st.kind, st.e.Kind = k, k.ID
	}
	tx.importing = true
	defer func() { tx.importing = false }()
	seen := map[value.ID]bool{ie.ID: true}
	for _, f := range ie.Fields {
		if seen[f.ID] {
			return errorf(CodeConstraint, "entity %s: field ID %s used twice", ie.ID, f.ID)
		}
		seen[f.ID] = true
		if err := tx.unusedID(f.ID, "field"); err != nil {
			return err
		}
		if f.Def == nil {
			if st.kind == nil {
				return errorf(CodeConstraint, "entity %s: field %s has no definition and the entity no kind", ie.ID, f.Name)
			}
			fd, ok := st.kind.Field(f.Name)
			if !ok {
				return &Error{Code: CodeUnknownField, Msg: model.UnknownField(f.Name, st.names()).Error() + " in kind " + st.kind.Name}
			}
			c, err := tx.conform(fd, f.Value)
			if err != nil {
				return err
			}
			st.fields[f.Name] = &model.Field{ID: f.ID, Entity: ie.ID, Name: f.Name, Value: c, Declared: true}
			continue
		}
		def := *f.Def
		def.Name = f.Name
		if err := tx.addField(st, Add{Def: def, Value: f.Value}); err != nil {
			return err
		}
		st.fields[f.Name].ID = f.ID
	}
	return tx.fail2(tx.save(st))
}

// ImportRelation writes a relation with the ID given.
func (tx *Tx) ImportRelation(id, from value.ID, typ string, to value.ID, props Fields) error {
	if err := tx.unusedID(id, "relation"); err != nil {
		return err
	}
	_, err := tx.relate(id, from, typ, to, props)
	return err
}

// AllEntities calls fn with the ID of every entity, kindless ones too, in
// ID order, until fn returns false.
func (tx *Tx) AllEntities(fn func(value.ID) bool) error {
	return tx.scanIDs(keys.PrefixEntity, func(id value.ID) (bool, error) { return fn(id), nil })
}

// AllRelations calls fn with every relation, in ID order, until fn
// returns false.
func (tx *Tx) AllRelations(fn func(*model.Relation) bool) error {
	return tx.scanIDs(keys.PrefixRelation, func(id value.ID) (bool, error) {
		r, err := tx.Relation(id)
		if err != nil {
			return false, err
		}
		return fn(r), nil
	})
}

// scanIDs calls fn with the ID in each key under a one-byte prefix whose
// keys are the prefix and an ID.
func (tx *Tx) scanIDs(prefix byte, fn func(value.ID) (bool, error)) error {
	stop := errorf(CodeInternal, "stop")
	err := tx.scan([]byte{prefix}, false, func(k, _ []byte) error {
		if len(k) != 17 {
			return errorf(CodeInternal, "bad key %x", k)
		}
		more, err := fn(value.ID(k[1:]))
		if err != nil {
			return err
		}
		if !more {
			return stop
		}
		return nil
	})
	if err == stop {
		return nil
	}
	return err
}
