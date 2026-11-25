package query

import (
	"fmt"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/value"
)

// historyStmt runs history @e.field: the field's stored versions, oldest
// first, with the time each started.
func (x *executor) historyStmt(s *lang.History) (*Result, error) {
	id, err := x.entityTarget(s.Field.Target, nil, security.Read)
	if err != nil {
		return nil, err
	}
	vs, err := x.tx.FieldHistory(id, s.Field.Field)
	if err != nil {
		return nil, fail(s, err)
	}
	res := &Result{Columns: []Column{{"at", lang.Static{Type: value.TypeTime}}, {"value", lang.Static{Type: value.TypeAny}}}}
	for _, v := range vs {
		var val value.Value = value.Null
		if v.Value != nil {
			val = v.Value
		}
		res.Rows = append(res.Rows, []any{v.At, val})
	}
	return res, nil
}

// asOf starts reading as of the pipeline's time, and returns a function
// that goes back to reading the present.
func (x *executor) asOf(pl *lang.Pipeline) (func(), error) {
	v, err := x.eval(pl.AsOf, nil)
	if err != nil {
		return nil, err
	}
	t, ok := v.(value.Time)
	if !ok {
		return nil, errAt(pl.AsOf, "type", "as of needs a time, not %s", v.Code())
	}
	for _, st := range pl.Steps {
		switch st.(type) {
		case *lang.Follow:
			return nil, errAt(st, "unsupported", "relations have no history, so an as of query can't follow them")
		case *lang.DataStep:
			return nil, errAt(st, "unsupported", "an as of query reads the past; it can't change data")
		}
	}
	prevPast, prevGraph := x.past, x.ev.Graph
	x.past = x.tx.AsOf(t)
	x.ev.Graph = &pastGraph{a: x.past, g: x.graph}
	return func() { x.past, x.ev.Graph = prevPast, prevGraph }, nil
}

// pastGraph implements lang.Graph as of a past time: fields with history
// read their value then; the rest can't be read.
type pastGraph struct {
	a *engine.AsOf
	g *graph
}

func pastErr(code engine.Code, format string, args ...any) error {
	return &engine.Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// entity reports whether id was an entity the user may read, at the time.
func (p *pastGraph) entity(id value.ID) (*model.Kind, bool, error) {
	kind, ok, err := p.a.Entity(id)
	if err != nil || !ok {
		return nil, false, err
	}
	if p.g.access != nil && !p.g.access.Can(security.Read, kind) {
		return nil, false, nil
	}
	return kind, true, nil
}

func (p *pastGraph) Field(id value.ID, name string) (value.Value, bool, error) {
	_, ok, err := p.entity(id)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		if n, exists, err := p.g.tx.Exists(id); err == nil && exists && n == model.NodeRelation {
			return nil, false, pastErr(engine.CodeNotFound, "relations have no history, so their properties can't be read as of a past time")
		}
		return value.Null, false, err
	}
	return p.a.Field(id, name)
}

func (p *pastGraph) FieldMeta(id value.ID, field, name string) (value.Value, bool, error) {
	if _, ok, err := p.entity(id); err != nil || !ok {
		return value.Null, false, err
	}
	// A field's id, name, doc, type and declared flag are definitions,
	// not data; they read as they are now.
	return p.g.FieldMeta(id, field, name)
}

func (p *pastGraph) Meta(id value.ID, name string) (value.Value, error) {
	kind, ok, err := p.entity(id)
	if err != nil || !ok {
		return value.Null, err
	}
	switch name {
	case "id":
		return id, nil
	case "kind":
		if kind == nil {
			return value.Null, nil
		}
		return value.MustText(kind.Name), nil
	case "created":
		return p.a.Created(id)
	}
	return nil, pastErr(engine.CodeNotFound, "::%s has no history, so it can't be read as of a past time", name)
}

func (p *pastGraph) Degree(id value.ID, typ, dir string) (int, error) {
	return 0, pastErr(engine.CodeNotFound, "relations have no history, so they can't be counted as of a past time")
}

func (p *pastGraph) Format(name string) (value.Format, bool) { return p.g.Format(name) }
