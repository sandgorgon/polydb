package lang

import (
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// pipeline checks a query and returns its output columns.
func (c *checker) pipeline(pl *Pipeline) ([]Column, error) {
	if _, err := c.cat(); err != nil {
		return nil, err
	}
	sc := newScope()
	if err := c.source(pl, sc); err != nil {
		return nil, err
	}
	if pl.AsOf != nil {
		ts := newScope()
		ts.noRows = "as of"
		if err := c.write(pl.AsOf, value.TypeTime, ts); err != nil {
			return nil, err
		}
	}
	var cols []Column // set by show
	ended := ""       // the data step that ended the pipeline
	for _, st := range pl.Steps {
		if ended != "" {
			if _, ok := st.(*Show); !ok || ended != "pop as" || cols != nil {
				return nil, errAt(st.Position(), "syntax", "%s must be the last step of a pipeline", ended)
			}
		}
		var err error
		switch st := st.(type) {
		case *Where:
			err = c.cond(st.Cond, sc, "where")
		case *Follow:
			err = c.follow(st, sc)
		case *Let:
			if _, bound := sc.lookup(st.Name); bound {
				return nil, errAt(st.P, "constraint", "%s is already bound", st.Name)
			}
			var t Static
			if t, err = c.groupable(st, sc, st.Value); err == nil {
				sc.bind(st.Name, t)
			}
		case *GroupBy:
			sc, err = c.groupBy(st, sc)
		case *Having:
			if !sc.grouped {
				return nil, errAt(st.P, "syntax", "having filters groups, so it needs a group by before it; use where to filter rows")
			}
			err = c.cond(st.Cond, sc, "having")
		case *Sort:
			for _, k := range st.Keys {
				var t Static
				if t, err = c.expr(k.Expr, sc); err != nil {
					break
				}
				if t.Form == FormPath {
					err = typeErr(k.Expr.Position(), "paths can't be sorted")
				}
			}
		case *Distinct:
			for _, e := range st.Exprs {
				if _, err = c.expr(e, sc); err != nil {
					break
				}
			}
		case *Skip:
			err = c.count(st.N, "skip")
		case *Limit:
			err = c.count(st.N, "limit")
		case *Show:
			cols, sc, err = c.show(st, sc)
		case *Bind:
			var t Static
			t, ok := sc.lookup(st.Var)
			switch {
			case !ok:
				err = errAt(st.P, "not_found", "no variable %s%s", st.Var, didYouMean(st.Var, sc.order))
			case t.Form == FormValue || t.Form == FormPath:
				err = typeErr(st.P, "bind saves an entity or relation, and %s is %s", st.Var, t)
			}
		case *UnionStep:
			var sub []Column
			if sub, err = c.pipeline(st.Pipeline); err == nil {
				ours := cols
				if ours == nil {
					ours = varColumns(sc)
				}
				if len(sub) != len(ours) {
					err = errAt(st.P, "type", "union needs the same number of columns on both sides: %d here, %d in the union; add a show step", len(ours), len(sub))
				}
			}
		case *DataStep:
			ended, err = c.dataStep(st, sc)
		}
		if err != nil {
			return nil, err
		}
	}
	if cols == nil {
		cols = varColumns(sc)
	}
	c.info.Columns[pl] = cols
	return cols, nil
}

func varColumns(sc *scope) []Column {
	cols := make([]Column, len(sc.order))
	for i, n := range sc.order {
		cols[i] = Column{n, sc.vars[n]}
	}
	return cols
}

func (c *checker) source(pl *Pipeline, sc *scope) error {
	src := &pl.Source
	if src.Start != nil {
		t, err := c.target(src.Start, nil)
		if err != nil {
			return err
		}
		sc.bind(src.StartAs, t)
		return nil
	}
	for _, kv := range src.Vars {
		if _, dup := sc.lookup(kv.Var); dup {
			return errAt(pl.P, "constraint", "variable %s is bound twice", kv.Var)
		}
		t := stEntity
		if kv.Kind != "" {
			k, err := c.kind(pl.P, kv.Kind)
			if err != nil {
				return err
			}
			t.Kind = k
		} else if c.isKindName(kv.Var) {
			return errAt(pl.P, "syntax", "%s is a kind; name a variable for it: find %s x", kv.Var, kv.Var)
		}
		sc.bind(kv.Var, t)
	}
	if src.In != nil {
		if len(src.Vars) != 1 {
			return errAt(src.In.Position(), "syntax", "find … in works with one variable")
		}
		t, err := c.expr(src.In, newScope())
		if err != nil {
			return err
		}
		vt := t.asValue()
		switch {
		case dynamic(vt):
		case vt.Code == value.CodeList || vt.Code == value.CodeSet || vt.Code == value.CodeArray:
			if e := value.Required(vt.Elem); !dynamic(e) && e.Code != value.CodeRef && e.Code != value.CodeID {
				return typeErr(src.In.Position(), "find … in needs a collection of entities, refs or ids, not %s", vt)
			}
		default:
			return typeErr(src.In.Position(), "find … in needs a list or set, not %s", t)
		}
	}
	return nil
}

// count checks a skip or limit: a constant int.
func (c *checker) count(e Expr, what string) error {
	sc := newScope()
	sc.noRows = what
	return c.write(e, value.TypeInt, sc)
}

// groupable checks an expression in a step that may aggregate (let,
// show). Aggregates without a group by before them make the step group
// the whole stream; then plain row values can't be used.
func (c *checker) groupable(st Step, sc *scope, es ...Expr) (Static, error) {
	if !sc.grouped && hasAgg(es...) {
		c.info.Grouped[st] = true
		g := newScope()
		g.grouped, g.pre = true, sc
		var t Static
		for _, e := range es {
			var err error
			if t, err = c.expr(e, g); err != nil {
				return t, err
			}
		}
		// The step reduces the stream to one row.
		*sc = *g
		sc.pre = nil
		return t, nil
	}
	var t Static
	for _, e := range es {
		var err error
		if t, err = c.expr(e, sc); err != nil {
			return t, err
		}
	}
	return t, nil
}

func (c *checker) groupBy(st *GroupBy, sc *scope) (*scope, error) {
	if sc.grouped {
		return nil, errAt(st.P, "syntax", "the rows are already grouped; use one group by with several keys")
	}
	g := newScope()
	g.grouped, g.pre = true, sc
	for _, k := range st.Keys {
		if hasAgg(k.Expr) {
			return nil, errAt(k.Expr.Position(), "syntax", "group by keys can't use aggregates")
		}
		t, err := c.expr(k.Expr, sc)
		if err != nil {
			return nil, err
		}
		if _, dup := g.lookup(k.Name); dup {
			return nil, errAt(st.P, "constraint", "group name %s is used twice", k.Name)
		}
		g.bind(k.Name, t)
	}
	return g, nil
}

// show checks a show step and returns the columns and the scope after it,
// in which the columns are the variables.
func (c *checker) show(st *Show, sc *scope) ([]Column, *scope, error) {
	exprs := make([]Expr, len(st.Items))
	for i, it := range st.Items {
		exprs[i] = it.Expr
	}
	if !sc.grouped && hasAgg(exprs...) {
		if _, err := c.groupable(st, sc, exprs...); err != nil {
			return nil, nil, err
		}
	} else {
		for _, e := range exprs {
			if _, err := c.expr(e, sc); err != nil {
				return nil, nil, err
			}
		}
	}
	out := newScope()
	out.columns = map[string]string{}
	var cols []Column
	for _, it := range st.Items {
		name := it.Name
		if name == "" {
			name = FormatExpr(it.Expr)
		}
		if _, dup := out.lookup(name); dup {
			return nil, nil, errAt(it.Expr.Position(), "constraint", "two columns are named %s; name one with as", name)
		}
		t := c.info.Types[it.Expr]
		out.bind(name, t)
		out.columns[FormatExpr(it.Expr)] = name
		cols = append(cols, Column{name, t})
	}
	return cols, out, nil
}

func (c *checker) dataStep(st *DataStep, sc *scope) (string, error) {
	if sc.grouped {
		return "", errAt(st.P, "syntax", "data steps work on rows, not groups")
	}
	switch s := st.Stmt.(type) {
	case *Update:
		return "update", c.update(s, sc)
	case *Delete:
		return "delete", c.stmt(s, sc)
	case *Unrelate:
		return "unrelate", c.unrelate(s, sc)
	case *Push:
		return "push", c.push(s, sc)
	case *Pop:
		t, err := c.pop(s, sc)
		if err != nil || s.As == "" {
			return "pop", err
		}
		if _, bound := sc.lookup(s.As); bound {
			return "", errAt(s.P, "constraint", "%s is already bound", s.As)
		}
		sc.bind(s.As, t)
		return "pop as", nil
	}
	return "", errAt(st.P, "syntax", "unexpected data step")
}

// ===== Patterns =====

func (c *checker) follow(st *Follow, sc *scope) error {
	verb := "follow"
	if st.Match {
		verb = "match"
	}
	if sc.grouped {
		return errAt(st.P, "syntax", "%s works on rows, not groups", verb)
	}
	pt := st.Pattern
	shared := false
	for i, n := range pt.Nodes {
		bound, err := c.pnode(n, sc, i == 0, st.Match, verb)
		if err != nil {
			return err
		}
		shared = shared || bound
	}
	if st.Match && !shared {
		c.warn(st.P, "match shares no variable with the rows before it, so it makes a cross product")
	}
	for _, e := range pt.Edges {
		if err := c.edge(e, sc, st.Match); err != nil {
			return err
		}
	}
	if st.PathVar != "" {
		if _, dup := sc.lookup(st.PathVar); dup {
			return errAt(st.P, "constraint", "%s is already bound", st.PathVar)
		}
		sc.bind(st.PathVar, Static{Form: FormPath})
	}
	return nil
}

// pnode checks a pattern node, binding a new variable, and reports
// whether it refers to something already bound (a variable, handle or
// ID).
func (c *checker) pnode(n *PNode, sc *scope, first, match bool, verb string) (bool, error) {
	switch {
	case n.Handle != "":
		t, err := c.handle(&HandleRef{n.At, n.Handle})
		if err == nil && n.Field != "" {
			_, err = fieldOf(n.P, t.Kind, n.Field, nil)
		}
		return true, err
	case n.ID != nil:
		return true, nil
	}
	t, bound := sc.lookup(n.Var)
	if bound {
		if n.Kind != "" {
			return true, errAt(n.P, "syntax", "%s is already bound, so it can't take a kind here; write %s", n.Var, n.Var)
		}
		if t.Form != FormEntity && t.Form != FormNode {
			return true, typeErr(n.P, "%s is %s, not an entity", n.Var, t)
		}
		if !first && !match {
			return true, errAt(n.P, "syntax", "follow can only add new variables after its start; use match to connect %s", n.Var)
		}
		if n.Field != "" {
			_, err := fieldOf(n.P, t.Kind, n.Field, nil)
			return true, err
		}
		return true, nil
	}
	if first && !match {
		return false, errAt(n.P, "syntax", "follow must start at a bound variable or a handle, and %s isn't bound%s", n.Var, didYouMean(n.Var, sc.order))
	}
	t = stEntity
	if n.Kind != "" {
		k, err := c.kind(n.P, n.Kind)
		if err != nil {
			return false, err
		}
		t.Kind = k
	} else if c.isKindName(n.Var) {
		return false, errAt(n.P, "syntax", "%s is a kind; name a variable for it: %s x", n.Var, n.Var)
	}
	if n.Field != "" {
		if _, err := fieldOf(n.P, t.Kind, n.Field, nil); err != nil {
			return false, err
		}
	}
	sc.bind(n.Var, t)
	return false, nil
}

func (c *checker) edge(e *Edge, sc *scope, match bool) error {
	var rt *model.RelationType
	for _, name := range e.Types {
		r, ok := c.env.Catalog.RelationType(name)
		if !ok {
			var names []string
			for _, r := range c.env.Catalog.RelationTypes() {
				names = append(names, r.Name)
			}
			c.warn(e.P, "no relation of type %s has been created yet%s", name, didYouMean(name, names))
		}
		if len(e.Types) == 1 && ok {
			rt = r
		}
	}
	if e.HasDepth && (e.Min < 1 || e.Max != -1 && e.Max < e.Min) {
		return errAt(e.P, "syntax", "a depth needs 1 ≤ min ≤ max, as in *1..3")
	}
	if e.Var != "" {
		if e.HasDepth {
			return errAt(e.P, "syntax", "a multi-hop step can't bind relation variable %s; bind the whole path with %s path", e.Var, map[bool]string{true: "match", false: "follow"}[match])
		}
		t := Static{Form: FormRelation}
		if rt != nil && rt.Defined {
			t.Rel = rt
		}
		if old, bound := sc.lookup(e.Var); bound {
			if !match || old.Form != FormRelation {
				return errAt(e.P, "constraint", "%s is already bound", e.Var)
			}
		} else {
			sc.bind(e.Var, t)
		}
	}
	return c.relProps(e, rt, sc, false)
}

// ===== Helpers =====

// Walk calls fn for e and its subexpressions, depth first, descending
// into an expression's parts only while fn returns true for it.
func Walk(e Expr, fn func(Expr) bool) { walk(e, fn) }

// walk calls fn for e and its subexpressions, depth first, while fn
// returns true.
func walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch e := e.(type) {
	case *Unary:
		walk(e.X, fn)
	case *Binary:
		walk(e.X, fn)
		walk(e.Y, fn)
	case *Is:
		walk(e.X, fn)
	case *Between:
		walk(e.X, fn)
		walk(e.Lo, fn)
		walk(e.Hi, fn)
	case *FieldExpr:
		walk(e.X, fn)
	case *MetaExpr:
		walk(e.X, fn)
	case *IndexExpr:
		walk(e.X, fn)
		walk(e.Index, fn)
	case *DocPath:
		walk(e.X, fn)
		walk(e.Step.Index, fn)
	case *Call:
		for _, a := range e.Args {
			walk(a, fn)
		}
	case *If:
		walk(e.Cond, fn)
		walk(e.Then, fn)
		walk(e.Else, fn)
	case *Cast:
		walk(e.X, fn)
	case *Exists:
		walk(e.X, fn)
	case *ListLit:
		for _, x := range e.Elems {
			walk(x, fn)
		}
	case *SetLit:
		for _, x := range e.Elems {
			walk(x, fn)
		}
	case *MapLit:
		for _, en := range e.Entries {
			walk(en.Key, fn)
			walk(en.Value, fn)
		}
	}
}

// hasAgg reports whether any of es calls an aggregate.
func hasAgg(es ...Expr) bool {
	found := false
	for _, e := range es {
		walk(e, func(x Expr) bool {
			if call, ok := x.(*Call); ok && isAggCall(call) {
				found = true
			}
			return !found
		})
	}
	return found
}
