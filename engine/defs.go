package engine

import (
	"fmt"
	"slices"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Definition changes write the catalog version key, which every writing
// transaction guards, so the data checks below can't race with concurrent
// data changes: one of the two transactions fails with a conflict.

// --- kinds ---

// DefineKind defines a kind and returns its ID (spec/rhizome.md §5.1).
func (tx *Tx) DefineKind(k model.Kind) (value.ID, error) {
	e, err := tx.editing()
	if err != nil {
		return value.ID{}, err
	}
	id, err := e.DefineKind(k)
	if err != nil {
		return id, wrap(CodeConstraint, err)
	}
	if err := tx.validateExprs(k.Fields, fieldTypes(k.Fields)); err != nil {
		return id, tx.fail(err)
	}
	return id, nil
}

// AlterKind changes a kind: fn receives a copy of the definition to
// modify (add, drop or change fields, rename, switch closed/extensible).
// Existing entities are checked and migrated: dropped fields are removed,
// fields some entities added themselves become declared when the kind
// declares them with the same type, and values are converted to changed
// types. If any entity doesn't fit, AlterKind fails with a count.
func (tx *Tx) AlterKind(name string, fn func(k *model.Kind) error) error {
	old, ok := tx.Catalog().Kind(name)
	if !ok {
		return tx.unknownKind(name)
	}
	nk := *old
	nk.Fields = slices.Clone(old.Fields)
	if err := fn(&nk); err != nil {
		return err
	}
	nk.ID = old.ID
	e, err := tx.editing()
	if err != nil {
		return err
	}
	if err := e.ReplaceKind(nk); err != nil {
		return wrap(CodeConstraint, err)
	}
	newK, _ := tx.Catalog().KindByID(old.ID)
	if err := tx.validateExprs(newK.Fields, fieldTypes(newK.Fields)); err != nil {
		return tx.fail(err)
	}
	if err := tx.historyBaseline(old, newK); err != nil {
		return tx.fail(err)
	}
	if sameData(old, newK) {
		return nil
	}

	var ids []value.ID
	if err := tx.scan(keys.Members(old.ID), false, func(k, _ []byte) error {
		_, id, err := keys.ParseMember(k)
		ids = append(ids, id)
		return wrap(CodeInternal, err)
	}); err != nil {
		return err
	}
	failed, firstErr := 0, error(nil)
	for _, id := range ids {
		if err := tx.migrate(id, old, newK); err != nil {
			if CodeOf(err) == CodeInternal || CodeOf(err) == CodeConflict {
				return tx.fail(err)
			}
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed > 0 {
		return tx.fail(errorf(CodeConstraint, "%d of %d %s entities don't fit the change; first: %v", failed, len(ids), name, firstErr))
	}
	return nil
}

// sameData reports whether a kind change leaves stored data valid as is.
func sameData(a, b *model.Kind) bool {
	if a.Closed != b.Closed || len(a.Fields) != len(b.Fields) {
		return false
	}
	for i := range a.Fields {
		x, y := a.Fields[i], b.Fields[i]
		if x.Name != y.Name || !x.Type.Equal(y.Type) || x.Unique != y.Unique || x.OnDelete != y.OnDelete || x.Check != y.Check {
			return false
		}
	}
	return true
}

// migrate rewrites one entity for a kind change from old to newK.
func (tx *Tx) migrate(id value.ID, old, newK *model.Kind) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	st.origKind, st.kind = old, newK
	for _, name := range slices.Sorted(mapKeys(st.fields)) {
		f := st.fields[name]
		nfd, declared := newK.Field(name)
		switch {
		case f.Declared && !declared: // the kind dropped this field
			if err := tx.detachNode(f.ID); err != nil {
				return err
			}
			delete(st.fields, name)
		case !f.Declared && declared: // the kind now declares a field this entity added
			// The types must match, apart from being optional.
			if !value.Required(f.Def.Type).Equal(value.Required(nfd.Type)) {
				return errorf(CodeConstraint, "entity %s added %s as %s, but the kind declares it as %s", id, name, f.Def.Type, nfd.Type)
			}
			cp := *f
			cp.Declared, cp.Def = true, nil
			if f.Value != nil {
				c, err := tx.conform(nfd, f.Value)
				if err != nil {
					return fmt.Errorf("entity %s: %w", id, err)
				}
				cp.Value = c
			}
			st.fields[name] = &cp
		case f.Declared && f.Value != nil:
			ofd, _ := old.Field(name)
			if ofd.Type.Equal(nfd.Type) {
				continue
			}
			c, err := tx.conform(nfd, f.Value)
			if err != nil {
				return fmt.Errorf("entity %s: %w", id, err)
			}
			cp := *f
			cp.Value = c
			st.fields[name] = &cp
		}
	}
	for i := range newK.Fields {
		fd := &newK.Fields[i]
		if _, has := st.fields[fd.Name]; has || fd.Default == "" {
			continue
		}
		v, err := tx.evalDefault(fd)
		if err != nil {
			return err
		}
		st.fields[fd.Name] = &model.Field{ID: tx.newID(), Entity: id, Name: fd.Name, Value: v, Declared: true}
	}
	if newK.Closed {
		for _, f := range st.fields {
			if !f.Declared {
				return errorf(CodeConstraint, "entity %s has its own field %s, so the kind can't be closed", id, f.Name)
			}
		}
	}
	if err := tx.save(st); err != nil {
		return fmt.Errorf("entity %s: %w", id, err)
	}
	return nil
}

// DropKind removes a kind. It fails while entities of the kind exist.
func (tx *Tx) DropKind(name string) error {
	k, ok := tx.Catalog().Kind(name)
	if !ok {
		return tx.unknownKind(name)
	}
	n, err := tx.count(keys.Members(k.ID))
	if err != nil {
		return err
	}
	if n > 0 {
		return errorf(CodeConstraint, "kind %s still has %d entities", name, n)
	}
	e, err := tx.editing()
	if err != nil {
		return err
	}
	if err := tx.dropKindHistory(k.ID); err != nil {
		return err
	}
	return wrap(CodeConstraint, e.DropKind(name))
}

func (tx *Tx) count(prefix []byte) (int, error) {
	n := 0
	err := tx.scan(prefix, false, func(_, _ []byte) error { n++; return nil })
	return n, err
}

// --- relation types ---

// RelationDef defines a relation type (spec/rhizome.md §5.2). From and To
// are a kind name, "any" (or "") or "field".
type RelationDef struct {
	Name     string
	Doc      string
	From, To string
	MaxOut   int
	MaxIn    int
	OnDelete model.RelAction
	Props    []model.FieldDef
}

func (tx *Tx) relationType(d RelationDef) (model.RelationType, error) {
	if err := tx.validateExprs(d.Props, fieldTypes(d.Props)); err != nil {
		return model.RelationType{}, err
	}
	rt := model.RelationType{Name: d.Name, Defined: true, Doc: d.Doc, MaxOut: d.MaxOut, MaxIn: d.MaxIn,
		OnDelete: d.OnDelete, Props: d.Props}
	for _, ep := range []struct {
		s   string
		out *model.Endpoint
	}{{d.From, &rt.From}, {d.To, &rt.To}} {
		switch ep.s {
		case "", "any":
		case "field":
			ep.out.Mode = model.EndpointField
		default:
			k, ok := tx.Catalog().Kind(ep.s)
			if !ok {
				return rt, tx.unknownKind(ep.s)
			}
			*ep.out = model.Endpoint{Mode: model.EndpointKind, Kind: k.ID}
		}
	}
	return rt, nil
}

// DefineRelation defines a relation type and returns its ID. If relations
// of that type already exist (it was used before being defined), they
// must fit the definition.
func (tx *Tx) DefineRelation(d RelationDef) (value.ID, error) {
	rt, err := tx.relationType(d)
	if err != nil {
		return value.ID{}, err
	}
	e, err := tx.editing()
	if err != nil {
		return value.ID{}, err
	}
	id, err := e.DefineRelationType(rt)
	if err != nil {
		return value.ID{}, wrap(CodeConstraint, err)
	}
	return id, tx.checkRelationsFit(id)
}

// AlterRelation replaces the definition of relation type name (alter
// relation; d.Name may rename it). Existing relations must fit.
func (tx *Tx) AlterRelation(name string, d RelationDef) error {
	old, ok := tx.Catalog().RelationType(name)
	if !ok {
		return errorf(CodeNotFound, "no relation %s", name)
	}
	rt, err := tx.relationType(d)
	if err != nil {
		return err
	}
	rt.ID = old.ID
	e, err := tx.editing()
	if err != nil {
		return err
	}
	if err := e.ReplaceRelationType(rt); err != nil {
		return wrap(CodeConstraint, err)
	}
	return tx.checkRelationsFit(rt.ID)
}

// checkRelationsFit checks every relation of a type against its
// definition: endpoints, properties and maximums.
func (tx *Tx) checkRelationsFit(typeID value.ID) error {
	rt, _ := tx.Catalog().RelationTypeByID(typeID)
	out, in := map[value.ID]int{}, map[value.ID]int{}
	n := 0
	err := tx.scan(keys.OfType(typeID), false, func(k, _ []byte) error {
		rel, err := tx.Relation(value.ID(k[len(k)-16:]))
		if err != nil {
			return err
		}
		n++
		if err := tx.checkEndpoint(rt, "from", rt.From, rel.From); err != nil {
			return err
		}
		if err := tx.checkEndpoint(rt, "to", rt.To, rel.To); err != nil {
			return err
		}
		props := Fields{}
		for k, v := range rel.Props.All() {
			props[k.(value.Text).Go()] = v
		}
		if _, err := tx.relationProps(rt, props); err != nil {
			return err
		}
		out[rel.From]++
		in[rel.To]++
		return nil
	})
	if err != nil {
		return tx.fail(errorf(CodeConstraint, "existing %s relations don't fit: %v", rt.Name, err))
	}
	for _, c := range []struct {
		counts map[value.ID]int
		limit  int
		which  string
	}{{out, rt.MaxOut, "out"}, {in, rt.MaxIn, "in"}} {
		for node, cnt := range c.counts {
			if c.limit > 0 && cnt > c.limit {
				return tx.fail(errorf(CodeConstraint, "%s has %d %s relations, more than max %s %d", node, cnt, rt.Name, c.which, c.limit))
			}
		}
	}
	return nil
}

// DropRelation removes a relation type. It fails while relations of the
// type exist.
func (tx *Tx) DropRelation(name string) error {
	rt, ok := tx.Catalog().RelationType(name)
	if !ok {
		return errorf(CodeNotFound, "no relation %s", name)
	}
	n, err := tx.count(keys.OfType(rt.ID))
	if err != nil {
		return err
	}
	if n > 0 {
		return errorf(CodeConstraint, "%d %s relations still exist", n, name)
	}
	e, err := tx.editing()
	if err != nil {
		return err
	}
	return wrap(CodeConstraint, e.DropRelationType(name))
}

// --- formats ---

// DefineFormat defines a derived format (spec/rhizome.md §5.3).
func (tx *Tx) DefineFormat(f model.Format) (value.ID, error) {
	e, err := tx.editing()
	if err != nil {
		return value.ID{}, err
	}
	if err := tx.tableDefaults(&f); err != nil {
		return value.ID{}, err
	}
	id, err := e.DefineFormat(f)
	return id, wrap(CodeConstraint, err)
}

// DropFormat removes a derived format. It fails while a definition or a
// field added to an entity uses it.
func (tx *Tx) DropFormat(name string) error {
	if _, ok := tx.Catalog().FormatDef(name); !ok {
		return errorf(CodeNotFound, "no derived format %s", name)
	}
	var user value.ID
	err := tx.scan(keys.Prefix(keys.PrefixField), false, func(k, v []byte) error {
		entity, fname, err := keys.ParseField(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		f, err := keys.DecodeField(entity, fname, v, tx.Catalog())
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if f.Def != nil {
			f.Def.Type.Names(func(c value.Code, n string) {
				if c == value.CodeDoc && n == name {
					user = f.ID
				}
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !user.IsZero() {
		return errorf(CodeConstraint, "format %s is used by field %s", name, user)
	}
	e, err := tx.editing()
	if err != nil {
		return err
	}
	return wrap(CodeConstraint, e.DropFormat(name))
}

// --- indexes ---

// DefineIndex defines an index on a kind (spec/rhizome.md §5.4). Unique
// indexes are enforced from now on; other indexes are recorded and will be
// built and used once the query planner exists (Phase 5).
func (tx *Tx) DefineIndex(name, kind string, unique, text bool, ks ...model.IndexKey) (value.ID, error) {
	k, ok := tx.Catalog().Kind(kind)
	if !ok {
		return value.ID{}, tx.unknownKind(kind)
	}
	if unique {
		for _, key := range ks {
			if len(key.Path) > 0 {
				return value.ID{}, errorf(CodeUnsupported, "unique indexes on paths inside docs aren't supported yet")
			}
		}
	}
	e, err := tx.editing()
	if err != nil {
		return value.ID{}, err
	}
	id, err := e.DefineIndex(model.Index{Name: name, Kind: k.ID, Unique: unique, Text: text, Keys: ks})
	if err != nil {
		return id, wrap(CodeConstraint, err)
	}
	ix, _ := tx.Catalog().IndexByID(id)
	if !unique {
		if err := tx.buildIndex(ix); err != nil {
			return value.ID{}, tx.fail(err)
		}
		return id, nil
	}
	var members []value.ID
	if err := tx.scan(keys.Members(k.ID), false, func(mk, _ []byte) error {
		_, eid, err := keys.ParseMember(mk)
		members = append(members, eid)
		return wrap(CodeInternal, err)
	}); err != nil {
		return value.ID{}, err
	}
	for _, eid := range members {
		st, err := tx.loadEntity(eid)
		if err != nil {
			return value.ID{}, tx.fail(err)
		}
		uk, ok, err := uniqueIndexKey(ix, st.fields)
		if err != nil {
			return value.ID{}, tx.fail(err)
		}
		if !ok {
			continue
		}
		if cur, found, err := tx.get(uk); err != nil {
			return value.ID{}, tx.fail(err)
		} else if found {
			return value.ID{}, tx.fail(errorf(CodeConstraint, "index %s: entities %s and %s have the same values", name, value.ID(cur), eid))
		}
		tx.t.Set(uk, eid[:])
	}
	return id, nil
}

// DropIndex removes an index.
func (tx *Tx) DropIndex(name string) error {
	ix, ok := tx.Catalog().Index(name)
	if !ok {
		return errorf(CodeNotFound, "no index %s", name)
	}
	e, err := tx.editing()
	if err != nil {
		return err
	}
	if err := e.DropIndex(name); err != nil {
		return wrap(CodeConstraint, err)
	}
	if ix.Unique {
		r := keys.Range(keys.UniquePrefix(ix.ID, ""))
		tx.t.DeleteRange(r.Lower, r.Upper)
	} else {
		r := keys.Range(keys.IndexPrefix(ix.ID))
		tx.t.DeleteRange(r.Lower, r.Upper)
		tx.t.Delete(keys.IndexBuild(ix.ID))
	}
	return nil
}
