package query

import (
	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/value"
)

// graph implements lang.Graph on a transaction. Entities are cached for
// the statement, up to cacheLimit at a time; writes reset the cache.
// The first field read from an entity reads only that field; reading
// it again loads and caches the whole entity, so a scan that touches
// each entity once stays cheap and a join that revisits them does too.
type graph struct {
	tx     *engine.Tx
	ents   map[value.ID]*engine.Entity
	hidden map[value.ID]bool // entities the user can't read
	seen   map[value.ID]bool // entities read once, one field at a time
	access Access
}

// cacheLimit bounds the entities a statement keeps cached, so a scan
// over a large kind doesn't hold all of it.
const cacheLimit = 16384

func (g *graph) reset() {
	clear(g.ents)
	clear(g.hidden)
	clear(g.seen)
}

// entity loads an entity, or reports false if id isn't one or is of a
// kind the user can't read.
func (g *graph) entity(id value.ID) (*engine.Entity, bool, error) {
	if e, ok := g.ents[id]; ok {
		return e, true, nil
	}
	if g.hidden[id] {
		return nil, false, nil
	}
	n, ok, err := g.tx.Exists(id)
	if err != nil || !ok || n != model.NodeEntity {
		return nil, false, err
	}
	e, err := g.tx.Get(id)
	if err != nil {
		return nil, false, err
	}
	if g.access != nil && !g.access.Can(security.Read, e.Kind) {
		g.hide(id)
		return nil, false, nil
	}
	if len(g.ents) >= cacheLimit {
		clear(g.ents)
	}
	g.ents[id] = e
	return e, true, nil
}

func (g *graph) hide(id value.ID) {
	if len(g.hidden) >= cacheLimit {
		clear(g.hidden)
	}
	g.hidden[id] = true
}

func (g *graph) Field(id value.ID, name string) (value.Value, bool, error) {
	if e, ok := g.ents[id]; ok {
		f, ok := e.Field(name)
		if !ok || f.Value == nil {
			return value.Null, false, nil
		}
		return f.Value, true, nil
	}
	if g.hidden[id] {
		return value.Null, false, nil
	}
	if g.seen[id] {
		// The second visit: load and cache the whole entity.
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
		return value.Null, false, nil // hidden
	}
	kind, v, isEntity, err := g.tx.EntityField(id, name)
	if err != nil {
		return nil, false, err
	}
	if isEntity {
		if g.access != nil && !g.access.Can(security.Read, kind) {
			g.hide(id)
			return value.Null, false, nil
		}
		if g.seen == nil || len(g.seen) >= cacheLimit {
			g.seen = map[value.ID]bool{}
		}
		g.seen[id] = true
		if v == nil {
			return value.Null, false, nil
		}
		return v, true, nil
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

// DatabaseMeta reads metadata of the transaction's database.
func (g *graph) DatabaseMeta(name string) (value.Value, error) {
	if name == "name" {
		return value.MustText(g.tx.DatabaseName()), nil
	}
	m, err := g.tx.DatabaseMeta()
	if err != nil {
		return nil, err
	}
	switch name {
	case "created":
		return m.Created, nil
	case "owner", "doc":
		s := m.Owner
		if name == "doc" {
			s = m.Doc
		}
		if s == "" {
			return value.Null, nil
		}
		return value.NewText(s)
	case "labels":
		return m.Labels, nil
	case "props":
		return m.Props, nil
	}
	return value.Null, nil
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
	if err != nil || g.access == nil {
		return len(rels), err
	}
	// Count only relations whose other end the user can see.
	n := 0
	for _, r := range rels {
		other := r.To
		if r.To == id {
			other = r.From
		}
		if visible, err := g.visible(other); err != nil {
			return 0, err
		} else if visible {
			n++
		}
	}
	return n, nil
}

// visible reports whether the user can see a node: an entity of a
// readable kind, or a field of one. Relations count as visible.
func (g *graph) visible(id value.ID) (bool, error) {
	n, ok, err := g.tx.Exists(id)
	if err != nil || !ok {
		return false, err
	}
	switch n {
	case model.NodeField:
		f, err := g.tx.FieldByID(id)
		if err != nil {
			return false, err
		}
		id = f.Entity
	case model.NodeRelation:
		return true, nil
	}
	_, ok, err = g.entity(id)
	return ok, err
}

func (g *graph) Format(name string) (value.Format, bool) { return g.tx.Catalog().Format(name) }
