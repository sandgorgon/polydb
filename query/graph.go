package query

import (
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// graph implements lang.Graph on a transaction. Entities are cached for
// the statement; writes reset the cache.
type graph struct {
	tx   *engine.Tx
	ents map[value.ID]*engine.Entity
}

func (g *graph) reset() { clear(g.ents) }

// entity loads an entity, or reports false if id isn't one.
func (g *graph) entity(id value.ID) (*engine.Entity, bool, error) {
	if e, ok := g.ents[id]; ok {
		return e, true, nil
	}
	n, ok, err := g.tx.Exists(id)
	if err != nil || !ok || n != model.NodeEntity {
		return nil, false, err
	}
	e, err := g.tx.Get(id)
	if err != nil {
		return nil, false, err
	}
	g.ents[id] = e
	return e, true, nil
}

func (g *graph) Field(id value.ID, name string) (value.Value, bool, error) {
	if e, ok, err := g.entity(id); err != nil || ok {
		if err != nil {
			return nil, false, err
		}
		f, ok := e.Field(name)
		if !ok || f.Value == nil {
			return value.Null, false, nil
		}
		return f.Value, true, nil
	}
	if n, ok, err := g.tx.Exists(id); err != nil || !ok || n != model.NodeRelation {
		return value.Null, false, err
	}
	rel, err := g.tx.Relation(id)
	if err != nil {
		return nil, false, err
	}
	v, ok := rel.Props.Get(value.MustText(name))
	if !ok {
		return value.Null, false, nil
	}
	return v, true, nil
}

func (g *graph) FieldMeta(id value.ID, field, name string) (value.Value, bool, error) {
	e, ok, err := g.entity(id)
	if err != nil || !ok {
		return value.Null, false, err
	}
	f, ok := e.Field(field)
	if !ok {
		return value.Null, false, nil
	}
	def := e.Def(f)
	switch name {
	case "id":
		return f.ID, true, nil
	case "name":
		return value.MustText(f.Name), true, nil
	case "doc":
		return value.MustText(def.Doc), true, nil
	case "type":
		return value.MustText(def.Type.String()), true, nil
	case "declared":
		return value.Bool(f.Declared), true, nil
	}
	return value.Null, false, nil
}

func (g *graph) Meta(id value.ID, name string) (value.Value, error) {
	e, ok, err := g.entity(id)
	if err != nil {
		return nil, err
	}
	if ok {
		m := e.Meta
		switch name {
		case "id":
			return e.ID, nil
		case "kind":
			if e.Kind == nil {
				return value.Null, nil
			}
			return value.MustText(e.Kind.Name), nil
		case "created":
			return m.Created, nil
		case "updated":
			return m.Updated, nil
		case "version":
			return value.Int(int64(m.Version)), nil
		case "labels":
			return m.Labels, nil
		case "owner":
			return value.MustText(m.Owner), nil
		case "props":
			return m.Props, nil
		}
		return value.Null, nil
	}
	n, ok, err := g.tx.Exists(id)
	if err != nil || !ok {
		return value.Null, err
	}
	switch n {
	case model.NodeRelation:
		rel, err := g.tx.Relation(id)
		if err != nil {
			return nil, err
		}
		switch name {
		case "id":
			return rel.ID, nil
		case "type":
			if rt, ok := g.tx.Catalog().RelationTypeByID(rel.Type); ok {
				return value.MustText(rt.Name), nil
			}
		case "from":
			return rel.From, nil
		case "to":
			return rel.To, nil
		}
	case model.NodeField:
		f, err := g.tx.FieldByID(id)
		if err != nil {
			return nil, err
		}
		switch name {
		case "id":
			return f.ID, nil
		case "name":
			return value.MustText(f.Name), nil
		case "declared":
			return value.Bool(f.Declared), nil
		}
	}
	return value.Null, nil
}

func (g *graph) Degree(id value.ID, typ, dir string) (int, error) {
	d := engine.Both
	switch dir {
	case "out":
		d = engine.Out
	case "in":
		d = engine.In
	}
	rels, err := g.tx.Relations(id, d, typ)
	return len(rels), err
}

func (g *graph) Format(name string) (value.Format, bool) { return g.tx.Catalog().Format(name) }
