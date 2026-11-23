package query

import (
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// frame returns the evaluation frame for a pipeline row (nil outside
// pipelines).
func frame(row lang.Row) *lang.Frame { return &lang.Frame{Row: row} }

// eval evaluates e for a row, giving errors e's position.
func (x *executor) eval(e lang.Expr, f *lang.Frame) (value.Value, error) {
	v, err := x.ev.Eval(e, f)
	return v, fail(e, err)
}

// target resolves the target of a data statement to a node ID and what
// it is.
func (x *executor) target(e lang.Expr, row lang.Row) (value.ID, model.NodeKind, error) {
	v, err := x.eval(e, frame(row))
	if err != nil {
		return value.ID{}, 0, err
	}
	var id value.ID
	switch v := v.(type) {
	case value.Ref:
		id = v.Target
	case value.ID:
		id = v
	default:
		if value.IsNull(v) {
			return value.ID{}, 0, errAt(e, "not_found", "%s is null", lang.FormatExpr(e))
		}
		return value.ID{}, 0, errAt(e, "type", "%s is %s, not an entity or relation", lang.FormatExpr(e), v.Code())
	}
	n, ok, err := x.tx.Exists(id)
	if err != nil {
		return id, 0, fail(e, err)
	}
	if !ok {
		return id, 0, errAt(e, "not_found", "nothing has id %s", id)
	}
	return id, n, nil
}

// entityTarget is target for statements that change an entity: it
// checks that the user may do p on the entity's kind.
func (x *executor) entityTarget(e lang.Expr, row lang.Row, p security.Perm) (value.ID, error) {
	id, n, err := x.target(e, row)
	if err != nil {
		return id, err
	}
	if n != model.NodeEntity {
		return id, errAt(e, "type", "%s is a %s, not an entity", lang.FormatExpr(e), n)
	}
	if x.ctx.Access != nil {
		k, err := x.nodeKind(e, id)
		if err != nil {
			return id, err
		}
		return id, x.need(e, p, k)
	}
	return id, nil
}

// needVisible fails with a permission error if the user can't read the
// entity.
func (x *executor) needVisible(n lang.Node, id value.ID) error {
	if x.ctx.Access == nil {
		return nil
	}
	if _, ok, err := x.graph.entity(id); err != nil || ok {
		return fail(n, err)
	}
	k, err := x.nodeKind(n, id)
	if err != nil {
		return err
	}
	return x.need(n, security.Read, k)
}

// added converts an add clause entry.
func (x *executor) added(a *lang.AddField, f *lang.Frame) (engine.Add, error) {
	def := model.FieldDef{Name: a.Name, Type: a.Type, Doc: str(a.Doc)}
	if a.Check != nil {
		def.Check = lang.FormatExpr(a.Check)
	}
	if a.OnDelete == "restrict" {
		def.OnDelete = model.RefRestrict
	}
	var err error
	if def.History, err = x.history(a.History); err != nil {
		return engine.Add{}, err
	}
	v, err := x.eval(a.Value, f)
	return engine.Add{Def: def, Value: v}, err
}

func (x *executor) create(s *lang.Create) (*Result, error) {
	k, _ := x.tx.Catalog().Kind(s.Kind)
	if err := x.need(s, security.Write, k); err != nil {
		return nil, err
	}
	vals := engine.Fields{}
	var adds []engine.Add
	for _, in := range s.Inits {
		v, err := x.eval(in.Value, nil)
		if err != nil {
			return nil, err
		}
		if s.Kind == "" {
			// Kindless fields are added with type any, keeping their docs.
			adds = append(adds, engine.Add{Def: model.FieldDef{Name: in.Key, Doc: str(in.Doc)}, Value: v})
			continue
		}
		vals[in.Key] = v
	}
	for _, a := range s.Adds {
		add, err := x.added(a, nil)
		if err != nil {
			return nil, err
		}
		adds = append(adds, add)
	}
	id, err := x.tx.Create(s.Kind, vals, adds...)
	if err != nil {
		return nil, fail(s, err)
	}
	if s.Handle != "" {
		h := Handle{ID: id, Node: model.NodeEntity}
		if k, ok := x.tx.Catalog().Kind(s.Kind); ok {
			h.Kind = k.ID
		}
		x.bindHandle(s.Handle, h)
	}
	return &Result{
		Columns:  []Column{{"id", lang.Static{Type: value.TypeID}}},
		Rows:     [][]any{{id}},
		Affected: 1,
	}, nil
}

func (x *executor) update(s *lang.Update, row lang.Row) (int, error) {
	id, err := x.entityTarget(s.Target, row, security.Write)
	if err != nil {
		return 0, err
	}
	f := &lang.Frame{Row: row, Target: id, HasTarget: true}
	for _, a := range s.Actions {
		var err error
		switch a := a.(type) {
		case *lang.SetAction:
			for _, as := range a.Assigns {
				if err = x.assign(as, id, f); err != nil {
					break
				}
			}
		case *lang.UnsetAction:
			err = fail(s, x.tx.Unset(id, a.Names...))
		case *lang.AddAction:
			var adds []engine.Add
			for _, af := range a.Fields {
				add, err := x.added(af, f)
				if err != nil {
					return 0, err
				}
				adds = append(adds, add)
			}
			err = fail(s, x.tx.AddField(id, adds...))
		case *lang.DropAction:
			err = fail(s, x.tx.DropField(id, a.Names...))
		}
		x.graph.reset()
		if err != nil {
			return 0, err
		}
	}
	return 1, nil
}

// assign runs one assignment: it reads the current value, applies the
// operator, writes the result back into nested elements, and stores the
// field or metadata.
func (x *executor) assign(as *lang.Assign, id value.ID, f *lang.Frame) error {
	lv := as.Target
	var cur value.Value
	var err error
	if lv.Meta {
		cur, err = x.graph.Meta(id, lv.Name)
	} else {
		cur, _, err = x.graph.Field(id, lv.Name)
	}
	if err != nil {
		return fail(as, err)
	}
	rhs, err := x.eval(as.Value, f)
	if err != nil {
		return err
	}
	nv, err := x.setIn(as, orNull(cur), lv.Steps, f, func(old value.Value) (value.Value, error) {
		return x.ev.Assign(as, old, rhs)
	})
	if err != nil {
		return fail(as, err)
	}
	switch {
	case !lv.Meta:
		err = x.tx.Set(id, engine.Fields{lv.Name: nv})
	case lv.Name == "labels":
		s, ok := nv.(value.Set)
		if !ok && !value.IsNull(nv) {
			return errAt(as, "type", "::labels must be a set of text, not %s", nv.Code())
		}
		err = x.tx.SetLabels(id, s)
	default:
		m, ok := nv.(value.Map)
		if !ok && !value.IsNull(nv) {
			return errAt(as, "type", "::props must be a map, not %s", nv.Code())
		}
		err = x.tx.SetProps(id, m)
	}
	x.graph.reset()
	return fail(as, err)
}

func orNull(v value.Value) value.Value {
	if v == nil {
		return value.Null
	}
	return v
}

// setIn applies change to the element of cur that steps lead to, and
// returns cur with that element replaced: name[i], name["k"], name.sub.
func (x *executor) setIn(as *lang.Assign, cur value.Value, steps []lang.LStep, f *lang.Frame, change func(value.Value) (value.Value, error)) (value.Value, error) {
	if len(steps) == 0 {
		return change(cur)
	}
	st := steps[0]
	var key value.Value
	if st.Index != nil {
		var err error
		if key, err = x.eval(st.Index, f); err != nil {
			return nil, err
		}
	} else {
		key = value.MustText(st.Field)
	}
	switch c := cur.(type) {
	case value.List, value.Array:
		var elems []value.Value
		if l, ok := c.(value.List); ok {
			elems = l.Values()
		} else {
			elems = c.(value.Array).Values()
		}
		i, ok := key.(value.Int)
		if !ok || i < 0 || int64(i) >= int64(len(elems)) {
			return nil, errAt(as, "constraint", "position %s is outside the list (length %d)", key, len(elems))
		}
		nv, err := x.setIn(as, orNull(elems[i]), steps[1:], f, change)
		if err != nil {
			return nil, err
		}
		elems[i] = nv
		if _, isList := c.(value.List); isList {
			return value.NewList(elems...)
		}
		return value.NewArray(elems...)
	case value.Map:
		old, _ := c.Get(key)
		nv, err := x.setIn(as, orNull(old), steps[1:], f, change)
		if err != nil {
			return nil, err
		}
		var ks, vs []value.Value
		for k, v := range c.All() {
			if !value.Equal(k, key) {
				ks, vs = append(ks, k), append(vs, v)
			}
		}
		return value.NewMap(append(ks, key), append(vs, nv))
	case value.Record:
		name, ok := key.(value.Text)
		if !ok {
			return nil, errAt(as, "type", "record fields are named by text, not %s", key.Code())
		}
		old, has := c.Get(name.Go())
		if !has {
			return nil, errAt(as, "unknown_field", "the record has no field %s", name.Go())
		}
		nv, err := x.setIn(as, orNull(old), steps[1:], f, change)
		if err != nil {
			return nil, err
		}
		names := c.Names()
		vals := make([]value.Value, len(names))
		for i, n := range names {
			vals[i], _ = c.Get(n)
			if n == name.Go() {
				vals[i] = nv
			}
		}
		return value.NewRecord(names, vals)
	}
	if value.IsNull(cur) {
		return nil, errAt(as, "constraint", "can't set an element of a field that is null")
	}
	return nil, errAt(as, "type", "can't set an element of %s", cur.Code())
}

func (x *executor) delete(s *lang.Delete, row lang.Row) (int, error) {
	id, err := x.entityTarget(s.Target, row, security.Write)
	if err != nil {
		return 0, err
	}
	err = x.tx.Delete(id)
	x.graph.reset()
	return 1, fail(s, err)
}

// node resolves one end of a relate or unrelate: an entity, or one of its
// fields.
func (x *executor) node(n lang.NodeRef, row lang.Row) (value.ID, error) {
	id, kind, err := x.target(n.Target, row)
	if err != nil || n.Field == "" {
		return id, err
	}
	if kind != model.NodeEntity {
		return id, errAt(n.Target, "type", "%s is a %s; only entities have fields", lang.FormatExpr(n.Target), kind)
	}
	e, _, err := x.graph.entity(id)
	if err != nil {
		return id, fail(n.Target, err)
	}
	f, ok := e.Field(n.Field)
	if !ok {
		return id, errAt(n.Target, "unknown_field", "%s has no field %s", lang.FormatExpr(n.Target), n.Field)
	}
	return f.ID, nil
}

// mayRelate checks the permissions relate and unrelate need: relate on
// the source's kind and read on the target's (spec/rhizome.md §11).
func (x *executor) mayRelate(n lang.Node, from, to value.ID) error {
	if x.ctx.Access == nil {
		return nil
	}
	fk, err := x.nodeKind(n, from)
	if err != nil {
		return err
	}
	if err := x.need(n, security.Relate, fk); err != nil {
		return err
	}
	tk, err := x.nodeKind(n, to)
	if err != nil {
		return err
	}
	return x.need(n, security.Read, tk)
}

func (x *executor) relate(s *lang.Relate, row lang.Row) (*Result, error) {
	from, err := x.node(s.From, row)
	if err != nil {
		return nil, err
	}
	to, err := x.node(s.To, row)
	if err != nil {
		return nil, err
	}
	if s.Edge.Dir == lang.DirIn {
		from, to = to, from
	}
	if err := x.mayRelate(s, from, to); err != nil {
		return nil, err
	}
	props := engine.Fields{}
	if s.Edge.Props != nil {
		for _, en := range s.Edge.Props.Entries {
			k, err := x.eval(en.Key, frame(row))
			if err != nil {
				return nil, err
			}
			v, err := x.eval(en.Value, frame(row))
			if err != nil {
				return nil, err
			}
			props[k.(value.Text).Go()] = v
		}
	}
	rel, err := x.tx.Relate(from, s.Edge.Types[0], to, props)
	if err != nil {
		return nil, fail(s, err)
	}
	if s.Handle != "" {
		rt, _ := x.tx.Catalog().RelationType(s.Edge.Types[0])
		x.bindHandle(s.Handle, Handle{ID: rel, Node: model.NodeRelation, Kind: rt.ID})
	}
	return &Result{Columns: []Column{{"id", lang.Static{Type: value.TypeID}}}, Rows: [][]any{{rel}}, Affected: 1}, nil
}

func (x *executor) unrelate(s *lang.Unrelate, row lang.Row) (int, error) {
	if s.Target != nil {
		id, n, err := x.target(s.Target, row)
		if err != nil {
			return 0, err
		}
		if n != model.NodeRelation {
			return 0, errAt(s.Target, "type", "%s is a %s, not a relation", lang.FormatExpr(s.Target), n)
		}
		rel, err := x.tx.Relation(id)
		if err != nil {
			return 0, fail(s, err)
		}
		if err := x.mayRelate(s, rel.From, rel.To); err != nil {
			return 0, err
		}
		return 1, fail(s, x.tx.Unrelate(id))
	}
	from, err := x.node(*s.From, row)
	if err != nil {
		return 0, err
	}
	to, err := x.node(*s.To, row)
	if err != nil {
		return 0, err
	}
	if s.Edge.Dir == lang.DirIn {
		from, to = to, from
	}
	if err := x.mayRelate(s, from, to); err != nil {
		return 0, err
	}
	n, err := x.tx.UnrelateAll(from, s.Edge.Types[0], to)
	return n, fail(s, err)
}

func (x *executor) push(s *lang.Push, row lang.Row) (int, error) {
	id, err := x.entityTarget(s.Field.Target, row, security.Write)
	if err != nil {
		return 0, err
	}
	v, err := x.eval(s.Value, frame(row))
	if err != nil {
		return 0, err
	}
	var prio int64
	if s.Priority != nil {
		p, err := x.eval(s.Priority, frame(row))
		if err != nil {
			return 0, err
		}
		n, ok := p.(value.Int)
		if !ok {
			return 0, errAt(s.Priority, "type", "a priority is an int, not %s", p.Code())
		}
		prio = int64(n)
	}
	err = x.tx.Push(id, s.Field.Field, v, prio)
	x.graph.reset()
	return 1, fail(s, err)
}

// pop runs a pop and returns the value, or a list with a count.
func (x *executor) pop(s *lang.Pop, row lang.Row) (value.Value, error) {
	id, err := x.entityTarget(s.Field.Target, row, security.Write)
	if err != nil {
		return nil, err
	}
	defer x.graph.reset()
	if s.Wait != nil {
		if s.Count != nil {
			return nil, errAt(s, "unsupported", "pop with both a count and wait isn't supported")
		}
		w, err := x.eval(s.Wait, frame(row))
		if err != nil {
			return nil, err
		}
		d, _ := w.(value.Duration)
		if x.ctx.Wait == nil {
			return nil, errAt(s, "constraint", "pop … wait isn't allowed inside a transaction")
		}
		v, err := x.ctx.Wait(id, s.Field.Field, time.Duration(d))
		return orNull(v), fail(s, err)
	}
	if s.Count == nil {
		v, err := x.tx.PopOne(id, s.Field.Field)
		return orNull(v), fail(s, err)
	}
	c, err := x.eval(s.Count, frame(row))
	if err != nil {
		return nil, err
	}
	n, ok := c.(value.Int)
	if !ok || n < 0 {
		return nil, errAt(s.Count, "type", "pop needs a count ≥ 0, not %s", c)
	}
	vs, err := x.tx.Pop(id, s.Field.Field, int(n))
	if err != nil {
		return nil, fail(s, err)
	}
	return value.NewList(vs...)
}
