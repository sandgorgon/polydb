package query

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/fulltext"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/value"
)

// The planner (spec/rhizome.md §12) decides how a pipeline's source finds
// its rows: for each variable, whether to scan its kind, look entities up
// by ID, read a value index or ask a text index, and in which order to
// bind the variables.
//
// A plan never changes a result. The rows are the ones the plain plan
// gives (scan each variable's kind in ID order, form every combination,
// run the where steps), in the same order. The where steps right after
// the source are split into the conditions joined by and. Each condition
// is checked as soon as its variables are bound, and an index may answer
// one of them per variable. A condition that fails to evaluate early
// doesn't drop the row: the where step runs whole on it later, so its
// error comes out as before. A row that an index or an early condition
// rules out never reaches the other conditions, though, so an error they
// would have raised on it doesn't happen.
//
// Estimates come from counting probes: a constant index range is counted
// up to probeCap entries. Ranges that depend on another variable are
// guessed. Stored statistics may replace both later.

// probeCap is where counting probes stop. Tests lower it.
var probeCap = 1000

// maxProbes is the most index ranges one lookup may read (from in lists
// and the two encodings of an entity reference); more fall back to a scan.
const maxProbes = 256

// A plan is how a pipeline's source runs.
type plan struct {
	consumed int      // the leading where steps the source applies
	levels   []*level // the variables, in the order they're bound
	permuted bool     // the levels aren't in the source's order
	top      *topN    // read in index order and stop early
}

// A level binds one variable.
type level struct {
	kv    lang.KindVar
	pos   int         // the variable's position in the source
	kind  *model.Kind // nil for a kindless variable
	path  *path
	conds []lang.Expr // checked once the variable is bound
	rows  int         // rows bound so far, for explain analyze
}

type pathKind uint8

const (
	pathScan  pathKind = iota // every entity of the kind
	pathID                    // v = expr, v in [exprs]
	pathIndex                 // a range of an ordered index
	pathText                  // a text index answers search()
)

// A path is how a level finds its variable's entities.
type path struct {
	how  pathKind
	cond lang.Expr // the condition it answers
	est  int       // estimated entities, per row of the earlier levels
	dep  bool      // its probes read earlier variables

	val  lang.Expr // pathID: the entity or entities; pathText: the query
	list bool      // pathID: val is a collection (in)

	acc *engine.Access // pathIndex
	ix  ixProbe

	text  *model.Index // pathText
	field string

	fixed  bool       // the probes are constant and worked out once
	ranges []keyRange // pathIndex, when fixed
	ids    []value.ID // pathID and pathText, when fixed
}

// ixProbe is what an index lookup evaluates: the values of its first
// keys, then a range or prefix on the next one. Checks are the other
// conditions on later keys; an entity passes them only if those keys
// have values that are in the index.
type ixProbe struct {
	eqs    []probe
	lo, hi *probe
	prefix lang.Expr
	checks []probe
}

type probe struct {
	e    lang.Expr
	list bool // in: one value per element of a collection
	incl bool // a range bound that includes its value
}

type keyRange struct{ lo, hi []byte }

// topN reads a single variable in index order, for sort … | limit n,
// and stops once it has the rows the limit keeps.
type topN struct {
	acc  *engine.Access
	rng  keyRange
	desc bool
	n    int
	sort lang.Expr // the first sort key
	// partial: entities without a value for the sort key aren't in the
	// index. They sort last (descending), so they matter only when the
	// index runs out first; the level then scans instead.
	partial bool
}

// A cond is one condition of the leading where steps, with the source
// variables it reads.
type cond struct {
	e    lang.Expr
	vars []string
}

// A fact is what one condition says about one variable: key op val,
// where key reads the variable and val reads only variables bound
// earlier.
type fact struct {
	op   string // = in < <= > >= prefix search
	key  lang.Expr
	val  lang.Expr
	cond lang.Expr
}

type planner struct {
	x      *executor
	pl     *lang.Pipeline
	conds  []*cond
	counts map[string]count

	kindStats map[value.ID]*engine.KindStats
}

// A count is how many entities a kind has, up to some limit.
type count struct {
	n     int
	exact bool
}

// planSource plans a pipeline's source, or returns nil when the source
// isn't a find over kinds, or the pipeline reads the past.
func (x *executor) planSource(pl *lang.Pipeline) (*plan, error) {
	src := &pl.Source
	if x.past != nil || src.Start != nil || src.In != nil || len(src.Vars) == 0 {
		return nil, nil
	}
	pc := &planner{x: x, pl: pl, counts: map[string]count{}, kindStats: map[value.ID]*engine.KindStats{}}
	p := &plan{}
	for _, st := range pl.Steps {
		w, ok := st.(*lang.Where)
		if !ok || x.info.Grouped[st] {
			break
		}
		p.consumed++
		for _, c := range conjuncts(w.Cond, nil) {
			pc.conds = append(pc.conds, &cond{e: c, vars: pc.deps(c)})
		}
	}
	var left []int
	for i := range src.Vars {
		left = append(left, i)
	}
	bound := map[string]bool{}
	for len(left) > 0 {
		var best *level
		at := 0
		for j, i := range left {
			lv, err := pc.level(i, bound)
			if err != nil {
				return nil, err
			}
			if best == nil || lv.path.est < best.path.est {
				best, at = lv, j
			}
		}
		if best.pos != len(p.levels) {
			p.permuted = true
		}
		p.levels = append(p.levels, best)
		bound[best.kv.Var] = true
		left = slices.Delete(left, at, at+1)
	}
	at := map[string]int{}
	for i, lv := range p.levels {
		at[lv.kv.Var] = i
	}
	for _, c := range pc.conds {
		i := 0
		for _, v := range c.vars {
			i = max(i, at[v])
		}
		p.levels[i].conds = append(p.levels[i].conds, c.e)
	}
	if len(p.levels) == 1 {
		if err := pc.topN(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// conjuncts splits a condition into the parts joined by and.
func conjuncts(e lang.Expr, out []lang.Expr) []lang.Expr {
	if b, ok := e.(*lang.Binary); ok && b.Op == "and" {
		return conjuncts(b.Y, conjuncts(b.X, out))
	}
	return append(out, e)
}

// deps lists the source variables an expression reads. An expression that
// reads anything else of the row depends on every variable.
func (pc *planner) deps(e lang.Expr) []string {
	var vars []string
	all := false
	lang.Walk(e, func(x lang.Expr) bool {
		switch x := x.(type) {
		case *lang.NameRef:
			r := pc.x.info.Refs[x]
			if r.Kind != lang.RefVar || !pc.isSourceVar(r.Name) {
				all = true
			} else if !slices.Contains(vars, r.Name) {
				vars = append(vars, r.Name)
			}
		case *lang.MetaRef:
			all = true
		}
		return true
	})
	if all {
		vars = nil
		for _, kv := range pc.pl.Source.Vars {
			vars = append(vars, kv.Var)
		}
	}
	return vars
}

func (pc *planner) isSourceVar(name string) bool {
	for _, kv := range pc.pl.Source.Vars {
		if kv.Var == name {
			return true
		}
	}
	return false
}

// isVar reports whether e is the variable v.
func (pc *planner) isVar(e lang.Expr, v string) bool {
	n, ok := e.(*lang.NameRef)
	return ok && pc.x.info.Refs[n] == lang.Ref{Kind: lang.RefVar, Name: v}
}

// keyExpr reports whether e reads index key k of variable v: v.field,
// then the key's path steps.
func (pc *planner) keyExpr(e lang.Expr, v string, k model.IndexKey) bool {
	for i := len(k.Path) - 1; i >= 0; i-- {
		d, ok := e.(*lang.DocPath)
		if !ok {
			return false
		}
		st := k.Path[i]
		if d.Step.Attr != st.Attr {
			return false
		}
		if st.IsPos {
			lit, ok := d.Step.Index.(*lang.Lit)
			if !ok {
				return false
			}
			if n, ok := lit.Val.(value.Int); !ok || int64(n) != int64(st.Pos) {
				return false
			}
		} else if d.Step.Index != nil || d.Step.Name != st.Name {
			return false
		}
		e = d.X
	}
	f, ok := e.(*lang.FieldExpr)
	return ok && f.Name == k.Field && pc.isVar(f.X, v)
}

// facts lists what the conditions available at this level say about v.
func (pc *planner) facts(v string, bound map[string]bool) []fact {
	usable := func(e lang.Expr) bool {
		for _, d := range pc.deps(e) {
			if !bound[d] {
				return false
			}
		}
		return true
	}
	flip := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}
	var out []fact
	for _, c := range pc.conds {
		if !slices.Contains(c.vars, v) {
			continue
		}
		switch e := c.e.(type) {
		case *lang.Binary:
			switch e.Op {
			case "=", "<", "<=", ">", ">=":
				if usable(e.Y) {
					out = append(out, fact{e.Op, e.X, e.Y, e})
				}
				if usable(e.X) {
					op := e.Op
					if f, ok := flip[op]; ok {
						op = f
					}
					out = append(out, fact{op, e.Y, e.X, e})
				}
			case "in", "starts with":
				if usable(e.Y) {
					op := e.Op
					if op == "starts with" {
						op = "prefix"
					}
					out = append(out, fact{op, e.X, e.Y, e})
				}
			}
		case *lang.Between:
			if !e.Not && usable(e.Lo) && usable(e.Hi) {
				out = append(out, fact{">=", e.X, e.Lo, e}, fact{"<=", e.X, e.Hi, e})
			}
		case *lang.Call:
			if e.Name == "search" && len(e.Args) == 2 && usable(e.Args[1]) {
				out = append(out, fact{"search", e.Args[0], e.Args[1], e})
			}
		}
	}
	return out
}

// size counts a kind's entities up to limit: estimates only need to know
// whether a scan is larger than the alternatives.
func (pc *planner) size(kind string, limit int) (int, error) {
	c, ok := pc.counts[kind]
	if ok && (c.exact || c.n >= limit) {
		return min(c.n, limit), nil
	}
	n, err := pc.x.tx.CountEntitiesUpTo(kind, limit)
	if err != nil {
		return 0, fail(pc.pl, err)
	}
	pc.counts[kind] = count{n, n < limit}
	return n, nil
}

// sizeCap bounds the counts behind estimates that depend on a kind's
// size. Larger kinds count as this size.
func sizeCap() int { return 10 * probeCap }

// level chooses how to find source variable i once the bound variables
// are: the path with the fewest estimated entities.
func (pc *planner) level(i int, bound map[string]bool) (*level, error) {
	kv := pc.pl.Source.Vars[i]
	lv := &level{kv: kv, pos: i, path: &path{how: pathScan}}
	if kv.Kind != "" {
		lv.kind, _ = pc.x.tx.Catalog().Kind(kv.Kind)
	}
	facts := pc.facts(kv.Var, bound)
	single := len(pc.pl.Source.Vars) == 1
	if len(facts) == 0 && single {
		return lv, nil // nothing to choose
	}
	paths, err := pc.paths(lv, facts)
	if err != nil {
		return nil, err
	}
	var best *path
	for _, p := range paths {
		if best == nil || p.est < best.est || p.est == best.est && narrower(p, best) {
			best = p
		}
	}
	// A scan is only worth counting as far as the best alternative, or,
	// to order several variables, up to sizeCap.
	limit := sizeCap()
	if best != nil && (single || best.est < limit) {
		limit = best.est + 1
	}
	n, err := pc.size(kv.Kind, limit)
	if err != nil {
		return nil, err
	}
	lv.path.est = n
	if best != nil && best.est < n {
		lv.path = best
	}
	return lv, nil
}

// stats returns a kind's statistics, read once per plan.
func (pc *planner) stats(k *model.Kind) (*engine.KindStats, error) {
	if st, ok := pc.kindStats[k.ID]; ok {
		return st, nil
	}
	st, err := pc.x.tx.KindStats(k)
	if err != nil {
		return nil, fail(pc.pl, err)
	}
	pc.kindStats[k.ID] = st
	return st, nil
}

// perValue estimates from statistics how many entities an index lookup
// with values for its first keys finds: the kind's entities over each
// key's distinct values, assuming independent keys.
func (pc *planner) perValue(lv *level, a *engine.Access, keys int) (int, bool) {
	st, err := pc.stats(lv.kind)
	if err != nil || st == nil {
		return 0, false
	}
	n, err := pc.size(lv.kv.Kind, sizeCap())
	if err != nil {
		return 0, false
	}
	est := float64(max(st.Entities, n))
	for _, k := range a.Keys[:keys] {
		c := st.Column(k)
		if c == nil || c.Distinct == 0 {
			return 0, false
		}
		est /= float64(c.Distinct)
	}
	return max(1, int(est+0.5)), true
}

// narrower prefers, between equal estimates, an index with fewer keys.
func narrower(a, b *path) bool {
	return a.how == pathIndex && b.how == pathIndex && len(a.acc.Keys) < len(b.acc.Keys)
}

// paths lists the ways the facts let a level find its entities.
func (pc *planner) paths(lv *level, facts []fact) ([]*path, error) {
	x, v := pc.x, lv.kv.Var
	var out []*path
	size := func() (int, error) { return pc.size(lv.kv.Kind, sizeCap()) }
	constant := func(es ...lang.Expr) bool {
		for _, e := range es {
			if e != nil && len(pc.deps(e)) > 0 {
				return false
			}
		}
		return true
	}
	for _, f := range facts {
		if (f.op == "=" || f.op == "in") && pc.isVar(f.key, v) {
			p := &path{how: pathID, cond: f.cond, val: f.val, list: f.op == "in", est: 1}
			if p.list {
				p.est = 10
			}
			if constant(f.val) {
				ids, ok := x.entityProbe(p, nil)
				if !ok {
					continue
				}
				p.fixed, p.ids, p.est = true, ids, len(ids)
			} else {
				p.dep = true
			}
			out = append(out, p)
		}
	}
	if lv.kind == nil {
		return out, nil
	}
	texts, err := x.tx.TextIndexes(lv.kind)
	if err != nil {
		return nil, fail(pc.pl, err)
	}
	for _, f := range facts {
		if f.op != "search" {
			continue
		}
		for _, ix := range texts {
			k := ix.Keys[0]
			if len(k.Path) > 0 || !pc.keyExpr(f.key, v, k) {
				continue
			}
			p := &path{how: pathText, cond: f.cond, val: f.val, text: ix, field: k.Field}
			if constant(f.val) {
				ids, ok, err := x.textProbe(p, nil)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				p.fixed, p.ids, p.est = true, ids, len(ids)
			} else {
				n, err := size()
				if err != nil {
					return nil, err
				}
				p.dep, p.est = true, n/3+1
			}
			out = append(out, p)
		}
	}
	accs, err := x.tx.Accesses(lv.kind)
	if err != nil {
		return nil, fail(pc.pl, err)
	}
	for _, a := range accs {
		ip, conds, ok := pc.ixProbe(lv, a, facts, -1, false)
		if !ok {
			continue
		}
		p := &path{how: pathIndex, cond: conds[0], acc: a, ix: ip}
		all := []lang.Expr{ip.prefix}
		for _, pr := range slices.Concat(ip.eqs, ip.checks) {
			all = append(all, pr.e)
		}
		if ip.lo != nil {
			all = append(all, ip.lo.e)
		}
		if ip.hi != nil {
			all = append(all, ip.hi.e)
		}
		if !constant(all...) {
			n, err := size()
			if err != nil {
				return nil, err
			}
			p.dep = true
			p.est = n/3 + 1
			if ip.lo == nil && ip.hi == nil && ip.prefix == nil {
				p.est = min(n, 10)
				if a.Unique && len(ip.eqs) == len(a.Keys) {
					p.est = 1
				} else if est, ok := pc.perValue(lv, a, len(ip.eqs)); ok {
					p.est = est
				}
			}
			out = append(out, p)
			continue
		}
		pk, ok := x.probeKeys(ip, nil)
		if !ok {
			continue
		}
		rs, ok := pk.ranges()
		if !ok {
			continue
		}
		c, err := x.countRanges(a, rs)
		if err != nil {
			return nil, fail(pc.pl, err)
		}
		p.fixed, p.ranges, p.est = true, rs, c
		if c >= probeCap {
			// Too many to count: estimate from statistics, or guess.
			n, err := size()
			if err != nil {
				return nil, err
			}
			p.est = max(probeCap, n/2)
			st, err := pc.stats(lv.kind)
			if err != nil {
				return nil, err
			}
			if frac, ok := pk.fraction(st, a); ok {
				p.est = max(probeCap, int(frac*float64(max(st.Entities, n))))
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// required reports whether every entity of the kind has a value for an
// index key that is in the index: a declared field that isn't optional,
// of a scalar type.
func required(k *model.Kind, key model.IndexKey) bool {
	if len(key.Path) > 0 {
		return false
	}
	fd, ok := k.Field(key.Field)
	return ok && fd.Type != nil && !fd.Type.Nullable() && scalarCode(fd.Type.Code)
}

// scalarKey reports whether an index key is a declared field of a scalar
// type, so that every value it has is in the index.
func scalarKey(k *model.Kind, key model.IndexKey) bool {
	if len(key.Path) > 0 {
		return false
	}
	fd, ok := k.Field(key.Field)
	return ok && fd.Type != nil && scalarCode(fd.Type.Code)
}

func scalarCode(c value.Code) bool {
	switch c {
	case value.CodeBool, value.CodeInt, value.CodeBigInt, value.CodeFloat, value.CodeDecimal,
		value.CodeText, value.CodeBytes, value.CodeTime, value.CodeDate, value.CodeDuration,
		value.CodeID, value.CodeRef:
		return true
	}
	return false
}

// ixProbe works out how the facts constrain an ordered index: values for
// its first keys, then a range on the next. Every later key must be
// constrained too, or always have a value, for the entities the facts let
// through to be in the index. With sortKey ≥ 0, keys before it must have
// a single value and the range must be on it, so the index reads in its
// order. It returns the conditions it answers.
func (pc *planner) ixProbe(lv *level, a *engine.Access, facts []fact, sortKey int, partial bool) (ixProbe, []lang.Expr, bool) {
	type keyFacts struct {
		eq, lo, hi, prefix *fact
		all                []fact
	}
	ks := make([]keyFacts, len(a.Keys))
	for _, f := range facts {
		for i, k := range a.Keys {
			if !pc.keyExpr(f.key, lv.kv.Var, k) {
				continue
			}
			f := f
			kf := &ks[i]
			switch f.op {
			case "=", "in":
				if kf.eq == nil {
					kf.eq = &f
				}
			case ">", ">=":
				if kf.lo == nil {
					kf.lo = &f
				}
			case "<", "<=":
				if kf.hi == nil {
					kf.hi = &f
				}
			case "prefix":
				if kf.prefix == nil {
					kf.prefix = &f
				}
			default:
				continue
			}
			kf.all = append(kf.all, f)
		}
	}
	var ip ixProbe
	var conds []lang.Expr
	j := 0
	stopAt := len(a.Keys)
	if sortKey >= 0 {
		stopAt = sortKey
	}
	for j < stopAt && ks[j].eq != nil {
		if sortKey >= 0 && ks[j].eq.op != "=" {
			return ixProbe{}, nil, false
		}
		ip.eqs = append(ip.eqs, probe{e: ks[j].eq.val, list: ks[j].eq.op == "in"})
		conds = append(conds, ks[j].eq.cond)
		j++
	}
	if sortKey >= 0 && j != sortKey {
		return ixProbe{}, nil, false
	}
	next := j
	if j < len(a.Keys) {
		kf := ks[j]
		switch {
		case kf.lo != nil || kf.hi != nil:
			if kf.lo != nil {
				ip.lo = &probe{e: kf.lo.val, incl: kf.lo.op == ">="}
				conds = append(conds, kf.lo.cond)
			}
			if kf.hi != nil {
				ip.hi = &probe{e: kf.hi.val, incl: kf.hi.op == "<="}
				conds = append(conds, kf.hi.cond)
			}
			next = j + 1
		case kf.prefix != nil:
			ip.prefix = kf.prefix.val
			conds = append(conds, kf.prefix.cond)
			next = j + 1
		case sortKey >= 0:
			if !required(lv.kind, a.Keys[j]) && !(partial && scalarKey(lv.kind, a.Keys[j])) {
				return ixProbe{}, nil, false
			}
			next = j + 1
		}
	}
	if next == 0 {
		return ixProbe{}, nil, false
	}
	// The facts not used for the range check that their keys have
	// values in the index.
	for i := range a.Keys {
		for _, f := range ks[i].all {
			if slices.Contains(conds, f.cond) && i < next {
				continue
			}
			ip.checks = append(ip.checks, probe{e: f.val, list: f.op == "in"})
		}
		if i >= next && len(ks[i].all) == 0 && !required(lv.kind, a.Keys[i]) {
			return ixProbe{}, nil, false
		}
	}
	return ip, conds, true
}

// topN uses index order for sort … [skip m] limit n right after the
// where steps, when the variable's best path isn't already small.
func (pc *planner) topN(p *plan) error {
	x, lv := pc.x, p.levels[0]
	if lv.kind == nil || lv.path.how != pathScan && lv.path.est < probeCap {
		return nil
	}
	steps := pc.pl.Steps[p.consumed:]
	if len(steps) < 2 {
		return nil
	}
	srt, ok := steps[0].(*lang.Sort)
	if !ok || x.info.Grouped[srt] {
		return nil
	}
	n := 0
	steps = steps[1:]
	if sk, ok := steps[0].(*lang.Skip); ok {
		m, err := x.count(sk.N)
		if err != nil {
			return nil
		}
		n, steps = m, steps[1:]
	}
	if len(steps) == 0 {
		return nil
	}
	lim, ok := steps[0].(*lang.Limit)
	if !ok {
		return nil
	}
	m, err := x.count(lim.N)
	if err != nil {
		return nil
	}
	n += m
	key := srt.Keys[0]
	facts := pc.facts(lv.kv.Var, map[string]bool{})
	accs, err := x.tx.Accesses(lv.kind)
	if err != nil {
		return fail(pc.pl, err)
	}
	for _, a := range accs {
		for j, k := range a.Keys {
			if !pc.keyExpr(key.Expr, lv.kv.Var, k) {
				continue
			}
			partial := false
			ip, _, ok := pc.ixProbe(lv, a, facts, j, false)
			if !ok && key.Desc {
				ip, _, ok = pc.ixProbe(lv, a, facts, j, true)
				partial = true
			}
			if !ok {
				continue
			}
			rs, ok := x.indexProbe(ip, nil)
			if !ok || len(rs) != 1 {
				continue
			}
			if p.top == nil || len(a.Keys) < len(p.top.acc.Keys) {
				p.top = &topN{acc: a, rng: rs[0], desc: key.Desc, n: n, sort: key.Expr, partial: partial}
			}
		}
	}
	return nil
}

// ===== Probes =====

// entityProbe evaluates an ID path: the entities v = val or v in val
// names. ok is false when val can't be evaluated.
func (x *executor) entityProbe(p *path, f *lang.Frame) ([]value.ID, bool) {
	v, err := x.ev.Eval(p.val, f)
	if err != nil {
		return nil, false
	}
	vs := []value.Value{v}
	if p.list {
		if vs, ok := elements(v); ok {
			return idsOf(vs), true
		}
		return nil, false
	}
	return idsOf(vs), true
}

func idsOf(vs []value.Value) []value.ID {
	var out []value.ID
	for _, v := range vs {
		switch v := v.(type) {
		case value.Ref:
			out = append(out, v.Target)
		case value.ID:
			out = append(out, v)
		}
	}
	return out
}

// elements returns a list's, array's or set's values.
func elements(v value.Value) ([]value.Value, bool) {
	switch c := v.(type) {
	case value.List:
		return c.Values(), true
	case value.Set:
		return c.Values(), true
	case value.Array:
		return c.Values(), true
	}
	return nil, false
}

// textProbe evaluates a text path's query and asks the index.
func (x *executor) textProbe(p *path, f *lang.Frame) ([]value.ID, bool, error) {
	v, err := x.ev.Eval(p.val, f)
	if err != nil {
		return nil, false, nil
	}
	t, ok := v.(value.Text)
	if !ok {
		return nil, false, nil
	}
	q, err := fulltext.Parse(t.Go())
	if err != nil {
		return nil, false, nil
	}
	ids, err := x.tx.TextSearch(p.text, p.field, q)
	if err != nil {
		return nil, false, fail(p.cond, err)
	}
	return ids, true, nil
}

// eqKeys returns the index keys equal to v: one, or two for an entity
// (the field may hold its ID or a reference). ok is false for null and
// for values whose equals might not be in an index.
func eqKeys(v value.Value) ([][]byte, bool) {
	var id value.ID
	switch t := v.(type) {
	case value.ID:
		id = t
	case value.Ref:
		id = t.Target
	default:
		if value.IsNull(v) || !scalarCode(v.Code()) {
			return nil, false
		}
		k, err := value.Key(v)
		return [][]byte{k}, err == nil
	}
	a, _ := value.Key(id)
	b, _ := value.Key(value.Ref{Target: id})
	return [][]byte{a, b}, true
}

// rangeKey returns a range bound's key, for values that compare only
// with their own group. Text and bytes longer than value.KeyPrefixLen
// can't bound a range: keys of long values sharing its start order by
// hash, not content.
func rangeKey(v value.Value) ([]byte, bool) {
	if value.IsNull(v) || !scalarCode(v.Code()) || v.Code() == value.CodeID || v.Code() == value.CodeRef || longValue(v) {
		return nil, false
	}
	k, err := value.Key(v)
	return k, err == nil
}

// longValue reports whether v is text or bytes whose key holds only its
// start and hash.
func longValue(v value.Value) bool {
	if l, ok := v.(value.Lazy); ok {
		return l.Len() > value.KeyPrefixLen
	}
	return value.LargeLen(v) > value.KeyPrefixLen
}

// probeKeys are an index lookup's evaluated values, as keys: the
// possible keys of each leading key (several for in, or an entity's two
// encodings), then a range or prefix on the next one.
type probeKeys struct {
	eqs            [][][]byte
	lo, hi, prefix []byte
	loIncl, hiIncl bool
}

// indexProbe evaluates an index lookup into key ranges. ok is false when
// a value can't be evaluated or an entity matching it might not be in the
// index; the level then scans instead.
func (x *executor) indexProbe(ip ixProbe, f *lang.Frame) ([]keyRange, bool) {
	pk, ok := x.probeKeys(ip, f)
	if !ok {
		return nil, false
	}
	return pk.ranges()
}

func (x *executor) probeKeys(ip ixProbe, f *lang.Frame) (probeKeys, bool) {
	var pk probeKeys
	eval := func(pr probe) ([]value.Value, bool) {
		v, err := x.ev.Eval(pr.e, f)
		if err != nil {
			return nil, false
		}
		if pr.list {
			return elements(v)
		}
		return []value.Value{v}, true
	}
	for _, c := range ip.checks {
		vs, ok := eval(c)
		if !ok {
			return pk, false
		}
		for _, v := range vs {
			if _, ok := eqKeys(v); !ok {
				return pk, false
			}
		}
	}
	for _, eq := range ip.eqs {
		vs, ok := eval(eq)
		if !ok {
			return pk, false
		}
		var ks [][]byte
		for _, v := range vs {
			k, ok := eqKeys(v)
			if !ok {
				return pk, false
			}
			ks = append(ks, k...)
		}
		pk.eqs = append(pk.eqs, ks)
	}
	bound := func(pr *probe) ([]byte, bool) {
		vs, ok := eval(*pr)
		if !ok {
			return nil, false
		}
		return rangeKey(vs[0])
	}
	if ip.lo != nil {
		var ok bool
		if pk.lo, ok = bound(ip.lo); !ok {
			return pk, false
		}
		pk.loIncl = ip.lo.incl
	}
	if ip.hi != nil {
		var ok bool
		if pk.hi, ok = bound(ip.hi); !ok {
			return pk, false
		}
		pk.hiIncl = ip.hi.incl
		if pk.lo != nil && pk.lo[0] != pk.hi[0] {
			pk.hi = nil // a type error for every entity in range; the where step reports it
		}
	}
	if ip.prefix != nil {
		v, err := x.ev.Eval(ip.prefix, f)
		if err != nil {
			return pk, false
		}
		t, ok := v.(value.Text)
		if !ok || longValue(t) {
			return pk, false
		}
		k, _ := value.Key(t)
		pk.prefix = k[:len(k)-2] // without the terminator
	}
	return pk, true
}

// last returns the bounds of the range on the key after the leading
// ones, alone: nil, nil when there is none.
func (pk probeKeys) last() (lo, hi []byte) {
	switch {
	case pk.lo != nil || pk.hi != nil:
		g := pk.lo
		if g == nil {
			g = pk.hi
		}
		lo, hi = []byte{g[0]}, []byte{g[0] + 1}
		if pk.lo != nil {
			lo = pk.lo
			if !pk.loIncl {
				lo = storage.PrefixEnd(lo)
			}
		}
		if pk.hi != nil {
			hi = pk.hi
			if pk.hiIncl {
				hi = storage.PrefixEnd(hi)
			}
		}
	case pk.prefix != nil:
		lo, hi = pk.prefix, storage.PrefixEnd(pk.prefix)
	}
	return lo, hi
}

// ranges turns probe keys into index ranges: one per combination of the
// leading keys' values.
func (pk probeKeys) ranges() ([]keyRange, bool) {
	bases := [][]byte{nil}
	for _, ks := range pk.eqs {
		var next [][]byte
		for _, b := range bases {
			for _, k := range ks {
				next = append(next, append(slices.Clip(b), k...))
			}
		}
		if len(next) > maxProbes {
			return nil, false
		}
		bases = next
	}
	lo, hi := pk.last()
	var out []keyRange
	for _, b := range bases {
		r := keyRange{lo: b, hi: storage.PrefixEnd(b)}
		if lo != nil {
			r.lo = append(slices.Clip(b), lo...)
			r.hi = nil
			if hi != nil {
				r.hi = append(slices.Clip(b), hi...)
			} else if len(b) > 0 {
				r.hi = storage.PrefixEnd(b)
			}
		}
		if r.hi != nil && bytes.Compare(r.lo, r.hi) >= 0 {
			continue
		}
		out = append(out, r)
	}
	return out, true
}

// fraction estimates from statistics the share of a kind's entities an
// index lookup finds, assuming its keys are independent. ok is false
// without statistics for every key it uses.
func (pk probeKeys) fraction(st *engine.KindStats, a *engine.Access) (float64, bool) {
	if st == nil {
		return 0, false
	}
	frac := 1.0
	for i, ks := range pk.eqs {
		c := st.Column(a.Keys[i])
		if c == nil {
			return 0, false
		}
		f := 0.0
		for _, k := range ks {
			f += c.EqFraction(k, st.Sampled)
		}
		frac *= min(f, 1)
	}
	if lo, hi := pk.last(); lo != nil {
		c := st.Column(a.Keys[len(pk.eqs)])
		if c == nil {
			return 0, false
		}
		frac *= c.RangeFraction(lo, hi, st.Sampled)
	}
	return frac, true
}

// countRanges counts index entries in ranges, up to probeCap.
func (x *executor) countRanges(a *engine.Access, rs []keyRange) (int, error) {
	n := 0
	for _, r := range rs {
		err := x.tx.ScanAccess(a, r.lo, r.hi, false, func(value.ID) bool {
			n++
			return n < probeCap
		})
		if err != nil || n >= probeCap {
			return n, err
		}
	}
	return n, nil
}

// ===== Running a plan =====

// srow is a source row being built: its variables, their entities in
// source order, and whether an early condition failed to evaluate on it.
type srow struct {
	vars  lang.Row
	ids   []value.ID
	dirty bool
}

// runPlan produces a planned source's rows, with its where steps applied.
func (x *executor) runPlan(pl *lang.Pipeline, p *plan) ([]prow, error) {
	for _, kv := range pl.Source.Vars {
		if kv.Kind != "" {
			k, _ := x.tx.Catalog().Kind(kv.Kind)
			if err := x.need(pl, security.Read, k); err != nil {
				return nil, err
			}
		}
	}
	var out []srow
	var walk func(i int, r srow) error
	walk = func(i int, r srow) error {
		if i == len(p.levels) {
			out = append(out, r)
			return nil
		}
		lv := p.levels[i]
		ids, err := x.candidates(pl, lv, &lang.Frame{Row: r.vars})
		if err != nil {
			return err
		}
		for _, id := range ids {
			nr, keep, err := x.bindLevel(pl, lv, r, id)
			if err != nil {
				return err
			}
			if keep {
				if err := walk(i+1, nr); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var err error
	done := false
	if p.top != nil {
		out, done, err = x.runTop(pl, p)
	}
	if err == nil && !done {
		p.levels[0].rows = 0 // an index read that ran out counts again
		err = walk(0, srow{vars: lang.Row{}, ids: make([]value.ID, len(pl.Source.Vars))})
	}
	if err != nil {
		return nil, err
	}
	if p.permuted || p.top != nil {
		slices.SortStableFunc(out, func(a, b srow) int {
			for i := range a.ids {
				if c := bytes.Compare(a.ids[i][:], b.ids[i][:]); c != 0 {
					return c
				}
			}
			return 0
		})
	}
	rows := make([]prow, 0, len(out))
	for _, r := range out {
		if r.dirty {
			keep, err := x.recheck(pl.Steps[:p.consumed], r.vars)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
		}
		rows = append(rows, prow{vars: r.vars})
	}
	return rows, nil
}

// bindLevel binds a level's variable to an entity and checks the level's
// conditions. A condition that is false or null drops the row; one that
// fails to evaluate marks it for the where steps to check whole.
func (x *executor) bindLevel(pl *lang.Pipeline, lv *level, r srow, id value.ID) (srow, bool, error) {
	if err := x.tick(pl); err != nil {
		return srow{}, false, err
	}
	vars := make(lang.Row, len(r.vars)+1)
	for k, v := range r.vars {
		vars[k] = v
	}
	vars[lv.kv.Var] = value.Ref{Target: id}
	if err := x.bindRow(pl, len(vars)); err != nil {
		return srow{}, false, err
	}
	ids := slices.Clone(r.ids)
	ids[lv.pos] = id
	nr := srow{vars: vars, ids: ids, dirty: r.dirty}
	f := &lang.Frame{Row: vars}
	for _, c := range lv.conds {
		ok, err := x.ev.Cond(c, f)
		if err != nil {
			nr.dirty = true
			continue
		}
		if !ok {
			return srow{}, false, nil
		}
	}
	lv.rows++
	return nr, true, nil
}

// recheck runs where steps on one row.
func (x *executor) recheck(steps []lang.Step, vars lang.Row) (bool, error) {
	f := &lang.Frame{Row: vars}
	for _, st := range steps {
		w := st.(*lang.Where)
		ok, err := x.ev.Cond(w.Cond, f)
		if err != nil {
			return false, fail(w.Cond, err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// candidates finds a level's entities for one row of the earlier levels,
// in ID order.
func (x *executor) candidates(pl *lang.Pipeline, lv *level, f *lang.Frame) ([]value.ID, error) {
	p := lv.path
	var ids []value.ID
	switch p.how {
	case pathID:
		ok := true
		if ids = p.ids; !p.fixed {
			ids, ok = x.entityProbe(p, f)
		}
		if !ok {
			return x.scanLevel(pl, lv)
		}
		var out []value.ID
		for _, id := range ids {
			e, ok, err := x.graph.entity(id)
			if err != nil {
				return nil, fail(pl, err)
			}
			if ok && (lv.kind == nil || e.Kind != nil && e.Kind.ID == lv.kind.ID) {
				out = append(out, id)
			}
		}
		ids = out
	case pathText:
		ok := true
		if ids = p.ids; !p.fixed {
			var err error
			if ids, ok, err = x.textProbe(p, f); err != nil {
				return nil, err
			}
		}
		if !ok {
			return x.scanLevel(pl, lv)
		}
		ids = slices.Clone(ids)
	case pathIndex:
		rs, ok := p.ranges, true
		if !p.fixed {
			rs, ok = x.indexProbe(p.ix, f)
		}
		if !ok {
			return x.scanLevel(pl, lv)
		}
		for _, r := range rs {
			err := x.tx.ScanAccess(p.acc, r.lo, r.hi, false, func(id value.ID) bool {
				ids = append(ids, id)
				return true
			})
			if err != nil {
				return nil, fail(pl, err)
			}
		}
	default:
		return x.scanLevel(pl, lv)
	}
	slices.SortFunc(ids, func(a, b value.ID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(ids), nil
}

// scanLevel lists every entity of a level's kind, or every visible
// entity for a kindless variable.
func (x *executor) scanLevel(pl *lang.Pipeline, lv *level) ([]value.ID, error) {
	var ids []value.ID
	var stop error
	filter := lv.kv.Kind == "" && x.ctx.Access != nil
	err := x.tx.Entities(lv.kv.Kind, func(id value.ID) bool {
		if stop = x.tick(pl); stop != nil {
			return false
		}
		if filter {
			var ok bool
			if _, ok, stop = x.graph.entity(id); stop != nil || !ok {
				return stop == nil
			}
		}
		ids = append(ids, id)
		return true
	})
	if stop != nil {
		return nil, stop
	}
	return ids, fail(pl, err)
}

// runTop reads a single variable in index order until it has the rows
// sort … limit n keeps: n rows that passed every early condition, and
// the rest of the rows that tie with the last of them on the sort key.
// done is false when the index ran out before the limit and entities
// missing from it might belong in the result.
func (x *executor) runTop(pl *lang.Pipeline, p *plan) (out []srow, done bool, err error) {
	t, lv := p.top, p.levels[0]
	if t.n == 0 {
		return nil, true, nil
	}
	clean := 0
	var last value.Value
	stopped := false
	var failure error
	err = x.tx.ScanAccess(t.acc, t.rng.lo, t.rng.hi, t.desc, func(id value.ID) bool {
		r, keep, err := x.bindLevel(pl, lv, srow{vars: lang.Row{}, ids: make([]value.ID, 1)}, id)
		if err != nil {
			failure = err
			return false
		}
		if !keep {
			return true
		}
		if stopped {
			// Read on through the ties, and through long values whose
			// keys share last's start: those sit in hash order.
			v, err := x.ev.EvalLazy(t.sort, &lang.Frame{Row: r.vars})
			if err == nil {
				if same, err := value.SameKeyGroup(v, last); err == nil && !same {
					lv.rows-- // read only to see the ties end
					return false
				}
			}
		}
		out = append(out, r)
		if !r.dirty && !stopped {
			if clean++; clean == t.n {
				v, err := x.ev.EvalLazy(t.sort, &lang.Frame{Row: r.vars})
				if err == nil {
					last, stopped = v, true
				}
			}
		}
		return true
	})
	if failure != nil {
		return nil, false, failure
	}
	if err != nil {
		return nil, false, fail(pl, err)
	}
	if t.partial && !stopped {
		return nil, false, nil
	}
	return out, true, nil
}

// String describes a plan: each level's variable and path in binding
// order, then any early stop.
func (p *plan) String() string {
	var parts []string
	for _, lv := range p.levels {
		s := lv.kv.Var + ": "
		switch lp := lv.path; lp.how {
		case pathScan:
			s += "scan"
		case pathID:
			s += "id"
		case pathIndex:
			s += "index " + lp.acc.Name
		case pathText:
			s += "text " + lp.text.Name
		}
		if lv.path.dep {
			s += " per row"
		}
		parts = append(parts, s)
	}
	if t := p.top; t != nil {
		dir := "asc"
		if t.desc {
			dir = "desc"
		}
		parts = append(parts, fmt.Sprintf("top %d by %s %s", t.n, t.acc.Name, dir))
	}
	return strings.Join(parts, "; ")
}
