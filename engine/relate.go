package engine

import (
	"slices"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Direction selects relations by which end a node is on.
type Direction uint8

const (
	Out  Direction = 1 << iota // relations starting at the node
	In                         // relations ending at the node
	Both = Out | In
)

// Relate creates a relation of type typ from one node (entity or field)
// to another and returns its ID (spec/rhizome.md §6.4). A type that isn't
// defined is registered on first use. props are the relation's
// properties; for a defined type they must match its declared properties.
func (tx *Tx) Relate(from value.ID, typ string, to value.ID, props Fields) (value.ID, error) {
	return tx.relate(tx.newID(), from, typ, to, props)
}

func (tx *Tx) relate(id, from value.ID, typ string, to value.ID, props Fields) (value.ID, error) {
	if err := tx.writable(); err != nil {
		return value.ID{}, err
	}
	rt, ok := tx.Catalog().RelationType(typ)
	if !ok {
		e, err := tx.editing()
		if err != nil {
			return value.ID{}, err
		}
		id, err := e.EnsureRelationType(typ)
		if err != nil {
			return value.ID{}, wrap(CodeType, err)
		}
		rt, _ = tx.Catalog().RelationTypeByID(id)
	}
	if err := tx.checkEndpoint(rt, "from", rt.From, from); err != nil {
		return value.ID{}, err
	}
	if err := tx.checkEndpoint(rt, "to", rt.To, to); err != nil {
		return value.ID{}, err
	}
	pm, err := tx.relationProps(rt, props)
	if err != nil {
		return value.ID{}, err
	}
	if err := tx.checkMax(rt, keys.PrefixOut, from, rt.MaxOut); err != nil {
		return value.ID{}, err
	}
	if err := tx.checkMax(rt, keys.PrefixIn, to, rt.MaxIn); err != nil {
		return value.ID{}, err
	}
	rel := &model.Relation{ID: id, Type: rt.ID, From: from, To: to, Props: pm}
	tx.writeRelation(rel)
	return rel.ID, nil
}

func (tx *Tx) writeRelation(rel *model.Relation) {
	tx.t.Set(keys.Relation(rel.ID), keys.EncodeRelation(rel))
	tx.t.Set(keys.Node(rel.ID), keys.EncodeNode(keys.NodeRef{Kind: model.NodeRelation}))
	tx.t.Set(keys.Out(rel.From, rel.Type, rel.To, rel.ID), nil)
	tx.t.Set(keys.In(rel.To, rel.Type, rel.From, rel.ID), nil)
	tx.t.Set(keys.ByType(rel.Type, rel.ID), nil)
}

func (tx *Tx) deleteRelation(rel *model.Relation) {
	tx.t.Delete(keys.Relation(rel.ID))
	tx.t.Delete(keys.Node(rel.ID))
	tx.t.Delete(keys.Out(rel.From, rel.Type, rel.To, rel.ID))
	tx.t.Delete(keys.In(rel.To, rel.Type, rel.From, rel.ID))
	tx.t.Delete(keys.ByType(rel.Type, rel.ID))
}

// checkEndpoint checks that node exists, is an entity or field, and fits
// the relation type's endpoint rule. The node's existence is guarded, so
// a concurrent delete of it conflicts.
func (tx *Tx) checkEndpoint(rt *model.RelationType, end string, ep model.Endpoint, node value.ID) error {
	n, ok, err := tx.node(node, true)
	if err != nil {
		return err
	}
	if !ok {
		return errorf(CodeNotFound, "relation %s: no entity or field %s", rt.Name, node)
	}
	if n.Kind == model.NodeRelation {
		return errorf(CodeType, "relation %s: %s is a relation; relations connect entities and fields", rt.Name, node)
	}
	switch ep.Mode {
	case model.EndpointField:
		if n.Kind != model.NodeField {
			return errorf(CodeConstraint, "relation %s must %s a field, and %s is an entity", rt.Name, endVerb(end), node)
		}
	case model.EndpointKind:
		k, _ := tx.Catalog().KindByID(ep.Kind)
		if n.Kind != model.NodeEntity {
			return errorf(CodeConstraint, "relation %s must %s a %s, and %s is a field", rt.Name, endVerb(end), k.Name, node)
		}
		b, _, err := tx.get(keys.Entity(node))
		if err != nil {
			return err
		}
		e, err := keys.DecodeEntity(node, b)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if e.Kind != ep.Kind {
			return errorf(CodeConstraint, "relation %s must %s a %s, and %s isn't one", rt.Name, endVerb(end), k.Name, node)
		}
	}
	return nil
}

func endVerb(end string) string {
	if end == "from" {
		return "start at"
	}
	return "end at"
}

// relationProps checks props against the relation type and returns them
// as a map.
func (tx *Tx) relationProps(rt *model.RelationType, props Fields) (value.Map, error) {
	names := slices.Sorted(mapKeys(props))
	vals := make(map[string]value.Value, len(props))
	for _, name := range names {
		v := props[name]
		if !rt.Defined {
			if err := value.ValidateName("property", name); err != nil {
				return value.Map{}, wrap(CodeType, err)
			}
			vals[name] = orNull(v)
			continue
		}
		fd, ok := rt.Prop(name)
		if !ok {
			var declared []string
			for _, p := range rt.Props {
				declared = append(declared, p.Name)
			}
			return value.Map{}, &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, declared).Error() + " in relation " + rt.Name}
		}
		c, err := tx.conform(fd, v)
		if err != nil {
			return value.Map{}, err
		}
		vals[name] = c
	}
	for _, p := range rt.Props {
		if _, ok := vals[p.Name]; !ok && !p.Type.Nullable() {
			return value.Map{}, errorf(CodeConstraint, "relation %s needs property %s", rt.Name, p.Name)
		}
	}
	types := fieldTypes(rt.Props)
	for i := range rt.Props {
		if p := &rt.Props[i]; p.Check != "" {
			if err := tx.runCheck("relation "+rt.Name, p, types, vals); err != nil {
				return value.Map{}, err
			}
		}
	}
	m, err := value.MapOfText(vals)
	return m, wrap(CodeType, err)
}

func orNull(v value.Value) value.Value {
	if v == nil {
		return value.Null
	}
	return v
}

// checkMax checks a max out or max in limit by counting the node's
// relations of the type. The count is guarded until commit.
func (tx *Tx) checkMax(rt *model.RelationType, dir byte, node value.ID, limit int) error {
	if limit == 0 {
		return nil
	}
	n := 0
	err := tx.scan(keys.Adjacent(dir, node, rt.ID), true, func(_, _ []byte) error {
		n++
		return nil
	})
	if err != nil {
		return err
	}
	if n >= limit {
		which := "out"
		if dir == keys.PrefixIn {
			which = "in"
		}
		return errorf(CodeConstraint, "relation %s allows at most %d per node (max %s), and %s has %d", rt.Name, limit, which, node, n)
	}
	return nil
}

// Relation reads a relation.
func (tx *Tx) Relation(id value.ID) (*model.Relation, error) {
	b, ok, err := tx.get(keys.Relation(id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeNotFound, "no relation %s", id)
	}
	rel, err := keys.DecodeRelation(id, b)
	return rel, wrap(CodeInternal, err)
}

// Relations returns the relations touching node in the given direction,
// optionally only those of type typ ("" for any), in key order.
func (tx *Tx) Relations(node value.ID, dir Direction, typ string) ([]*model.Relation, error) {
	var typeID []value.ID
	if typ != "" {
		rt, ok := tx.Catalog().RelationType(typ)
		if !ok {
			return nil, nil
		}
		typeID = []value.ID{rt.ID}
	}
	var out []*model.Relation
	seen := map[value.ID]bool{}
	for _, d := range []struct {
		bit    Direction
		prefix byte
	}{{Out, keys.PrefixOut}, {In, keys.PrefixIn}} {
		if dir&d.bit == 0 {
			continue
		}
		err := tx.scan(keys.Adjacent(d.prefix, node, typeID...), false, func(k, _ []byte) error {
			e, err := keys.ParseEdge(k)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if seen[e.Relation] {
				return nil
			}
			seen[e.Relation] = true
			rel, err := tx.Relation(e.Relation)
			if err != nil {
				return err
			}
			out = append(out, rel)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Unrelate deletes one relation.
func (tx *Tx) Unrelate(id value.ID) error {
	if err := tx.writable(); err != nil {
		return err
	}
	rel, err := tx.Relation(id)
	if err != nil {
		return err
	}
	tx.deleteRelation(rel)
	return nil
}

// UnrelateAll deletes every relation of type typ from one node to another
// and returns how many there were.
func (tx *Tx) UnrelateAll(from value.ID, typ string, to value.ID) (int, error) {
	if err := tx.writable(); err != nil {
		return 0, err
	}
	rels, err := tx.Relations(from, Out, typ)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rel := range rels {
		if rel.To == to {
			tx.deleteRelation(rel)
			n++
		}
	}
	return n, nil
}
