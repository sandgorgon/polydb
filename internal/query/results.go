package query

import (
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// cell converts a row value to a result cell: entities, relations and
// paths become whole objects (spec/protocol.md §6.5), other values stay.
func (x *executor) cell(v value.Value, t lang.Static) (any, error) {
	if v == nil || value.IsNull(v) {
		return value.Null, nil
	}
	switch t.Form {
	case lang.FormEntity, lang.FormNode:
		id, ok := nodeID(v)
		if !ok {
			return v, nil
		}
		n, exists, err := x.tx.Exists(id)
		if err != nil || !exists {
			return value.Null, err
		}
		switch n {
		case model.NodeEntity:
			e, err := x.entityCell(id)
			if e == nil || err != nil {
				return value.Null, err
			}
			return e, nil
		case model.NodeRelation:
			return x.relationCell(id)
		}
		return id, nil
	case lang.FormRelation:
		id, ok := nodeID(v)
		if !ok {
			return v, nil
		}
		return x.relationCell(id)
	case lang.FormPath:
		l, ok := v.(value.List)
		if !ok {
			return v, nil
		}
		p := &Path{}
		for i, item := range l.Values() {
			id, _ := nodeID(item)
			if i%2 == 1 {
				r, err := x.relationCell(id)
				if err != nil {
					return nil, err
				}
				p.Rels = append(p.Rels, r)
				continue
			}
			if _, isRef := item.(value.Ref); isRef {
				e, err := x.entityCell(id)
				if err != nil {
					return nil, err
				}
				if e == nil {
					p.Nodes = append(p.Nodes, value.Null)
					continue
				}
				p.Nodes = append(p.Nodes, e)
			} else {
				p.Nodes = append(p.Nodes, id)
			}
		}
		return p, nil
	}
	return v, nil
}

func nodeID(v value.Value) (value.ID, bool) {
	switch v := v.(type) {
	case value.Ref:
		return v.Target, true
	case value.ID:
		return v, true
	}
	return value.ID{}, false
}

func (x *executor) entityCell(id value.ID) (*Entity, error) {
	e, ok, err := x.graph.entity(id)
	if err != nil || !ok {
		return nil, err
	}
	out := &Entity{ID: e.ID, Meta: e.Meta}
	if e.Kind != nil {
		out.Kind = e.Kind.Name
	}
	for _, f := range e.Fields {
		def := e.Def(f)
		out.Fields = append(out.Fields, &Field{ID: f.ID, Name: f.Name, Type: def.Type, Doc: def.Doc,
			Declared: f.Declared, Value: f.Value})
	}
	return out, nil
}

func (x *executor) relationCell(id value.ID) (*Relation, error) {
	rel, err := x.tx.Relation(id)
	if err != nil {
		return nil, err
	}
	out := &Relation{ID: rel.ID, From: rel.From, To: rel.To, Props: rel.Props}
	if rt, ok := x.tx.Catalog().RelationTypeByID(rel.Type); ok {
		out.Type = rt.Name
	}
	return out, nil
}
