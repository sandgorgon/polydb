package catalog

import (
	"fmt"
	"slices"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// Edit is a set of catalog changes being prepared. Each change is checked
// against the definitions as changed so far; Commit writes them into a
// storage batch and returns the new catalog. An Edit is not safe for
// concurrent use.
//
// Checks that depend on stored data (for example, that no entities of a
// kind exist before it is dropped) are the engine's job.
type Edit struct {
	c     *Catalog
	ids   *value.IDGenerator
	dirty map[defKey]bool // definitions to write or delete
}

type defKey struct {
	typ byte
	id  value.ID
}

// Edit starts an edit of c. ids generates IDs for new definitions.
func (c *Catalog) Edit(ids *value.IDGenerator) *Edit {
	n := &Catalog{
		version:   c.version,
		kinds:     c.kinds.clone(),
		relations: c.relations.clone(),
		formats:   c.formats.clone(),
		indexes:   c.indexes.clone(),
		derived:   c.derived,
	}
	return &Edit{c: n, ids: ids, dirty: map[defKey]bool{}}
}

// Catalog returns the catalog as changed so far. It must not be used after
// further changes.
func (e *Edit) Catalog() *Catalog { return e.c }

// Changed reports whether the edit has any changes.
func (e *Edit) Changed() bool { return len(e.dirty) > 0 }

// --- kinds ---

// DefineKind adds a new kind and returns its ID. k.ID is ignored.
func (e *Edit) DefineKind(k model.Kind) (value.ID, error) {
	if _, ok := e.c.kinds.byName[k.Name]; ok {
		return value.ID{}, fmt.Errorf("kind %s already exists", k.Name)
	}
	k.ID = e.ids.New()
	k.Fields = slices.Clone(k.Fields)
	if err := e.putKind(&k, ""); err != nil {
		return value.ID{}, err
	}
	return k.ID, nil
}

// ReplaceKind replaces the kind with ID k.ID. This covers every alter kind
// form: adding, dropping or changing fields, renaming, and switching
// between closed and extensible.
func (e *Edit) ReplaceKind(k model.Kind) error {
	old, ok := e.c.kinds.byID[k.ID]
	if !ok {
		return fmt.Errorf("no kind with ID %s", k.ID)
	}
	if id, ok := e.c.kinds.byName[k.Name]; ok && id != k.ID {
		return fmt.Errorf("kind %s already exists", k.Name)
	}
	k.Fields = slices.Clone(k.Fields)
	return e.putKind(&k, old.Name)
}

func (e *Edit) putKind(k *model.Kind, oldName string) error {
	if err := k.Validate(); err != nil {
		return err
	}
	// Refs to the kind itself are allowed, including under a new name.
	if err := e.checkFieldRefs("kind "+k.Name, k.Fields, k.Name, oldName); err != nil {
		return err
	}
	for _, ix := range e.c.indexes.byID {
		if ix.Kind == k.ID {
			if err := e.checkIndexFields(ix, k); err != nil {
				return fmt.Errorf("%w (drop the index first)", err)
			}
		}
	}
	if oldName != "" && oldName != k.Name {
		delete(e.c.kinds.byName, oldName)
		e.renameRefs(oldName, k.Name, k)
	}
	e.c.kinds.byID[k.ID] = k
	e.c.kinds.byName[k.Name] = k.ID
	e.dirty[defKey{defKind, k.ID}] = true
	return nil
}

// renameRefs rewrites ref<old> to ref<new> in every definition held in
// memory. Stored definitions use IDs, so only the renamed kind is written.
func (e *Edit) renameRefs(old, new string, renamed *model.Kind) {
	fix := func(fields []model.FieldDef) []model.FieldDef {
		out := slices.Clone(fields)
		for i := range out {
			out[i].Type, _ = out[i].Type.MapNames(func(c value.Code, n string) (string, error) {
				if c == value.CodeRef && n == old {
					return new, nil
				}
				return n, nil
			})
		}
		return out
	}
	for id, k := range e.c.kinds.byID {
		if id == renamed.ID {
			k = renamed
		}
		c := *k
		c.Fields = fix(k.Fields)
		if id == renamed.ID {
			*renamed = c
		} else {
			e.c.kinds.byID[id] = &c
		}
	}
	for id, r := range e.c.relations.byID {
		c := *r
		c.Props = fix(r.Props)
		e.c.relations.byID[id] = &c
	}
	for id, f := range e.c.formats.byID {
		c := *f
		c.Fields = fix(f.Fields)
		e.c.formats.byID[id] = &c
	}
	e.rebuildDerived()
}

// DropKind removes a kind. It fails while other definitions refer to it.
func (e *Edit) DropKind(name string) error {
	k, ok := e.c.kinds.get(name)
	if !ok {
		return fmt.Errorf("no kind %s", name)
	}
	if user := e.kindUser(k); user != "" {
		return fmt.Errorf("kind %s is used by %s", name, user)
	}
	delete(e.c.kinds.byID, k.ID)
	delete(e.c.kinds.byName, name)
	e.dirty[defKey{defKind, k.ID}] = true
	return nil
}

// kindUser names a definition that refers to k, or returns "".
func (e *Edit) kindUser(k *model.Kind) string {
	uses := func(fields []model.FieldDef) bool {
		for _, fd := range fields {
			found := false
			fd.Type.Names(func(c value.Code, n string) { found = found || c == value.CodeRef && n == k.Name })
			if found {
				return true
			}
		}
		return false
	}
	for _, o := range e.c.Kinds() {
		if o.ID != k.ID && uses(o.Fields) {
			return "kind " + o.Name
		}
	}
	for _, r := range e.c.RelationTypes() {
		if r.From.Kind == k.ID || r.To.Kind == k.ID || uses(r.Props) {
			return "relation " + r.Name
		}
	}
	for _, f := range e.c.Formats() {
		if uses(f.Fields) {
			return "format " + f.Name
		}
	}
	for _, ix := range e.c.Indexes() {
		if ix.Kind == k.ID {
			return "index " + ix.Name
		}
	}
	return ""
}

// checkFieldRefs checks that every ref<Kind> and doc<Format> in fields
// names an existing kind or format. selfNames are extra kind names
// allowed (the kind being defined).
func (e *Edit) checkFieldRefs(owner string, fields []model.FieldDef, selfNames ...string) error {
	for _, fd := range fields {
		var err error
		fd.Type.Names(func(c value.Code, n string) {
			if err != nil {
				return
			}
			switch {
			case c == value.CodeRef:
				if _, ok := e.c.kinds.byName[n]; !ok && !slices.Contains(selfNames, n) {
					err = fmt.Errorf("%s: field %s: no kind %s", owner, fd.Name, n)
				}
			case c == value.CodeDoc:
				if _, ok := e.c.Format(n); !ok {
					err = fmt.Errorf("%s: field %s: no format %s", owner, fd.Name, n)
				}
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// --- relation types ---

// DefineRelationType defines a relation type and returns its ID. If the
// type was already registered as undefined (by use), the definition takes
// over its ID so existing relations keep their type.
func (e *Edit) DefineRelationType(r model.RelationType) (value.ID, error) {
	r.Defined = true
	if old, ok := e.c.relations.get(r.Name); ok {
		if old.Defined {
			return value.ID{}, fmt.Errorf("relation %s already exists", r.Name)
		}
		r.ID = old.ID
	} else {
		r.ID = e.ids.New()
	}
	r.Props = slices.Clone(r.Props)
	if err := e.putRelation(&r, ""); err != nil {
		return value.ID{}, err
	}
	return r.ID, nil
}

// EnsureRelationType returns the ID of the named relation type, registering
// it as undefined if it doesn't exist yet.
func (e *Edit) EnsureRelationType(name string) (value.ID, error) {
	if id, ok := e.c.relations.byName[name]; ok {
		return id, nil
	}
	r := model.RelationType{ID: e.ids.New(), Name: name}
	if err := e.putRelation(&r, ""); err != nil {
		return value.ID{}, err
	}
	return r.ID, nil
}

// ReplaceRelationType replaces the relation type with ID r.ID (alter
// relation). Setting Defined to false turns it back into an undefined type.
func (e *Edit) ReplaceRelationType(r model.RelationType) error {
	old, ok := e.c.relations.byID[r.ID]
	if !ok {
		return fmt.Errorf("no relation type with ID %s", r.ID)
	}
	if id, ok := e.c.relations.byName[r.Name]; ok && id != r.ID {
		return fmt.Errorf("relation %s already exists", r.Name)
	}
	r.Props = slices.Clone(r.Props)
	return e.putRelation(&r, old.Name)
}

func (e *Edit) putRelation(r *model.RelationType, oldName string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for _, ep := range []model.Endpoint{r.From, r.To} {
		if ep.Mode == model.EndpointKind {
			if _, ok := e.c.kinds.byID[ep.Kind]; !ok {
				return fmt.Errorf("relation %s: endpoint kind %s doesn't exist", r.Name, ep.Kind)
			}
		}
	}
	if err := e.checkFieldRefs("relation "+r.Name, r.Props); err != nil {
		return err
	}
	if oldName != "" {
		delete(e.c.relations.byName, oldName)
	}
	e.c.relations.byID[r.ID] = r
	e.c.relations.byName[r.Name] = r.ID
	e.dirty[defKey{defRelation, r.ID}] = true
	return nil
}

// DropRelationType removes a relation type. The engine must first make
// sure no relations of the type exist.
func (e *Edit) DropRelationType(name string) error {
	r, ok := e.c.relations.get(name)
	if !ok {
		return fmt.Errorf("no relation %s", name)
	}
	delete(e.c.relations.byID, r.ID)
	delete(e.c.relations.byName, name)
	e.dirty[defKey{defRelation, r.ID}] = true
	return nil
}

// --- formats ---

// DefineFormat adds a derived format and returns its ID.
func (e *Edit) DefineFormat(f model.Format) (value.ID, error) {
	if _, ok := e.c.Format(f.Name); ok {
		return value.ID{}, fmt.Errorf("format %s already exists", f.Name)
	}
	if err := f.Validate(); err != nil {
		return value.ID{}, err
	}
	if _, derived := e.c.formats.byName[f.Base]; derived {
		return value.ID{}, fmt.Errorf("format %s: base %s must be a base format, not a derived one", f.Name, f.Base)
	}
	if _, ok := value.LookupFormat(f.Base); !ok {
		return value.ID{}, fmt.Errorf("format %s: no base format %s", f.Name, f.Base)
	}
	if err := e.checkFieldRefs("format "+f.Name, f.Fields); err != nil {
		return value.ID{}, err
	}
	f.ID = e.ids.New()
	f.Fields = slices.Clone(f.Fields)
	e.c.formats.byID[f.ID] = &f
	e.c.formats.byName[f.Name] = f.ID
	e.dirty[defKey{defFormat, f.ID}] = true
	e.rebuildDerived()
	return f.ID, nil
}

// DropFormat removes a derived format. It fails while other definitions
// use it.
func (e *Edit) DropFormat(name string) error {
	f, ok := e.c.formats.get(name)
	if !ok {
		return fmt.Errorf("no derived format %s", name)
	}
	uses := func(fields []model.FieldDef) bool {
		found := false
		for _, fd := range fields {
			fd.Type.Names(func(c value.Code, n string) { found = found || c == value.CodeDoc && n == name })
		}
		return found
	}
	for _, k := range e.c.Kinds() {
		if uses(k.Fields) {
			return fmt.Errorf("format %s is used by kind %s", name, k.Name)
		}
	}
	for _, r := range e.c.RelationTypes() {
		if uses(r.Props) {
			return fmt.Errorf("format %s is used by relation %s", name, r.Name)
		}
	}
	for _, o := range e.c.Formats() {
		if o.ID != f.ID && uses(o.Fields) {
			return fmt.Errorf("format %s is used by format %s", name, o.Name)
		}
	}
	delete(e.c.formats.byID, f.ID)
	delete(e.c.formats.byName, name)
	e.dirty[defKey{defFormat, f.ID}] = true
	e.rebuildDerived()
	return nil
}

// rebuildDerived rebuilds the value.Format objects for derived formats.
func (e *Edit) rebuildDerived() {
	e.c.derived = buildDerived(e.c.formats)
}

func buildDerived(t table[model.Format]) map[string]*value.DerivedFormat {
	out := map[string]*value.DerivedFormat{}
	for _, f := range t.byID {
		base, ok := value.LookupFormat(f.Base)
		if !ok {
			continue // its base was unregistered from the Go program
		}
		var d *value.DerivedFormat
		var err error
		if f.IsTable() {
			d, err = value.NewTableFormat(f.Name, base, f.Table())
		} else {
			d, err = value.NewDerivedFormat(f.Name, base, f.Shape())
		}
		if err == nil {
			out[f.Name] = d
		}
	}
	return out
}

// --- indexes ---

// DefineIndex adds an index and returns its ID.
func (e *Edit) DefineIndex(ix model.Index) (value.ID, error) {
	if _, ok := e.c.indexes.byName[ix.Name]; ok {
		return value.ID{}, fmt.Errorf("index %s already exists", ix.Name)
	}
	if err := ix.Validate(); err != nil {
		return value.ID{}, err
	}
	k, ok := e.c.kinds.byID[ix.Kind]
	if !ok {
		return value.ID{}, fmt.Errorf("index %s: kind %s doesn't exist", ix.Name, ix.Kind)
	}
	if err := e.checkIndexFields(&ix, k); err != nil {
		return value.ID{}, err
	}
	ix.ID = e.ids.New()
	ix.Keys = slices.Clone(ix.Keys)
	e.c.indexes.byID[ix.ID] = &ix
	e.c.indexes.byName[ix.Name] = ix.ID
	e.dirty[defKey{defIndex, ix.ID}] = true
	return ix.ID, nil
}

func (e *Edit) checkIndexFields(ix *model.Index, k *model.Kind) error {
	for _, key := range ix.Keys {
		fd, ok := k.Field(key.Field)
		if !ok {
			return fmt.Errorf("index %s: kind %s has no field %s", ix.Name, k.Name, key.Field)
		}
		switch {
		case len(key.Path) > 0 && fd.Type.Code != value.CodeDoc:
			return fmt.Errorf("index %s: field %s is not a doc, so it has no path", ix.Name, key.Field)
		case ix.Text && fd.Type.Code != value.CodeText:
			return fmt.Errorf("index %s: a text index needs a text field, and %s is %s", ix.Name, key.Field, fd.Type)
		case len(key.Path) == 0 && !ix.Text && !fd.Type.Keyable():
			return fmt.Errorf("index %s: field %s has type %s, which can't be indexed", ix.Name, key.Field, fd.Type)
		}
	}
	return nil
}

// DropIndex removes an index.
func (e *Edit) DropIndex(name string) error {
	ix, ok := e.c.indexes.get(name)
	if !ok {
		return fmt.Errorf("no index %s", name)
	}
	delete(e.c.indexes.byID, ix.ID)
	delete(e.c.indexes.byName, name)
	e.dirty[defKey{defIndex, ix.ID}] = true
	return nil
}

// --- commit ---

// Writer receives the stored form of catalog changes: a storage.Batch or a
// transaction.
type Writer interface {
	Set(key, value []byte)
	Delete(key []byte)
}

// Commit writes the changes into b and returns the new catalog, whose
// version is one more than the catalog the edit started from. The edit
// must not be used afterwards. The caller commits b together with any
// data changes, atomically.
func (e *Edit) Commit(b Writer) (*Catalog, error) {
	c := e.c
	for dk := range e.dirty {
		k := keys.Catalog(dk.typ, dk.id)
		var rec []byte
		var err error
		switch dk.typ {
		case defKind:
			if d, ok := c.kinds.byID[dk.id]; ok {
				rec, err = encodeKind(d, c)
			}
		case defRelation:
			if d, ok := c.relations.byID[dk.id]; ok {
				rec, err = encodeRelation(d, c)
			}
		case defFormat:
			if d, ok := c.formats.byID[dk.id]; ok {
				rec, err = encodeFormat(d, c)
			}
		case defIndex:
			if d, ok := c.indexes.byID[dk.id]; ok {
				rec = encodeIndex(d)
			}
		}
		if err != nil {
			return nil, err
		}
		if rec == nil {
			b.Delete(k)
		} else {
			b.Set(k, rec)
		}
	}
	if len(e.dirty) > 0 {
		c.version++
	}
	e.c, e.dirty = nil, nil
	return c, nil
}
