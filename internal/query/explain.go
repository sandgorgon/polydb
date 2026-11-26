package query

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/security"
)

// SourcePlan describes how the planner finds a pipeline's first rows
// (spec/rhizome.md §7.5), for explain.
type SourcePlan struct {
	Vars []VarPlan // in the order they're bound
	// Top, if set, describes reading an index in order for sort … limit.
	Top      string
	TopLimit int
	// Consumed is the number of leading where steps the source applies.
	Consumed int
	Warnings []string
}

// VarPlan is how one variable's entities are found.
type VarPlan struct {
	Var      string
	Detail   string // e.g. "p: index person_age for p.age > 30"
	Estimate int    // per row of the earlier variables when PerRow
	PerRow   bool
	Rows     int // explain analyze: the rows this variable bound
}

// Plan checks a pipeline and plans its source without running it. The
// plan is nil when the source isn't a find over kinds, or the pipeline
// reads the past.
func Plan(ctx *Context, pl *lang.Pipeline) (*SourcePlan, *lang.Info, error) {
	if ctx.Handles == nil {
		ctx.Handles = map[string]Handle{}
	}
	info, err := lang.Check(ctx.env(), pl)
	if err != nil {
		return nil, nil, err
	}
	if pl.AsOf != nil {
		return nil, info, nil
	}
	x := newExecutor(ctx, info)
	for _, kv := range pl.Source.Vars {
		if kv.Kind != "" {
			k, _ := x.tx.Catalog().Kind(kv.Kind)
			if err := x.need(pl, security.Read, k); err != nil {
				return nil, nil, err
			}
		}
	}
	p, err := x.planSource(pl)
	if err != nil || p == nil {
		return nil, info, err
	}
	sp, err := x.describe(pl, p)
	return sp, info, err
}

// describe turns a plan into its explain form.
func (x *executor) describe(pl *lang.Pipeline, p *plan) (*SourcePlan, error) {
	sp := &SourcePlan{Consumed: p.consumed}
	var bound []string
	for i, lv := range p.levels {
		v := lv.kv.Var
		d := v + ": "
		lp := lv.path
		answers := ""
		if lp.cond != nil {
			answers = " for " + lang.FormatExpr(lp.cond)
		}
		switch lp.how {
		case pathScan:
			if lv.kind == nil {
				d += "scan every entity"
				sp.Warnings = append(sp.Warnings, v+" scans every entity in the database; give it a kind to read fewer")
			} else {
				d += "scan kind " + lv.kind.Name
			}
		case pathID:
			d += "lookup" + answers
		case pathIndex:
			what := "index "
			if lp.acc.Index == nil {
				what = "unique field "
			}
			var used []lang.Expr
			for _, a := range lp.ix.answers {
				if !slices.Contains(used, a.cond) {
					used = append(used, a.cond)
				}
			}
			if len(used) > 0 {
				var ts []string
				exact := lp.fixed && !(i == 0 && p.top != nil) // the top path checks its rows
				for _, c := range used {
					ts = append(ts, lang.FormatExpr(c))
					exact = exact && slices.Contains(lp.answered, c)
				}
				answers = " for " + strings.Join(ts, " and ")
				if exact {
					answers += ", exact"
				}
			}
			d += what + lp.acc.Name + answers
		case pathText:
			d += "text index " + lp.text.Name + answers
		}
		if lp.dep {
			d += ", for each " + strings.Join(bound, ", ")
		}
		est := lp.est
		if lp.how == pathScan && (lp.est == 0 || len(p.levels) > 1) {
			n, err := x.tx.CountEntities(lv.kv.Kind)
			if err != nil {
				return nil, fail(pl, err)
			}
			est = n
		}
		sp.Vars = append(sp.Vars, VarPlan{Var: v, Detail: d, Estimate: est, PerRow: lp.dep})
		if i > 0 && !lp.dep && !linked(lv, bound, x) {
			sp.Warnings = append(sp.Warnings, fmt.Sprintf("no condition links %s to %s, so every combination is formed (a cross product)", v, strings.Join(bound, ", ")))
		}
		bound = append(bound, v)
		ws, err := x.indexWarnings(pl, lv, p)
		if err != nil {
			return nil, err
		}
		sp.Warnings = append(sp.Warnings, ws...)
	}
	if t := p.top; t != nil {
		dir := "ascending"
		if t.desc {
			dir = "descending"
		}
		sp.Top = fmt.Sprintf("read %s in %s order, stopping after %d rows", t.acc.Name, dir, t.n)
		sp.TopLimit = t.n
	}
	return sp, nil
}

// linked reports whether some condition checked at a level reads an
// earlier variable.
func linked(lv *level, bound []string, x *executor) bool {
	found := false
	for _, c := range lv.conds {
		lang.Walk(c, func(e lang.Expr) bool {
			if n, ok := e.(*lang.NameRef); ok {
				r := x.info.Refs[n]
				found = found || r.Kind == lang.RefVar && slices.Contains(bound, r.Name)
			}
			return !found
		})
	}
	return found
}

// indexWarnings reports conditions on a variable that an index would
// answer if it were ready, and search() without a text index.
func (x *executor) indexWarnings(pl *lang.Pipeline, lv *level, p *plan) ([]string, error) {
	if lv.kind == nil {
		return nil, nil
	}
	pc := &planner{x: x, pl: pl}
	var out []string
	var searched []string
	for _, c := range lv.conds {
		lang.Walk(c, func(e lang.Expr) bool {
			call, ok := e.(*lang.Call)
			if ok && call.Name == "search" && len(call.Args) == 2 {
				if f, ok := call.Args[0].(*lang.FieldExpr); ok && pc.isVar(f.X, lv.kv.Var) && !slices.Contains(searched, f.Name) {
					searched = append(searched, f.Name)
				}
			}
			return true
		})
	}
	reads := func(field string) bool {
		found := false
		for _, c := range lv.conds {
			lang.Walk(c, func(e lang.Expr) bool {
				if f, ok := e.(*lang.FieldExpr); ok && f.Name == field && pc.isVar(f.X, lv.kv.Var) {
					found = true
				}
				return !found
			})
		}
		return found
	}
	for _, field := range searched {
		has := false
		for _, ix := range x.tx.Catalog().IndexesOn(lv.kind.ID) {
			if ix.Text && ix.Keys[0].Field == field {
				has = true
			}
		}
		if !has {
			out = append(out, fmt.Sprintf("search() on %s.%s has no text index, so each %s's text is matched; define text index … on %s(%s) to make it fast",
				lv.kv.Var, field, lv.kv.Var, lv.kind.Name, field))
		}
	}
	for _, ix := range x.tx.Catalog().IndexesOn(lv.kind.ID) {
		if ix.Unique || !reads(ix.Keys[0].Field) {
			continue
		}
		st, err := x.tx.IndexState(ix)
		if err != nil {
			return nil, fail(pl, err)
		}
		if !st.Ready {
			out = append(out, fmt.Sprintf("index %s is still building (%d entities indexed), so it isn't used yet", ix.Name, st.Done))
		}
	}
	return out, nil
}

// reportPlan hands the plan, with the rows each level bound, to
// Context.Planned (explain analyze).
func (x *executor) reportPlan(pl *lang.Pipeline, p *plan) error {
	if x.ctx.Planned == nil || (x.ctx.PlanIfSlower > 0 && time.Since(x.began) < x.ctx.PlanIfSlower) {
		return nil
	}
	sp, err := x.describe(pl, p)
	if err != nil {
		return err
	}
	for i, lv := range p.levels {
		sp.Vars[i].Rows = lv.rows
	}
	x.ctx.Planned(sp)
	return nil
}
