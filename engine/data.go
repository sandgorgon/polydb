package engine

import (
	"maps"
	"slices"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Fields maps field names to values.
type Fields map[string]value.Value

// Add is a field added to one entity (spec/rhizome.md §6.2): its
// definition and value. A nil Def.Type means any.
type Add struct {
	Def   model.FieldDef
	Value value.Value
}

// conform checks v against a field definition: its type (with the
// implicit conversions of spec/types.md §10.1) and, for ref<Kind> values,
// that the target exists and has that kind.
func (tx *Tx) conform(fd *model.FieldDef, v value.Value) (value.Value, error) {
	c, err := value.Conform(v, fd.Type)
	if err != nil {
		return nil, errorf(CodeType, "field %s: %v", fd.Name, err)
	}
	if err := tx.checkRefs(fd.Name, fd.Type, c); err != nil {
		return nil, err
	}
	return c, nil
}

// checkRefs checks ref<Kind> values inside v (which conforms to t).
func (tx *Tx) checkRefs(field string, t *value.Type, v value.Value) error {
	if value.IsNull(v) {
		return nil
	}
	switch t.Code {
	case value.CodeRef:
		if t.Name == "" {
			return nil
		}
		return tx.checkRefTarget(field, []string{t.Name}, v.(value.Ref))
	case value.CodeUnion:
		r, ok := v.(value.Ref)
		if !ok {
			return nil
		}
		var kinds []string
		for _, a := range t.Alts {
			if a.Code == value.CodeRef {
				if a.Name == "" {
					return nil
				}
				kinds = append(kinds, a.Name)
			}
		}
		return tx.checkRefTarget(field, kinds, r)
	case value.CodeList, value.CodeArray, value.CodeSet:
		var elems []value.Value
		switch x := v.(type) {
		case value.List:
			elems = x.Values()
		case value.Array:
			elems = x.Values()
		case value.Set:
			elems = x.Values()
		}
		for _, e := range elems {
			if err := tx.checkRefs(field, t.Elem, e); err != nil {
				return err
			}
		}
	case value.CodeFifo, value.CodeLifo, value.CodeRing, value.CodePriority:
		for _, e := range v.(value.Queue).Items() {
			if err := tx.checkRefs(field, t.Elem, e); err != nil {
				return err
			}
		}
	case value.CodeMap:
		for k, e := range v.(value.Map).All() {
			if err := tx.checkRefs(field, t.Key, k); err != nil {
				return err
			}
			if err := tx.checkRefs(field, t.Elem, e); err != nil {
				return err
			}
		}
	case value.CodePair:
		p := v.(value.Pair)
		if err := tx.checkRefs(field, t.Key, p.Key()); err != nil {
			return err
		}
		return tx.checkRefs(field, t.Elem, p.Value())
	case value.CodeRecord:
		r := v.(value.Record)
		for i, f := range t.Fields {
			_, fv := r.Field(i)
			if err := tx.checkRefs(field, f.Type, fv); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRefTarget checks that r points to an entity of one of the kinds.
// The target's existence is guarded until commit.
func (tx *Tx) checkRefTarget(field string, kinds []string, r value.Ref) error {
	tx.t.GuardKey(keys.Entity(r.Target))
	b, ok, err := tx.get(keys.Entity(r.Target))
	if err != nil {
		return err
	}
	if !ok {
		return errorf(CodeConstraint, "field %s: ref target %s doesn't exist", field, r.Target)
	}
	e, err := keys.DecodeEntity(r.Target, b)
	if err != nil {
		return wrap(CodeInternal, err)
	}
	for _, name := range kinds {
		if k, ok := tx.Catalog().Kind(name); ok && k.ID == e.Kind {
			return nil
		}
	}
	return errorf(CodeType, "field %s: ref target %s is not a %s", field, r.Target, joinOr(kinds))
}

func joinOr(names []string) string {
	switch len(names) {
	case 0:
		return "?"
	case 1:
		return names[0]
	}
	s := ""
	for i, n := range names {
		if i == len(names)-1 {
			s += " or "
		} else if i > 0 {
			s += ", "
		}
		s += n
	}
	return s
}

func addedDef(def model.FieldDef) model.FieldDef {
	if def.Type == nil {
		def.Type = value.TypeAny
	}
	return def
}

// --- create ---

// Create creates an entity and returns its ID. With a kind, vals may only
// name declared fields, and every required field must be given; extra
// fields go in adds (extensible kinds only). Without a kind (kind ""),
// every field in vals is added with type any.
func (tx *Tx) Create(kind string, vals Fields, adds ...Add) (value.ID, error) {
	if err := tx.writable(); err != nil {
		return value.ID{}, err
	}
	st := &entState{
		e:      &model.Entity{ID: tx.newID()},
		fields: map[string]*model.Field{},
		isNew:  true,
	}
	st.e.Meta.Created = tx.timeNow()
	st.e.Meta.Owner = tx.user
	if kind != "" {
		k, ok := tx.Catalog().Kind(kind)
		if !ok {
			return value.ID{}, tx.unknownKind(kind)
		}
		st.kind, st.e.Kind = k, k.ID
	}
	for _, name := range slices.Sorted(mapKeys(vals)) {
		v := vals[name]
		if st.kind == nil {
			adds = append(adds, Add{Def: model.FieldDef{Name: name}, Value: v})
			continue
		}
		fd, ok := st.kind.Field(name)
		if !ok {
			return value.ID{}, &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, st.names()).Error() + " in kind " + st.kind.Name}
		}
		c, err := tx.conform(fd, v)
		if err != nil {
			return value.ID{}, err
		}
		st.fields[name] = &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: name, Value: c, Declared: true}
	}
	if st.kind != nil {
		for i := range st.kind.Fields {
			fd := &st.kind.Fields[i]
			if _, given := st.fields[fd.Name]; given || fd.Default == "" {
				continue
			}
			v, err := tx.evalDefault(fd)
			if err != nil {
				return value.ID{}, err
			}
			st.fields[fd.Name] = &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: fd.Name, Value: v, Declared: true}
		}
	}
	for _, a := range adds {
		if err := tx.addField(st, a); err != nil {
			return value.ID{}, err
		}
	}
	if err := tx.save(st); err != nil {
		return value.ID{}, tx.fail(err)
	}
	return st.e.ID, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func (tx *Tx) unknownKind(name string) error {
	var names []string
	for _, k := range tx.Catalog().Kinds() {
		names = append(names, k.Name)
	}
	if s := model.Suggest(name, names); s != "" {
		return errorf(CodeNotFound, "no kind %s (did you mean %s?)", name, s)
	}
	return errorf(CodeNotFound, "no kind %s", name)
}

func (tx *Tx) addField(st *entState, a Add) error {
	def := addedDef(a.Def)
	has := func(n string) bool { _, ok := st.fields[n]; return ok }
	if err := model.CheckAddField(st.kind, has, &def); err != nil {
		return wrap(CodeConstraint, err)
	}
	if def.Check != "" {
		types := map[string]*value.Type{def.Name: def.Type}
		if st.kind != nil {
			maps.Copy(types, fieldTypes(st.kind.Fields))
		}
		for n, f := range st.fields {
			if d := fieldDef(st.kind, f); d != nil {
				types[n] = d.Type
			}
		}
		if _, err := tx.checkExpr(&def, types); err != nil {
			return err
		}
	}
	if err := tx.checkTypeNames(def.Type); err != nil {
		return err
	}
	c, err := tx.conform(&def, a.Value)
	if err != nil {
		return err
	}
	st.fields[def.Name] = &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: def.Name, Value: c, Def: &def}
	return nil
}

// checkTypeNames checks that the kinds and formats a type names exist.
func (tx *Tx) checkTypeNames(t *value.Type) error {
	var err error
	t.Names(func(c value.Code, n string) {
		if err != nil {
			return
		}
		if c == value.CodeRef {
			if _, ok := tx.Catalog().Kind(n); !ok {
				err = tx.unknownKind(n)
			}
		} else if _, ok := tx.Catalog().Format(n); !ok {
			err = errorf(CodeNotFound, "no format %s", n)
		}
	})
	return err
}

// --- update ---

// Set changes the values of existing fields (declared by the kind or added
// earlier). It never creates fields; see AddField.
func (tx *Tx) Set(id value.ID, vals Fields) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(mapKeys(vals)) {
		if err := tx.setField(st, name, vals[name]); err != nil {
			return err
		}
	}
	return tx.fail2(tx.save(st))
}

// fail2 marks the transaction failed if a save fails partway.
func (tx *Tx) fail2(err error) error {
	if err != nil {
		return tx.fail(err)
	}
	return nil
}

func (tx *Tx) setField(st *entState, name string, v value.Value) error {
	f, exists := st.fields[name]
	var fd *model.FieldDef
	switch {
	case exists:
		fd = fieldDef(st.kind, f)
	case st.kind != nil:
		if d, ok := st.kind.Field(name); ok {
			fd = d // a declared field that was never set
		}
	}
	if fd == nil {
		return &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, st.names()).Error()}
	}
	c, err := tx.conform(fd, v)
	if err != nil {
		return err
	}
	nf := &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: name, Declared: true}
	if exists {
		cp := *f
		nf = &cp
	}
	nf.Value = c
	st.fields[name] = nf
	return nil
}

// Unset removes the value of an optional declared field or of an added
// field, so it reads as absent. The field keeps its ID and relations.
func (tx *Tx) Unset(id value.ID, names ...string) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	for _, name := range names {
		f, ok := st.fields[name]
		if !ok {
			if st.kind != nil {
				if _, declared := st.kind.Field(name); declared {
					continue // already absent
				}
			}
			return &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, st.names()).Error()}
		}
		if fd := fieldDef(st.kind, f); f.Declared && !fd.Type.Nullable() {
			return errorf(CodeConstraint, "%s.%s is required and can't be unset", st.kind.Name, name)
		}
		cp := *f
		cp.Value = nil
		st.fields[name] = &cp
	}
	return tx.fail2(tx.save(st))
}

// AddField adds fields to one entity (spec/rhizome.md §6.2).
func (tx *Tx) AddField(id value.ID, adds ...Add) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	for _, a := range adds {
		if err := tx.addField(st, a); err != nil {
			return err
		}
	}
	return tx.fail2(tx.save(st))
}

// DropField removes fields the entity added itself. Relations attached to
// a dropped field are removed (or block the drop) as for a deleted entity.
func (tx *Tx) DropField(id value.ID, names ...string) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	for _, name := range names {
		f, ok := st.fields[name]
		switch {
		case !ok:
			return &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, st.names()).Error()}
		case f.Declared:
			return errorf(CodeConstraint, "%s is declared by kind %s; unset it or use alter kind … drop", name, st.kind.Name)
		}
		if err := tx.detachNode(f.ID); err != nil {
			return err
		}
		delete(st.fields, name)
	}
	return tx.fail2(tx.save(st))
}

// SetLabels replaces an entity's labels.
func (tx *Tx) SetLabels(id value.ID, labels value.Set) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	if _, err := value.Conform(labels, value.SetOf(value.TypeText)); err != nil {
		return errorf(CodeType, "labels: %v", err)
	}
	st.e.Meta.Labels = labels
	return tx.fail2(tx.save(st))
}

// SetProps replaces an entity's custom metadata properties.
func (tx *Tx) SetProps(id value.ID, props value.Map) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	for k := range props.All() {
		if _, ok := k.(value.Text); !ok {
			return errorf(CodeType, "metadata property names must be text, not %s", k.Code())
		}
	}
	st.e.Meta.Props = props
	return tx.fail2(tx.save(st))
}

// --- delete ---

// Delete deletes an entity, its fields and the relations touching them
// (spec/rhizome.md §6.3). It fails if a restrict relation or restrict ref
// points to the entity or one of its fields.
func (tx *Tx) Delete(id value.ID) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	nodes := []value.ID{id}
	for _, f := range st.fields {
		nodes = append(nodes, f.ID)
	}
	for _, n := range nodes {
		if err := tx.detachNode(n); err != nil {
			return err
		}
	}
	st.deleted = true
	return tx.fail2(tx.save(st))
}

// detachNode prepares a node (entity or field) for removal: it fails if a
// restrict ref or restrict relation points at it, and otherwise deletes
// the relations touching it. The scans are guarded, so a concurrent
// transaction adding such a ref or relation conflicts.
func (tx *Tx) detachNode(node value.ID) error {
	var refs []string
	err := tx.scan(keys.BackRefs(node), true, func(k, _ []byte) error {
		refs = append(refs, value.ID(k[len(k)-16:]).String())
		return nil
	})
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return errorf(CodeConstraint, "%s is referenced by restrict ref fields %v", node, refs)
	}
	rels := map[value.ID]*model.Relation{} // a self-relation shows up in both directions
	for _, dir := range []byte{keys.PrefixOut, keys.PrefixIn} {
		err := tx.scan(keys.Adjacent(dir, node), true, func(k, _ []byte) error {
			e, err := keys.ParseEdge(k)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			rt, ok := tx.Catalog().RelationTypeByID(e.Type)
			if ok && rt.OnDelete == model.RelRestrict {
				return errorf(CodeConstraint, "%s has a %s relation (%s), which is restrict on delete", node, rt.Name, e.Relation)
			}
			if rels[e.Relation] == nil {
				rel, err := tx.Relation(e.Relation)
				if err != nil {
					return err
				}
				rels[e.Relation] = rel
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, rel := range rels {
		tx.deleteRelation(rel)
	}
	return nil
}

// --- reads ---

// Entity is an entity with its fields, as read in a transaction.
type Entity struct {
	*model.Entity
	Kind   *model.Kind // nil for kindless entities
	Fields []*model.Field
}

// Field returns the named field.
func (e *Entity) Field(name string) (*model.Field, bool) {
	for _, f := range e.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return nil, false
}

// Value returns the value of the named field, or nil if it is absent.
func (e *Entity) Value(name string) value.Value {
	if f, ok := e.Field(name); ok {
		return f.Value
	}
	return nil
}

// Def returns the definition of one of the entity's fields.
func (e *Entity) Def(f *model.Field) *model.FieldDef { return fieldDef(e.Kind, f) }

// Get reads an entity. Fields are ordered as the kind declares them, then
// added fields by name.
func (tx *Tx) Get(id value.ID) (*Entity, error) {
	st, err := tx.loadEntity(id)
	if err != nil {
		return nil, err
	}
	out := &Entity{Entity: st.e, Kind: st.kind}
	if st.kind != nil {
		for _, fd := range st.kind.Fields {
			if f, ok := st.fields[fd.Name]; ok {
				out.Fields = append(out.Fields, f)
			}
		}
	}
	for _, name := range slices.Sorted(mapKeys(st.fields)) {
		if f := st.fields[name]; !f.Declared {
			out.Fields = append(out.Fields, f)
		}
	}
	return out, nil
}

// EntityField reads one field of an entity without loading the others:
// the entity's kind (nil for kindless) and the field's value, nil when
// the field is absent. isEntity is false when id isn't an entity.
func (tx *Tx) EntityField(id value.ID, name string) (kind *model.Kind, v value.Value, isEntity bool, err error) {
	b, ok, err := tx.get(keys.Entity(id))
	if err != nil || !ok {
		return nil, nil, false, err
	}
	kid, err := keys.EntityKind(b)
	if err != nil {
		return nil, nil, false, wrap(CodeInternal, err)
	}
	if !kid.IsZero() {
		if kind, ok = tx.Catalog().KindByID(kid); !ok {
			return nil, nil, false, errorf(CodeInternal, "entity %s has unknown kind %s", id, kid)
		}
	}
	fb, ok, err := tx.get(keys.Field(id, name))
	if err != nil || !ok {
		return kind, nil, true, err
	}
	f, err := keys.DecodeField(id, name, fb, tx.Catalog())
	if err != nil {
		return nil, nil, false, wrap(CodeInternal, err)
	}
	if v, err = tx.resolve(f.Value); err != nil {
		return nil, nil, false, err
	}
	return kind, v, true, nil
}

// Exists reports whether id names an entity, field or relation, and which.
func (tx *Tx) Exists(id value.ID) (model.NodeKind, bool, error) {
	n, ok, err := tx.node(id, false)
	return n.Kind, ok, err
}

// FieldByID returns a field by its ID.
func (tx *Tx) FieldByID(id value.ID) (*model.Field, error) {
	n, ok, err := tx.node(id, false)
	if err != nil {
		return nil, err
	}
	if !ok || n.Kind != model.NodeField {
		return nil, errorf(CodeNotFound, "no field %s", id)
	}
	b, ok, err := tx.get(keys.Field(n.Entity, n.Name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeInternal, "field %s has a node record but no field record", id)
	}
	f, err := keys.DecodeField(n.Entity, n.Name, b, tx.Catalog())
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	f.Value, err = tx.resolve(f.Value)
	return f, err
}

// Entities calls fn with the ID of every entity of the kind, in ID order
// (creation order), until fn returns false. An empty kind lists every
// entity, kindless or not.
func (tx *Tx) Entities(kind string, fn func(value.ID) bool) error {
	prefix := keys.Prefix(keys.PrefixEntity)
	if kind != "" {
		k, ok := tx.Catalog().Kind(kind)
		if !ok {
			return tx.unknownKind(kind)
		}
		prefix = keys.Members(k.ID)
	}
	stop := errorf(CodeInternal, "stop")
	err := tx.scan(prefix, false, func(k, _ []byte) error {
		if !fn(value.ID(k[len(k)-16:])) {
			return stop
		}
		return nil
	})
	if err == stop {
		return nil
	}
	return err
}

// CountEntities counts the entities of a kind, or every entity for "".
func (tx *Tx) CountEntities(kind string) (int, error) {
	if kind == "" {
		return tx.count(keys.Prefix(keys.PrefixEntity))
	}
	k, ok := tx.Catalog().Kind(kind)
	if !ok {
		return 0, tx.unknownKind(kind)
	}
	return tx.count(keys.Members(k.ID))
}

// CountEntitiesUpTo counts the entities of a kind, or every entity for
// "", stopping at max.
func (tx *Tx) CountEntitiesUpTo(kind string, max int) (int, error) {
	prefix := keys.Prefix(keys.PrefixEntity)
	if kind != "" {
		k, ok := tx.Catalog().Kind(kind)
		if !ok {
			return 0, tx.unknownKind(kind)
		}
		prefix = keys.Members(k.ID)
	}
	n := 0
	stop := errorf(CodeInternal, "stop")
	err := tx.scan(prefix, false, func(_, _ []byte) error {
		if n >= max {
			return stop
		}
		n++
		return nil
	})
	if err == stop {
		err = nil
	}
	return n, err
}

// CountRelations counts the relations of a type, or every relation for "".
func (tx *Tx) CountRelations(typ string) (int, error) {
	if typ == "" {
		return tx.count(keys.Prefix(keys.PrefixRelation))
	}
	rt, ok := tx.Catalog().RelationType(typ)
	if !ok {
		return 0, nil
	}
	return tx.count(keys.OfType(rt.ID))
}
