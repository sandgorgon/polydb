package session

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/query"
	"github.com/sandgorgon/polydb/value"
)

// explain shows how a statement would run (spec/rhizome.md §12). For a
// query it shows the planner's choices (spec/rhizome.md §7.5) and the
// steps. For a data statement it
// runs the statement in a scratch transaction that is rolled back, and
// reports what would change or what blocks it. explain analyze runs the
// statement for real and reports rows and timings.
func (s *env) explain(ctx context.Context, qc *query.Context, st *lang.Explain) (*query.Result, error) {
	switch inner := st.Stmt.(type) {
	case *lang.Explain:
		return nil, sessionErr(st, "syntax", "explain can't be nested")
	case *lang.Pipeline:
		if st.Analyze {
			return s.analyze(qc, inner)
		}
		return s.plan(qc, inner)
	case *lang.Create, *lang.Update, *lang.Delete, *lang.Relate, *lang.Unrelate, *lang.Push, *lang.Pop:
		if st.Analyze {
			began := time.Now()
			res, err := query.Exec(qc, inner)
			if err != nil {
				return nil, err
			}
			r := table("step", "rows", "time")
			add(r, lang.Format(inner), res.Affected, value.Duration(time.Since(began)))
			return r, nil
		}
		return s.dryRun(qc, inner)
	}
	return nil, sessionErr(st, "syntax", "explain works on queries and data statements")
}

// plan describes how a pipeline would run, without running it: how the
// source finds each variable (spec/rhizome.md §7.5) with its estimated
// entities, then the steps.
func (s *env) plan(qc *query.Context, pl *lang.Pipeline) (*query.Result, error) {
	sp, info, err := query.Plan(qc, pl)
	if err != nil {
		return nil, err
	}
	r := table("step", "detail", "estimate")
	consumed, err := s.sourceRows(qc, pl, sp, func(step, detail string, est any) {
		add(r, step, detail, est)
	})
	if err != nil {
		return nil, err
	}
	for i, step := range pl.Steps {
		add(r, stepName(step), stepDetail(info, step, i < consumed), nil)
	}
	return withWarnings(r, info, sp, func(msg string) { add(r, "warning", msg, nil) }), nil
}

// sourceRows describes a pipeline's source, and returns how many where
// steps it applies.
func (s *env) sourceRows(qc *query.Context, pl *lang.Pipeline, sp *query.SourcePlan, row func(step, detail string, est any)) (int, error) {
	src := pl.Source
	switch {
	case src.Start != nil:
		row("find", "start at "+lang.FormatExpr(src.Start)+" as "+src.StartAs, 1)
	case src.In != nil:
		row("find", "the entities in "+lang.FormatExpr(src.In), nil)
	case sp == nil: // as of: every kind is scanned
		for _, kv := range src.Vars {
			n, err := qc.Tx.CountEntities(kv.Kind)
			if err != nil {
				return 0, failAt(pl, err)
			}
			what := "scan kind " + kv.Kind
			if kv.Kind == "" {
				what = "scan every entity"
			}
			row("find", kv.Var+": "+what, n)
		}
	default:
		for _, v := range sp.Vars {
			est := any(v.Estimate)
			if v.PerRow {
				est = fmt.Sprintf("%d per row", v.Estimate)
			}
			row("find", v.Detail, est)
		}
		if sp.Top != "" {
			row("top", sp.Top, sp.TopLimit)
		}
		return sp.Consumed, nil
	}
	return 0, nil
}

func stepDetail(info *lang.Info, step lang.Step, atSource bool) string {
	detail := lang.FormatStep(step)
	if info.Grouped[step] {
		detail += " (aggregates every row)"
	}
	if atSource {
		detail += " (checked at the source)"
	}
	return detail
}

// withWarnings adds the checker's and the planner's warnings to a result.
func withWarnings(r *query.Result, info *lang.Info, sp *query.SourcePlan, row func(string)) *query.Result {
	r.Warnings = append(r.Warnings, info.Warnings...)
	for _, w := range info.Warnings {
		row(w.Msg)
	}
	if sp != nil {
		for _, w := range sp.Warnings {
			r.Warnings = append(r.Warnings, &lang.Error{Code: "warning", Msg: w})
			row(w)
		}
	}
	return r
}

func stepName(st lang.Step) string {
	switch st := st.(type) {
	case *lang.Where:
		return "where"
	case *lang.Follow:
		if st.Match {
			return "match"
		}
		return "follow"
	case *lang.Let:
		return "let"
	case *lang.GroupBy:
		return "group"
	case *lang.Having:
		return "having"
	case *lang.Sort:
		return "sort"
	case *lang.Distinct:
		return "distinct"
	case *lang.Skip:
		return "skip"
	case *lang.Limit:
		return "limit"
	case *lang.Show:
		return "show"
	case *lang.Bind:
		return "bind"
	case *lang.UnionStep:
		return "union"
	case *lang.DataStep:
		return "change"
	}
	return "step"
}

// analyze runs a pipeline and reports, next to the plan's estimates,
// the rows each variable and step produced and the time each step took.
func (s *env) analyze(qc *query.Context, pl *lang.Pipeline) (*query.Result, error) {
	type stepRun struct {
		rows int
		d    time.Duration
	}
	var source *stepRun
	runs := map[lang.Step]stepRun{}
	var planned *query.SourcePlan
	traced := *qc
	traced.Trace = func(step lang.Step, rows int, d time.Duration) {
		if step == nil {
			source = &stepRun{rows, d}
		} else {
			runs[step] = stepRun{rows, d}
		}
	}
	traced.Planned = func(sp *query.SourcePlan) { planned = sp }
	traced.PlanIfSlower = 0 // explain analyze always reports the plan
	began := time.Now()
	res, err := query.Exec(&traced, pl)
	if err != nil {
		return nil, err
	}
	total := time.Since(began)
	_, info, err := query.Plan(qc, pl)
	if err != nil {
		return nil, err
	}
	r := table("step", "detail", "estimate", "rows", "time")
	if planned != nil {
		for _, v := range planned.Vars {
			est := any(v.Estimate)
			if v.PerRow {
				est = fmt.Sprintf("%d per row", v.Estimate)
			}
			add(r, "find", v.Detail, est, v.Rows, nil)
		}
		if planned.Top != "" {
			add(r, "top", planned.Top, planned.TopLimit, nil, nil)
		}
	} else if _, err := s.sourceRows(qc, pl, nil, func(step, detail string, est any) {
		add(r, step, detail, est, nil, nil)
	}); err != nil {
		return nil, err
	}
	if source != nil {
		add(r, "source", "the rows the source produced", nil, source.rows, value.Duration(source.d))
	}
	consumed := 0
	if planned != nil {
		consumed = planned.Consumed
	}
	for i, step := range pl.Steps {
		run, ok := runs[step]
		var rows, d any
		if ok && i >= consumed {
			rows, d = run.rows, value.Duration(run.d)
		}
		add(r, stepName(step), stepDetail(info, step, i < consumed), nil, rows, d)
	}
	add(r, "total", "", nil, len(res.Rows), value.Duration(total))
	return withWarnings(r, info, planned, func(msg string) { add(r, "warning", msg, nil, nil, nil) }), nil
}

// dryRun runs a data statement in a scratch transaction and reports what
// it would do. The scratch transaction sees committed data only, not the
// session's open transaction.
func (s *env) dryRun(qc *query.Context, st lang.Stmt) (*query.Result, error) {
	scratch := s.db.Begin(engine.TxOptions{User: s.user})
	defer scratch.Rollback()
	sc := *qc
	sc.Tx = scratch
	sc.Wait = nil
	handles := map[string]query.Handle{}
	for k, v := range qc.Handles {
		handles[k] = v
	}
	sc.Handles = handles
	r := table("step", "detail")
	if del, ok := st.(*lang.Delete); ok {
		if err := describeDelete(r, &sc, del); err != nil {
			return nil, err
		}
	}
	res, err := query.Exec(&sc, st)
	if err != nil {
		le, _ := err.(*lang.Error)
		msg := err.Error()
		if le != nil {
			msg = le.Code + ": " + le.Msg
		}
		add(r, "blocked", msg)
		return r, nil
	}
	add(r, "would change", count(res.Affected, "item", "items"))
	return r, nil
}

// describeDelete lists what deleting an entity removes along with it.
func describeDelete(r *query.Result, qc *query.Context, st *lang.Delete) error {
	var id value.ID
	switch t := st.Target.(type) {
	case *lang.HandleRef:
		h, ok := qc.Handles[t.Name]
		if !ok {
			return nil // the executor reports it
		}
		id = h.ID
	case *lang.Lit:
		id, _ = t.Val.(value.ID)
	default:
		return nil
	}
	e, err := qc.Tx.Get(id)
	if err != nil {
		return nil
	}
	what := "kindless entity"
	if e.Kind != nil {
		what = e.Kind.Name
	}
	add(r, "delete", fmt.Sprintf("%s %s with %s", what, lang.FormatExpr(st.Target), count(len(e.Fields), "field", "fields")))
	nodes := []value.ID{id}
	for _, f := range e.Fields {
		nodes = append(nodes, f.ID)
	}
	byType := map[string]int{}
	seen := map[value.ID]bool{}
	for _, n := range nodes {
		rels, err := qc.Tx.Relations(n, engine.Both, "")
		if err != nil {
			return failAt(st, err)
		}
		for _, rel := range rels {
			if seen[rel.ID] {
				continue
			}
			seen[rel.ID] = true
			if rt, ok := qc.Tx.Catalog().RelationTypeByID(rel.Type); ok {
				action := "deletes"
				if rt.OnDelete == model.RelRestrict {
					action = "blocks"
				}
				byType[action+" "+rt.Name]++
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(byType)) {
		add(r, "relations", fmt.Sprintf("%s: %d", k, byType[k]))
	}
	return nil
}
