package query

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/value"
)

// prow is a pipeline row: its variables and, after grouping, the rows of
// its group for aggregates.
type prow struct {
	vars  lang.Row
	group []lang.Row
}

func (r prow) frame() *lang.Frame { return &lang.Frame{Row: r.vars, Group: r.group} }

// with returns a copy of r's variables with name bound to v.
func (r prow) with(name string, v value.Value) prow {
	vars := maps.Clone(r.vars)
	if vars == nil {
		vars = lang.Row{}
	}
	vars[name] = v
	return prow{vars: vars, group: r.group}
}

// query runs a pipeline and builds its result.
func (x *executor) query(pl *lang.Pipeline) (*Result, error) {
	rows, _, affected, dataOnly, err := x.pipeline(pl)
	if err != nil {
		return nil, err
	}
	res := &Result{Affected: affected}
	if dataOnly {
		return res, nil
	}
	if max := x.ctx.Settings.MaxRows; max > 0 && len(rows) > max {
		return nil, errAt(pl, "limit", "the result has more than %d rows (max_rows); add a limit step or raise max_rows", max)
	}
	for _, c := range x.info.Columns[pl] {
		if pl.AsOf != nil && (c.Type.Form == lang.FormEntity || c.Type.Form == lang.FormNode || c.Type.Form == lang.FormRelation) {
			return nil, errAt(pl, "unsupported", "an as of query shows fields (like p.name), not whole entities or relations: %s", c.Name)
		}
		res.Columns = append(res.Columns, Column{c.Name, c.Type})
	}
	for _, r := range rows {
		cells := make([]any, len(res.Columns))
		for i, c := range res.Columns {
			cell, err := x.cell(r.vars[c.Name], c.Type)
			if err != nil {
				return nil, fail(pl, err)
			}
			cells[i] = cell
		}
		res.Rows = append(res.Rows, cells)
	}
	return res, nil
}

// pipeline runs a pipeline's source and steps. It returns the rows, the
// current column names, the number of changes made by data steps, and
// whether the pipeline ended with a data step that returns no rows.
func (x *executor) pipeline(pl *lang.Pipeline) ([]prow, []string, int, bool, error) {
	if pl.AsOf != nil {
		restore, err := x.asOf(pl)
		if err != nil {
			return nil, nil, 0, false, err
		}
		defer restore()
	}
	began := time.Now()
	rows, names, consumed, err := x.source(pl)
	if err == nil {
		err = x.hold(pl, rows)
	}
	if err != nil {
		return nil, nil, 0, false, err
	}
	if x.ctx.Trace != nil {
		x.ctx.Trace(nil, len(rows), time.Since(began))
	}
	affected, dataOnly := 0, false
	for i, st := range pl.Steps {
		if i < consumed { // the source applied it
			if x.ctx.Trace != nil {
				x.ctx.Trace(st, len(rows), 0)
			}
			continue
		}
		began = time.Now()
		if x.info.Grouped[st] {
			rows = []prow{collapse(rows)}
			names = nil
		}
		switch st := st.(type) {
		case *lang.Where:
			rows, err = x.filter(rows, st.Cond)
		case *lang.Having:
			rows, err = x.filter(rows, st.Cond)
		case *lang.Follow:
			rows, err = x.follow(st, rows)
			names = appendNew(names, patternNames(st)...)
		case *lang.Let:
			for i, r := range rows {
				var v value.Value
				if v, err = x.eval(st.Value, r.frame()); err != nil {
					break
				}
				rows[i] = r.with(st.Name, v)
			}
			names = appendNew(names, st.Name)
		case *lang.GroupBy:
			rows, err = x.groupBy(st, rows)
			names = nil
			for _, k := range st.Keys {
				names = append(names, k.Name)
			}
		case *lang.Sort:
			err = x.sort(st, rows)
		case *lang.Distinct:
			rows, err = x.distinct(st, rows)
		case *lang.Skip:
			var n int
			if n, err = x.count(st.N); err == nil {
				rows = rows[min(n, len(rows)):]
			}
		case *lang.Limit:
			var n int
			if n, err = x.count(st.N); err == nil {
				rows = rows[:min(n, len(rows))]
			}
		case *lang.Show:
			rows, names, err = x.show(st, rows)
		case *lang.Bind:
			err = x.bind(st, rows)
		case *lang.UnionStep:
			var sub []prow
			var subNames []string
			if sub, subNames, _, _, err = x.pipeline(st.Pipeline); err == nil {
				for _, r := range sub {
					vars := lang.Row{}
					for i, n := range names {
						if i < len(subNames) {
							vars[n] = r.vars[subNames[i]]
						}
					}
					rows = append(rows, prow{vars: vars})
				}
			}
		case *lang.DataStep:
			var n int
			rows, n, dataOnly, err = x.dataStep(st, rows)
			affected += n
			if pop, ok := st.Stmt.(*lang.Pop); ok && pop.As != "" {
				names = appendNew(names, pop.As)
			}
		}
		if err == nil {
			err = x.hold(st, rows)
		}
		if err != nil {
			return nil, nil, 0, false, err
		}
		if x.ctx.Trace != nil {
			x.ctx.Trace(st, len(rows), time.Since(began))
		}
	}
	return rows, names, affected, dataOnly, nil
}

func appendNew(names []string, add ...string) []string {
	for _, n := range add {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// patternNames lists the variables a follow or match step may bind, in
// the order the checker binds them.
func patternNames(st *lang.Follow) []string {
	var out []string
	for _, n := range st.Pattern.Nodes {
		out = append(out, n.Var)
	}
	for _, e := range st.Pattern.Edges {
		out = append(out, e.Var)
	}
	return append(out, st.PathVar)
}

// collapse makes the whole stream one group, for aggregates without a
// group by.
func collapse(rows []prow) prow {
	g := make([]lang.Row, len(rows))
	for i, r := range rows {
		g[i] = r.vars
	}
	return prow{vars: lang.Row{}, group: g}
}

// ===== Source =====

// source produces a pipeline's first rows. It returns the number of
// leading where steps it applied itself.
func (x *executor) source(pl *lang.Pipeline) ([]prow, []string, int, error) {
	src := &pl.Source
	if src.Start != nil {
		id, n, err := x.target(src.Start, nil)
		if err != nil {
			return nil, nil, 0, err
		}
		if n == model.NodeEntity {
			if err := x.needVisible(src.Start, id); err != nil {
				return nil, nil, 0, err
			}
		}
		return []prow{{vars: lang.Row{src.StartAs: nodeValue(id, n)}}}, []string{src.StartAs}, 0, nil
	}
	p, err := x.planSource(pl)
	if err != nil {
		return nil, nil, 0, err
	}
	if p != nil && !x.ctx.NoPlanner {
		rows, err := x.runPlan(pl, p)
		if err == nil {
			err = x.reportPlan(pl, p)
		}
		if err != nil {
			return nil, nil, 0, err
		}
		var names []string
		for _, kv := range src.Vars {
			names = append(names, kv.Var)
		}
		return rows, names, p.consumed, nil
	}
	rows := []prow{{vars: lang.Row{}}}
	var names []string
	for _, kv := range src.Vars {
		var cands []value.Value
		if src.In != nil {
			var err error
			if cands, err = x.inList(src.In, kv.Kind); err != nil {
				return nil, nil, 0, err
			}
		} else {
			var stop error
			filter := false
			if kv.Kind != "" {
				k, _ := x.tx.Catalog().Kind(kv.Kind)
				if err := x.need(pl, security.Read, k); err != nil {
					return nil, nil, 0, err
				}
			} else {
				filter = x.ctx.Access != nil
			}
			entities := x.tx.Entities
			if x.past != nil {
				entities = x.past.Entities
			}
			err := entities(kv.Kind, func(id value.ID) bool {
				if stop = x.tick(pl); stop != nil {
					return false
				}
				if filter {
					var ok bool
					if x.past != nil {
						_, ok, stop = (&pastGraph{a: x.past, g: x.graph}).entity(id)
					} else {
						_, ok, stop = x.graph.entity(id)
					}
					if stop != nil || !ok {
						return stop == nil
					}
				}
				cands = append(cands, value.Ref{Target: id})
				return true
			})
			if stop != nil {
				return nil, nil, 0, stop
			}
			if err != nil {
				return nil, nil, 0, fail(pl, err)
			}
		}
		var next []prow
		for _, r := range rows {
			for _, c := range cands {
				next = append(next, r.with(kv.Var, c))
			}
		}
		rows = next
		names = append(names, kv.Var)
	}
	return rows, names, 0, nil
}

// inList evaluates find … in: entities (as refs) from a collection of
// entities, refs or ids, keeping those of the kind.
func (x *executor) inList(e lang.Expr, kind string) ([]value.Value, error) {
	v, err := x.eval(e, nil)
	if err != nil {
		return nil, err
	}
	var vs []value.Value
	switch c := v.(type) {
	case value.List:
		vs = c.Values()
	case value.Set:
		vs = c.Values()
	case value.Array:
		vs = c.Values()
	default:
		if value.IsNull(v) {
			return nil, nil
		}
		return nil, errAt(e, "type", "find … in needs a list or set, not %s", v.Code())
	}
	var out []value.Value
	seen := map[value.ID]bool{}
	for _, item := range vs {
		var id value.ID
		switch it := item.(type) {
		case value.Ref:
			id = it.Target
		case value.ID:
			id = it
		default:
			if value.IsNull(item) {
				continue
			}
			return nil, errAt(e, "type", "find … in needs entities, refs or ids, not %s", item.Code())
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		var k *model.Kind
		var ok bool
		var err error
		if x.past != nil {
			k, ok, err = (&pastGraph{a: x.past, g: x.graph}).entity(id)
		} else {
			var ent *engine.Entity
			if ent, ok, err = x.graph.entity(id); ok {
				k = ent.Kind
			}
		}
		if err != nil {
			return nil, fail(e, err)
		}
		if ok && (kind == "" || k != nil && k.Name == kind) {
			out = append(out, value.Ref{Target: id})
		}
	}
	return out, nil
}

// ===== Steps =====

func (x *executor) filter(rows []prow, cond lang.Expr) ([]prow, error) {
	var out []prow
	for _, r := range rows {
		if err := x.tick(cond); err != nil {
			return nil, err
		}
		ok, err := x.ev.Cond(cond, r.frame())
		if err != nil {
			return nil, fail(cond, err)
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// rowKey encodes values for grouping and distinct: equal values give
// equal keys, including numbers of different types (1 and 1.0). Large
// text and bytes key by their start and hash, so they aren't loaded.
func rowKey(n lang.Node, vs []value.Value) (string, error) {
	var b strings.Builder
	for _, v := range vs {
		var k []byte
		if value.IsKeyable(v) {
			var err error
			if k, err = value.Key(v); err != nil {
				return "", errAt(n, "internal", "reading a large value: %v", err)
			}
		} else {
			k = value.Encode(v)
		}
		b.WriteString(string(k))
		b.WriteByte(0xff)
	}
	return b.String(), nil
}

func (x *executor) evalAll(es []lang.Expr, r prow) ([]value.Value, error) {
	out := make([]value.Value, len(es))
	for i, e := range es {
		var err error
		if out[i], err = x.eval(e, r.frame()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// evalAllLazy is evalAll leaving large values unread.
func (x *executor) evalAllLazy(es []lang.Expr, r prow) ([]value.Value, error) {
	out := make([]value.Value, len(es))
	for i, e := range es {
		var err error
		if out[i], err = x.evalLazy(e, r.frame()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (x *executor) groupBy(st *lang.GroupBy, rows []prow) ([]prow, error) {
	exprs := make([]lang.Expr, len(st.Keys))
	for i, k := range st.Keys {
		exprs[i] = k.Expr
	}
	var out []prow
	index := map[string]int{}
	for _, r := range rows {
		ks, err := x.evalAllLazy(exprs, r)
		if err != nil {
			return nil, err
		}
		key, err := rowKey(st, ks)
		if err != nil {
			return nil, err
		}
		i, ok := index[key]
		if !ok {
			vars := lang.Row{}
			for j, k := range st.Keys {
				vars[k.Name] = ks[j]
			}
			i = len(out)
			index[key] = i
			out = append(out, prow{vars: vars})
		}
		out[i].group = append(out[i].group, r.vars)
	}
	return out, nil
}

func (x *executor) sort(st *lang.Sort, rows []prow) error {
	keys := make([][]value.Value, len(rows))
	exprs := make([]lang.Expr, len(st.Keys))
	for i, k := range st.Keys {
		exprs[i] = k.Expr
	}
	for i, r := range rows {
		var err error
		if keys[i], err = x.evalAllLazy(exprs, r); err != nil {
			return err
		}
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	// Large keys are compared a piece at a time (lang.Evaluator.Order).
	var failed error
	slices.SortStableFunc(idx, func(a, b int) int {
		for j, k := range st.Keys {
			c, err := x.ev.Order(st, keys[a][j], keys[b][j])
			if err != nil && failed == nil {
				failed = fail(st, err)
			}
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
	if failed != nil {
		return failed
	}
	sorted := make([]prow, len(rows))
	for i, j := range idx {
		sorted[i] = rows[j]
	}
	copy(rows, sorted)
	return nil
}

func (x *executor) distinct(st *lang.Distinct, rows []prow) ([]prow, error) {
	seen := map[string]bool{}
	var out []prow
	for _, r := range rows {
		var vs []value.Value
		if len(st.Exprs) > 0 {
			var err error
			if vs, err = x.evalAllLazy(st.Exprs, r); err != nil {
				return nil, err
			}
		} else {
			for _, n := range slices.Sorted(maps.Keys(r.vars)) {
				vs = append(vs, value.MustText(n), r.vars[n])
			}
		}
		k, err := rowKey(st, vs)
		if err != nil {
			return nil, err
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// count evaluates a skip or limit.
func (x *executor) count(e lang.Expr) (int, error) {
	v, err := x.eval(e, nil)
	if err != nil {
		return 0, err
	}
	n, ok := v.(value.Int)
	if !ok || n < 0 {
		return 0, errAt(e, "type", "skip and limit need an int ≥ 0, not %s", v)
	}
	return int(min(int64(n), 1<<31)), nil
}

func (x *executor) show(st *lang.Show, rows []prow) ([]prow, []string, error) {
	names := make([]string, len(st.Items))
	for i, it := range st.Items {
		names[i] = it.Name
		if names[i] == "" {
			names[i] = lang.FormatExpr(it.Expr)
		}
	}
	out := make([]prow, len(rows))
	for i, r := range rows {
		vars := lang.Row{}
		for j, it := range st.Items {
			v, err := x.evalLazy(it.Expr, r.frame())
			if err != nil {
				return nil, nil, err
			}
			vars[names[j]] = v
		}
		out[i] = prow{vars: vars}
	}
	return out, names, nil
}

func (x *executor) bind(st *lang.Bind, rows []prow) error {
	if len(rows) != 1 {
		return errAt(st, "constraint", "bind needs exactly one row, and there are %d", len(rows))
	}
	v := rows[0].vars[st.Var]
	var id value.ID
	switch v := v.(type) {
	case value.Ref:
		id = v.Target
	case value.ID:
		id = v
	default:
		return errAt(st, "type", "bind saves an entity or relation, not %s", orNull(v).Code())
	}
	n, ok, err := x.tx.Exists(id)
	if err != nil {
		return fail(st, err)
	}
	if !ok {
		return errAt(st, "not_found", "%s no longer exists", st.Var)
	}
	h := Handle{ID: id, Node: n}
	switch n {
	case model.NodeEntity:
		if e, _, err := x.graph.entity(id); err == nil && e.Kind != nil {
			h.Kind = e.Kind.ID
		}
	case model.NodeRelation:
		if rel, err := x.tx.Relation(id); err == nil {
			h.Kind = rel.Type
		}
	}
	x.bindHandle(st.Handle, h)
	return nil
}

// dataStep runs a data change for every row. Only pop … as keeps rows.
func (x *executor) dataStep(st *lang.DataStep, rows []prow) ([]prow, int, bool, error) {
	total := 0
	for i, r := range rows {
		var n int
		var err error
		switch s := st.Stmt.(type) {
		case *lang.Update:
			n, err = x.update(s, r.vars)
		case *lang.Delete:
			n, err = x.delete(s, r.vars)
		case *lang.Unrelate:
			n, err = x.unrelate(s, r.vars)
		case *lang.Push:
			n, err = x.push(s, r.vars)
		case *lang.Pop:
			var v value.Value
			if v, err = x.pop(s, r.vars); err == nil && s.As != "" {
				rows[i] = r.with(s.As, v)
			}
			n = 1
		}
		if err != nil {
			return nil, 0, false, err
		}
		total += n
	}
	if pop, ok := st.Stmt.(*lang.Pop); ok && pop.As != "" {
		return rows, total, false, nil
	}
	return nil, total, true, nil
}

// ===== Patterns =====

func (x *executor) follow(st *lang.Follow, rows []prow) ([]prow, error) {
	var out []prow
	var over error
	for _, r := range rows {
		err := x.matchPattern(st, r, func(nr prow) {
			if over == nil {
				over = x.bindRow(st, len(nr.vars))
			}
			out = append(out, nr)
		})
		if err == nil {
			err = over
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// matchPattern finds every way the pattern matches from row r.
func (x *executor) matchPattern(st *lang.Follow, r prow, emit func(prow)) error {
	pt := st.Pattern
	starts, err := x.startNodes(pt.Nodes[0], r)
	if err != nil {
		return err
	}
	for _, s := range starts {
		path := []value.Value{x.pathNode(s.id)}
		if err := x.walk(st, 0, s.id, s.row, path, emit); err != nil {
			return err
		}
	}
	return nil
}

type start struct {
	id  value.ID
	row prow
}

// nodeOf returns the node a bound value names: the entity, or its field.
func (x *executor) nodeOf(n lang.Node, v value.Value, field string) (value.ID, bool, error) {
	var id value.ID
	switch v := v.(type) {
	case value.Ref:
		id = v.Target
	case value.ID:
		id = v
	default:
		return id, false, nil
	}
	if field == "" {
		return id, true, nil
	}
	e, ok, err := x.graph.entity(id)
	if err != nil || !ok {
		return id, false, fail(n, err)
	}
	f, ok := e.Field(field)
	if !ok {
		return id, false, nil
	}
	return f.ID, true, nil
}

// startNodes returns where a pattern starts: a bound variable, handle or
// ID, or (in match) every entity of the node's kind.
func (x *executor) startNodes(pn *lang.PNode, r prow) ([]start, error) {
	var v value.Value
	switch {
	case pn.Handle != "":
		v, _ = x.handleValue(pn.Handle)
	case pn.ID != nil:
		v = *pn.ID
	default:
		if bound, ok := r.vars[pn.Var]; ok {
			v = bound
			break
		}
		var out []start
		var hidden error
		err := x.tx.Entities(pn.Kind, func(id value.ID) bool {
			var ok bool
			if _, ok, hidden = x.graph.entity(id); hidden != nil {
				return false
			}
			if ok {
				out = append(out, start{id, r.with(pn.Var, value.Ref{Target: id})})
			}
			return true
		})
		if hidden != nil {
			return nil, fail(pn, hidden)
		}
		if err != nil || pn.Field == "" {
			return out, fail(pn, err)
		}
		var fields []start
		for _, s := range out {
			if fid, ok, err := x.nodeOf(pn, value.Ref{Target: s.id}, pn.Field); err != nil {
				return nil, err
			} else if ok {
				fields = append(fields, start{fid, s.row})
			}
		}
		return fields, nil
	}
	id, ok, err := x.nodeOf(pn, v, pn.Field)
	if err != nil || !ok {
		return nil, err
	}
	if owner, isRef := v.(value.Ref); isRef {
		if _, visible, err := x.graph.entity(owner.Target); err != nil || !visible {
			return nil, fail(pn, err)
		}
	}
	return []start{{id, r}}, nil
}

// pathNode is how a node appears in a path value: an entity as a ref,
// a field as its ID.
func (x *executor) pathNode(id value.ID) value.Value {
	if _, ok := x.graph.ents[id]; ok {
		return value.Ref{Target: id}
	}
	if n, ok, _ := x.tx.Exists(id); ok && n == model.NodeEntity {
		return value.Ref{Target: id}
	}
	return id
}

// walk matches edge i onwards from node cur.
func (x *executor) walk(st *lang.Follow, i int, cur value.ID, r prow, path []value.Value, emit func(prow)) error {
	pt := st.Pattern
	if i == len(pt.Edges) {
		if st.PathVar != "" {
			p, err := value.NewList(path...)
			if err != nil {
				return fail(st, err)
			}
			r = r.with(st.PathVar, p)
		}
		emit(r)
		return nil
	}
	e, next := pt.Edges[i], pt.Nodes[i+1]
	props, err := x.edgeProps(e, r)
	if err != nil {
		return err
	}
	return x.hops(e, cur, props, func(end value.ID, seg []value.Value, rel value.ID) error {
		nr, ok, err := x.nodeMatches(next, end, r)
		if err != nil || !ok {
			return err
		}
		if e.Var != "" {
			if old, bound := nr.vars[e.Var]; bound {
				if !value.Equal(old, rel) {
					return nil
				}
			} else {
				nr = nr.with(e.Var, rel)
			}
		}
		return x.walk(st, i+1, end, nr, append(slices.Clone(path), seg...), emit)
	})
}

// edgeProps evaluates an edge's property filter.
func (x *executor) edgeProps(e *lang.Edge, r prow) (map[string]value.Value, error) {
	if e.Props == nil {
		return nil, nil
	}
	out := map[string]value.Value{}
	for _, en := range e.Props.Entries {
		k, err := x.eval(en.Key, r.frame())
		if err != nil {
			return nil, err
		}
		v, err := x.eval(en.Value, r.frame())
		if err != nil {
			return nil, err
		}
		out[k.(value.Text).Go()] = v
	}
	return out, nil
}

// hops calls fn for every node reachable from cur along e, with the path
// segment walked (alternating relation IDs and nodes) and, for a single
// hop, the relation. Multi-hop walks never visit an entity twice.
func (x *executor) hops(e *lang.Edge, cur value.ID, props map[string]value.Value, fn func(end value.ID, seg []value.Value, rel value.ID) error) error {
	lo, hi := 1, 1
	if e.HasDepth {
		lo, hi = e.Min, e.Max
		if hi < 0 {
			hi = x.ctx.Settings.MaxDepth
		}
	}
	visited := map[value.ID]bool{cur: true}
	var step func(node value.ID, depth int, seg []value.Value, last value.ID) error
	step = func(node value.ID, depth int, seg []value.Value, last value.ID) error {
		if depth >= lo {
			if err := fn(node, seg, last); err != nil {
				return err
			}
		}
		if depth == hi {
			return nil
		}
		rels, err := x.edgeRelations(e, node)
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if err := x.tick(e); err != nil {
				return err
			}
			if !propsMatch(rel, props) {
				continue
			}
			other := rel.To
			switch {
			case e.Dir == lang.DirIn:
				other = rel.From
			case e.Dir == lang.DirEither && rel.To == node:
				other = rel.From
			}
			if visited[other] {
				continue
			}
			visited[other] = true
			err := step(other, depth+1, append(slices.Clone(seg), rel.ID, x.pathNode(other)), rel.ID)
			delete(visited, other)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return step(cur, 0, nil, value.ID{})
}

// edgeRelations lists the relations of an edge's types touching node in
// the edge's direction.
func (x *executor) edgeRelations(e *lang.Edge, node value.ID) ([]*model.Relation, error) {
	dir := engine.Out
	switch e.Dir {
	case lang.DirIn:
		dir = engine.In
	case lang.DirEither:
		dir = engine.Both
	}
	types := e.Types
	if e.AnyType {
		types = []string{""}
	}
	var out []*model.Relation
	for _, t := range types {
		rels, err := x.tx.Relations(node, dir, t)
		if err != nil {
			return nil, fail(e, err)
		}
		out = append(out, rels...)
	}
	return out, nil
}

func propsMatch(rel *model.Relation, props map[string]value.Value) bool {
	for k, want := range props {
		got, ok := rel.Props.Get(value.MustText(k))
		if !ok {
			got = value.Null
		}
		if !value.Equal(got, want) {
			return false
		}
	}
	return true
}

// nodeMatches checks that the node reached is one pattern node pn
// accepts, binding pn's variable if it is new.
func (x *executor) nodeMatches(pn *lang.PNode, id value.ID, r prow) (prow, bool, error) {
	var want value.Value
	switch {
	case pn.Handle != "":
		want, _ = x.handleValue(pn.Handle)
	case pn.ID != nil:
		want = *pn.ID
	default:
		if bound, ok := r.vars[pn.Var]; ok {
			want = bound
		}
	}
	if want != nil {
		wid, ok, err := x.nodeOf(pn, want, pn.Field)
		return r, ok && wid == id, err
	}
	owner := id
	if pn.Field != "" {
		f, err := x.tx.FieldByID(id)
		if err != nil || f.Name != pn.Field {
			return r, false, nil // not a field, or another field
		}
		owner = f.Entity
	}
	e, ok, err := x.graph.entity(owner)
	if err != nil || !ok {
		return r, false, fail(pn, err)
	}
	if pn.Kind != "" && (e.Kind == nil || e.Kind.Name != pn.Kind) {
		return r, false, nil
	}
	return r.with(pn.Var, value.Ref{Target: owner}), true, nil
}
