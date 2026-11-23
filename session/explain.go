package session

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// explain shows how a statement would run (spec/rhizome.md §12). For a
// query it lists the steps with what each starts from; there is no cost
// based planner yet, so steps run as written. For a data statement it
// runs the statement in a scratch transaction that is rolled back, and
// reports what would change or what blocks it. explain analyze runs the
// statement for real and reports rows and timings.
func (s *Session) explain(ctx context.Context, qc *query.Context, st *lang.Explain) (*query.Result, error) {
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

// plan describes a pipeline's steps without running it.
func (s *Session) plan(qc *query.Context, pl *lang.Pipeline) (*query.Result, error) {
	info, err := lang.Check(envOf(qc), pl)
	if err != nil {
		return nil, err
	}
	tx := qc.Tx
	r := table("step", "detail")
	src := pl.Source
	switch {
	case src.Start != nil:
		add(r, "start", "at "+lang.FormatExpr(src.Start)+" as "+src.StartAs)
	case src.In != nil:
		add(r, "start", "the entities in "+lang.FormatExpr(src.In))
	default:
		for _, kv := range src.Vars {
			n, err := tx.CountEntities(kv.Kind)
			if err != nil {
				return nil, failAt(pl, err)
			}
			if kv.Kind == "" {
				add(r, "scan", fmt.Sprintf("every entity as %s (%d, a full scan)", kv.Var, n))
			} else {
				add(r, "scan", fmt.Sprintf("kind %s as %s (%s)", kv.Kind, kv.Var, count(n, "entity", "entities")))
			}
		}
		if len(src.Vars) > 1 {
			add(r, "join", "every combination of the scans (a cross product)")
		}
	}
	for _, step := range pl.Steps {
		detail := lang.FormatStep(step)
		if info.Grouped[step] {
			detail += " (aggregates every row)"
		}
		add(r, stepName(step), detail)
	}
	res := r
	res.Warnings = info.Warnings
	for _, w := range info.Warnings {
		add(r, "warning", w.Msg)
	}
	return res, nil
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

// analyze runs a pipeline and reports each step's rows and time.
func (s *Session) analyze(qc *query.Context, pl *lang.Pipeline) (*query.Result, error) {
	r := table("step", "rows", "time")
	traced := *qc
	traced.Trace = func(step lang.Step, rows int, d time.Duration) {
		name := "source"
		if step != nil {
			name = stepName(step) + " " + lang.FormatStep(step)
		}
		add(r, name, rows, value.Duration(d))
	}
	began := time.Now()
	res, err := query.Exec(&traced, pl)
	if err != nil {
		return nil, err
	}
	add(r, "total", len(res.Rows), value.Duration(time.Since(began)))
	r.Warnings = res.Warnings
	return r, nil
}

// dryRun runs a data statement in a scratch transaction and reports what
// it would do. The scratch transaction sees committed data only, not the
// session's open transaction.
func (s *Session) dryRun(qc *query.Context, st lang.Stmt) (*query.Result, error) {
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
