// Package query executes Rhizome statements (spec/rhizome.md) on an
// engine transaction: definitions, data changes and pipelines. Sessions,
// transactions statements and introspection build on it.
package query

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// Handle is what a session handle (@name) refers to.
type Handle struct {
	ID   value.ID
	Node model.NodeKind // NodeEntity or NodeRelation
	Kind value.ID       // an entity's kind, zero if kindless; a relation's type
}

// Settings are the session settings a statement runs with.
type Settings struct {
	MaxDepth int           // cap for open-ended multi-hop patterns (*, *2..)
	MaxRows  int           // 0: no limit
	Timeout  time.Duration // 0: no limit; enforced by the session
}

// DefaultSettings are the settings of a new session.
var DefaultSettings = Settings{MaxDepth: 10}

// Context is what a statement runs with.
type Context struct {
	Tx *engine.Tx
	// Handles are the session's handles. Statements that bind handles
	// (create @x, relate … as @x, bind) add to it.
	Handles  map[string]Handle
	Params   map[string]value.Value
	Settings Settings
	// Wait, if set, runs pop … wait outside the transaction. Without it,
	// a waiting pop is an error.
	Wait func(id value.ID, field string, wait time.Duration) (value.Value, error)
	// Interrupt, if set, is called regularly during long work. An error
	// stops the statement: a timeout or a cancellation.
	Interrupt func() error
	// Trace, if set, receives the rows each pipeline step produced and
	// the time it took, for explain analyze. The source has a nil step.
	Trace func(step lang.Step, rows int, d time.Duration)
	// Access, if set, decides what the user may do. Entities of kinds the
	// user can't read are invisible: scans and patterns skip them, and
	// naming one directly is a permission error. Nil allows everything
	// (embedded mode).
	Access Access
	// NoPlanner runs pipelines as written, scanning every kind, without
	// the planner (tests compare the two).
	NoPlanner bool
}

// Access decides what the user running a statement may do with entities
// of a kind (nil: kindless entities, decided at database level).
type Access interface {
	Can(p security.Perm, kind *model.Kind) bool
}

// Column is a result column.
type Column struct {
	Name string
	Type lang.Static
}

// Result is what a statement returns (spec/rhizome.md §13). Cells are
// value.Value, or *Entity, *Relation or *Path for whole graph objects.
type Result struct {
	Columns  []Column
	Rows     [][]any
	Affected int
	Warnings []*lang.Error
}

// Entity is a whole entity in a result (spec/protocol.md §6.5).
type Entity struct {
	ID     value.ID
	Kind   string // "" for kindless
	Meta   model.Meta
	Fields []*Field
}

// Field is one field of an entity in a result.
type Field struct {
	ID       value.ID
	Name     string
	Type     *value.Type
	Doc      string
	Declared bool
	Value    value.Value // nil when absent
}

// Relation is a whole relation in a result.
type Relation struct {
	ID       value.ID
	Type     string
	From, To value.ID
	Props    value.Map
}

// Path is a path in a result: its nodes (entities, or fields as IDs) and
// the relations between them.
type Path struct {
	Nodes []any
	Rels  []*Relation
}

// Exec checks and runs one statement.
func Exec(ctx *Context, s lang.Stmt) (*Result, error) {
	if ctx.Handles == nil {
		ctx.Handles = map[string]Handle{}
	}
	if ctx.Settings.MaxDepth <= 0 {
		ctx.Settings.MaxDepth = DefaultSettings.MaxDepth
	}
	info, err := lang.Check(ctx.env(), s)
	if err != nil {
		return nil, err
	}
	x := newExecutor(ctx, info)
	res, err := x.stmt(s)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, info.Warnings...)
	return res, nil
}

func newExecutor(ctx *Context, info *lang.Info) *executor {
	x := &executor{ctx: ctx, info: info, tx: ctx.Tx, now: ctx.Tx.Now()}
	x.graph = &graph{tx: ctx.Tx, ents: map[value.ID]*engine.Entity{}, hidden: map[value.ID]bool{}, access: ctx.Access}
	x.ev = &lang.Evaluator{Info: info, Graph: x.graph, Params: ctx.Params, LookupHandle: x.handleValue, Now: x.now}
	return x
}

// executor runs one checked statement.
type executor struct {
	ctx   *Context
	info  *lang.Info
	tx    *engine.Tx
	graph *graph
	past  *engine.AsOf // set while an as of pipeline runs
	ev    *lang.Evaluator
	now   time.Time
	ticks int
}

// can reports whether the user may do p on entities of kind.
func (x *executor) can(p security.Perm, kind *model.Kind) bool {
	return x.ctx.Access == nil || x.ctx.Access.Can(p, kind)
}

// need fails with a permission error unless the user may do p on kind.
func (x *executor) need(n lang.Node, p security.Perm, kind *model.Kind) error {
	if x.can(p, kind) {
		return nil
	}
	if kind == nil {
		return errAt(n, "permission", "you don't have %s permission on kindless entities in this database", p)
	}
	return errAt(n, "permission", "you don't have %s permission on kind %s", p, kind.Name)
}

// nodeKind returns the kind of a node's entity: the entity itself, or the
// entity a field belongs to. Relations have none.
func (x *executor) nodeKind(n lang.Node, id value.ID) (*model.Kind, error) {
	nk, ok, err := x.tx.Exists(id)
	if err != nil || !ok {
		return nil, fail(n, err)
	}
	switch nk {
	case model.NodeField:
		f, err := x.tx.FieldByID(id)
		if err != nil {
			return nil, fail(n, err)
		}
		id = f.Entity
	case model.NodeRelation:
		return nil, nil
	}
	e, err := x.tx.Get(id)
	if err != nil {
		return nil, fail(n, err)
	}
	return e.Kind, nil
}

// tick calls the Interrupt hook every so often.
func (x *executor) tick(n lang.Node) error {
	x.ticks++
	if x.ctx.Interrupt == nil || x.ticks%256 != 0 {
		return nil
	}
	return fail(n, x.ctx.Interrupt())
}

// env builds the checker's view of the session.
func (ctx *Context) env() *lang.Env {
	cat := ctx.Tx.Catalog()
	env := &lang.Env{Catalog: cat, Params: ctx.Params}
	env.LookupHandle = func(name string) (lang.Static, bool) {
		h, ok := ctx.Handles[name]
		if !ok {
			return lang.Static{}, false
		}
		st := lang.Static{Form: lang.FormEntity}
		switch h.Node {
		case model.NodeRelation:
			st = lang.Static{Form: lang.FormRelation}
			if rt, ok := cat.RelationTypeByID(h.Kind); ok && rt.Defined {
				st.Rel = rt
			}
		case model.NodeEntity:
			if k, ok := cat.KindByID(h.Kind); ok {
				st.Kind = k
			}
		default:
			st = lang.Static{Form: lang.FormNode}
		}
		return st, true
	}
	env.HandleNames = func() []string { return slices.Collect(maps.Keys(ctx.Handles)) }
	return env
}

// handleValues returns the handles as the evaluator sees them: entities
// as refs, others as IDs.
// handleValue is what a handle evaluates to: a ref for an entity, an ID
// for a relation.
func (x *executor) handleValue(name string) (value.Value, bool) {
	h, ok := x.ctx.Handles[name]
	if !ok {
		return nil, false
	}
	return nodeValue(h.ID, h.Node), true
}

func nodeValue(id value.ID, n model.NodeKind) value.Value {
	if n == model.NodeEntity {
		return value.Ref{Target: id}
	}
	return id
}

// bindHandle records a handle and makes it visible to later expressions.
func (x *executor) bindHandle(name string, h Handle) {
	x.ctx.Handles[name] = h
}

// fail gives an error the position of the node being executed, keeping
// its code. Rhizome errors keep their own position.
func fail(n lang.Node, err error) error {
	if err == nil {
		return nil
	}
	var le *lang.Error
	if errors.As(err, &le) {
		return le
	}
	var ee *engine.Error
	if errors.As(err, &ee) {
		return &lang.Error{Pos: n.Position(), Code: string(ee.Code), Msg: ee.Msg}
	}
	return &lang.Error{Pos: n.Position(), Code: string(engine.CodeOf(err)), Msg: err.Error()}
}

func errAt(n lang.Node, code, format string, args ...any) *lang.Error {
	return &lang.Error{Pos: n.Position(), Code: code, Msg: fmt.Sprintf(format, args...)}
}

func (x *executor) stmt(s lang.Stmt) (*Result, error) {
	switch s := s.(type) {
	case *lang.DefineKind, *lang.AlterKind, *lang.DefineRelation, *lang.AlterRelation,
		*lang.DefineFormat, *lang.DefineIndex, *lang.Drop:
		return x.definition(s)
	case *lang.Create:
		return x.create(s)
	case *lang.Update:
		n, err := x.update(s, nil)
		return &Result{Affected: n}, err
	case *lang.Delete:
		n, err := x.delete(s, nil)
		return &Result{Affected: n}, err
	case *lang.Relate:
		return x.relate(s, nil)
	case *lang.Unrelate:
		n, err := x.unrelate(s, nil)
		return &Result{Affected: n}, err
	case *lang.Push:
		n, err := x.push(s, nil)
		return &Result{Affected: n}, err
	case *lang.Pop:
		v, err := x.pop(s, nil)
		if err != nil {
			return nil, err
		}
		return &Result{Columns: []Column{{"value", lang.Static{Type: value.TypeAny}}}, Rows: [][]any{{v}}}, nil
	case *lang.Pipeline:
		return x.query(s)
	case *lang.History:
		return x.historyStmt(s)
	}
	return nil, errAt(s, "unsupported", "%s statements aren't handled by the executor", stmtName(s))
}

func stmtName(s lang.Stmt) string {
	switch s.(type) {
	case *lang.CreateUser, *lang.CreateRole, *lang.Grant, *lang.AlterUser:
		return "security"
	case *lang.History:
		return "history"
	case *lang.Describe, *lang.ShowStmt, *lang.Explain:
		return "introspection"
	case *lang.Begin, *lang.Commit, *lang.Rollback:
		return "transaction"
	}
	return "session"
}
