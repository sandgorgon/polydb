package lang

import (
	"fmt"
	"regexp"
	"time"

	"github.com/sandgorgon/polydb/fulltext"
	"github.com/sandgorgon/polydb/value"
)

// Graph is what evaluating expressions needs from the database. The
// executor implements it on a transaction.
type Graph interface {
	// Field reads field name of an entity, or property name of a
	// relation. ok is false when it is absent.
	Field(id value.ID, name string) (v value.Value, ok bool, err error)
	// FieldMeta reads metadata of an entity's field: id, name, doc, type
	// or declared. ok is false when the entity has no such field.
	FieldMeta(id value.ID, field, name string) (v value.Value, ok bool, err error)
	// Meta reads metadata of an entity or relation (spec/rhizome.md §2.1).
	// An entity's ::kind is null when it has no kind.
	Meta(id value.ID, name string) (value.Value, error)
	// Degree counts the relations of a type ("" for any) touching a node
	// in a direction: "out", "in" or "both".
	Degree(id value.ID, typ, dir string) (int, error)
	// Format looks up a document format by name.
	Format(name string) (value.Format, bool)
}

// Row holds a pipeline row's variables. Entities are bound as refs,
// relations as IDs, and paths as lists that alternate node refs and
// relation IDs.
type Row map[string]value.Value

// Frame is what an expression can read besides constants.
type Frame struct {
	Row Row
	// Target is the entity an update changes, read by bare field names
	// and ::meta (with HasTarget set).
	Target    value.ID
	HasTarget bool
	// Fields, when set, are the field values a check or default
	// expression reads by bare name, instead of the target's stored ones.
	Fields map[string]value.Value
	// Group holds the rows of the current group, for aggregates.
	Group []Row
}

// Evaluator evaluates checked expressions. One evaluator serves one
// statement: Now is fixed for the statement, and parsed documents and
// compiled regular expressions are cached.
type Evaluator struct {
	Info    *Info
	Graph   Graph
	Params  map[string]value.Value
	Handles map[string]value.Value // a ref for an entity, an ID for a relation
	// LookupHandle, if set, finds handles missing from Handles.
	LookupHandle func(name string) (value.Value, bool)
	Now          time.Time
	// Budget is the statement's working memory, which loading large
	// values (value.Lazy) counts against; nil means no limit.
	Budget *Budget

	loaded   map[value.BlobRef]value.Value // lazy values loaded so far
	prefixes map[value.BlobRef][]byte      // starts of lazy values (Order)

	regexps map[string]*regexp.Regexp
	queries map[string]*fulltext.Query // search() queries
	docs    map[docKey]value.Value
}

type docKey struct{ format, text string }

// runErr is an error found while evaluating, at an expression's position.
func runErr(e Node, code, format string, args ...any) *Error {
	return &Error{Pos: e.Position(), Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Cond evaluates a condition. Null counts as false (spec/rhizome.md §8.3).
func (ev *Evaluator) Cond(e Expr, f *Frame) (bool, error) {
	v, err := ev.Eval(e, f)
	if err != nil {
		return false, err
	}
	switch v := v.(type) {
	case value.Bool:
		return bool(v), nil
	case nil:
		return false, nil
	}
	if value.IsNull(v) {
		return false, nil
	}
	return false, runErr(e, "type", "a condition must be true or false, not %s", typeName(v))
}

// Eval evaluates e. It never returns a nil value: absent and missing
// values are value.Null. A large value is loaded (see Load).
func (ev *Evaluator) Eval(e Expr, f *Frame) (value.Value, error) {
	v, err := ev.EvalLazy(e, f)
	if err != nil {
		return nil, err
	}
	return ev.Load(e, v)
}

// EvalLazy is Eval leaving a large value whose content is still in the
// blob store as a value.Lazy, for callers that pass it along unread.
func (ev *Evaluator) EvalLazy(e Expr, f *Frame) (value.Value, error) {
	if f == nil {
		f = &Frame{}
	}
	v, err := ev.eval(e, f)
	if ve, isVE := err.(valueErr); isVE {
		err = runErr(e, "type", "%v", ve.err)
	}
	return v, err
}

// Load returns v with a lazy value's content loaded, counting it against
// the Budget. A value loaded once is kept for the statement.
func (ev *Evaluator) Load(n Node, v value.Value) (value.Value, error) {
	l, ok := v.(value.Lazy)
	if !ok {
		return v, nil
	}
	if c, ok := ev.loaded[l.Ref]; ok {
		return c, nil
	}
	if err := ev.Budget.Load(l.Ref.Len); err != nil {
		return nil, runErr(n, "limit", "loading %s: %v", l, err)
	}
	c, err := l.Load()
	if err != nil {
		return nil, runErr(n, "internal", "%v", err)
	}
	if ev.loaded == nil {
		ev.loaded = map[value.BlobRef]value.Value{}
	}
	ev.loaded[l.Ref] = c
	return c, nil
}

func (ev *Evaluator) eval(e Expr, f *Frame) (value.Value, error) {
	if v, ok := ev.Info.Consts[e]; ok {
		return v, nil
	}
	if r, ok := ev.Info.Refs[e]; ok && r.Kind == RefColumn {
		return rowGet(f, r.Name), nil
	}
	switch e := e.(type) {
	case *Lit:
		return orNull(e.Val), nil
	case *NameRef:
		return ev.name(e, f)
	case *HandleRef:
		v, ok := ev.Handles[e.Name]
		if !ok && ev.LookupHandle != nil {
			v, ok = ev.LookupHandle(e.Name)
		}
		if !ok {
			return nil, runErr(e, "not_found", "no handle @%s", e.Name)
		}
		return v, nil
	case *ParamRef:
		v, ok := ev.Params[e.Name]
		if !ok {
			return nil, runErr(e, "not_found", "parameter $%s isn't set", e.Name)
		}
		return orNull(v), nil
	case *MetaRef:
		if !f.HasTarget {
			return nil, runErr(e, "syntax", "::%s needs an update target", e.Name)
		}
		return ev.Graph.Meta(f.Target, e.Name)
	case *Unary:
		x, err := ev.Eval(e.X, f)
		if err != nil {
			return nil, err
		}
		if e.Op == "not" {
			return not(e, x)
		}
		return neg(e, x)
	case *Binary:
		return ev.binary(e, f)
	case *Is:
		return ev.is(e, f)
	case *Between:
		return ev.between(e, f)
	case *FieldExpr:
		x, err := ev.Eval(e.X, f)
		if err != nil {
			return nil, err
		}
		return ev.member(e, x, e.Name)
	case *MetaExpr:
		return ev.metaExpr(e, f)
	case *IndexExpr:
		x, err := ev.Eval(e.X, f)
		if err != nil {
			return nil, err
		}
		i, err := ev.Eval(e.Index, f)
		if err != nil {
			return nil, err
		}
		v, _, err := index(e, x, i)
		return v, err
	case *DocPath:
		v, _, err := ev.docPath(e, f)
		return v, err
	case *Call:
		return ev.call(e, f)
	case *If:
		c, err := ev.Cond(e.Cond, f)
		if err != nil {
			return nil, err
		}
		if c {
			return ev.EvalLazy(e.Then, f)
		}
		return ev.EvalLazy(e.Else, f)
	case *Cast:
		x, err := ev.Eval(e.X, f)
		if err != nil {
			return nil, err
		}
		v, err := ev.cast(x, e.Type)
		if err != nil {
			return nil, runErr(e, "type", "%v", err)
		}
		return v, nil
	case *Exists:
		_, ok, err := ev.present(e.X, f)
		return value.Bool(ok), err
	case *ListLit:
		vs, err := ev.evalAll(e.Elems, f)
		if err != nil {
			return nil, err
		}
		return built(value.NewList(vs...))
	case *SetLit:
		vs, err := ev.evalAll(e.Elems, f)
		if err != nil {
			return nil, err
		}
		return built(value.NewSet(vs...))
	case *MapLit:
		keys := make([]value.Value, len(e.Entries))
		vals := make([]value.Value, len(e.Entries))
		for i, en := range e.Entries {
			var err error
			if keys[i], err = ev.Eval(en.Key, f); err != nil {
				return nil, err
			}
			if vals[i], err = ev.Eval(en.Value, f); err != nil {
				return nil, err
			}
		}
		return built(value.NewMap(keys, vals))
	}
	return nil, runErr(e, "internal", "evaluator: unexpected expression %T", e)
}

// built adapts a value constructor's result. Its error is reported by Eval
// as a type error at the innermost expression being evaluated.
func built[T value.Value](v T, err error) (value.Value, error) {
	if err != nil {
		return nil, valueErr{err}
	}
	return v, nil
}

// valueErr is an error from building a value, not yet given a position.
type valueErr struct{ err error }

func (e valueErr) Error() string { return e.err.Error() }

func (ev *Evaluator) evalAll(es []Expr, f *Frame) ([]value.Value, error) {
	out := make([]value.Value, len(es))
	for i, x := range es {
		var err error
		if out[i], err = ev.Eval(x, f); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func orNull(v value.Value) value.Value {
	if v == nil {
		return value.Null
	}
	return v
}

func rowGet(f *Frame, name string) value.Value {
	return orNull(f.Row[name])
}

// name evaluates a bare name as the checker resolved it.
func (ev *Evaluator) name(e *NameRef, f *Frame) (value.Value, error) {
	r, ok := ev.Info.Refs[e]
	if !ok {
		return nil, runErr(e, "internal", "evaluator: name %s wasn't checked", e.Name)
	}
	if r.Kind != RefField {
		return rowGet(f, r.Name), nil
	}
	if f.Fields != nil {
		return orNull(f.Fields[r.Name]), nil
	}
	if !f.HasTarget {
		return nil, runErr(e, "internal", "evaluator: field %s read without a target", e.Name)
	}
	v, _, err := ev.Graph.Field(f.Target, r.Name)
	return orNull(v), err
}

// nodeID returns the ID an entity, relation or node value names.
func nodeID(v value.Value) (value.ID, bool) {
	switch v := v.(type) {
	case value.Ref:
		return v.Target, true
	case value.ID:
		return v, true
	}
	return value.ID{}, false
}

// member evaluates x.name: a field of an entity (following refs), a
// property of a relation, a record field or a map entry.
func (ev *Evaluator) member(e Expr, x value.Value, name string) (value.Value, error) {
	v, _, err := ev.memberOK(e, x, name)
	return v, err
}

func (ev *Evaluator) memberOK(e Expr, x value.Value, name string) (value.Value, bool, error) {
	switch x := x.(type) {
	case value.Ref, value.ID:
		id, _ := nodeID(x)
		v, ok, err := ev.Graph.Field(id, name)
		return orNull(v), ok, err
	case value.Record:
		v, ok := x.Get(name)
		return orNull(v), ok, nil
	case value.Map:
		v, ok := x.Get(value.MustText(name))
		return orNull(v), ok, nil
	}
	if value.IsNull(x) {
		return value.Null, false, nil
	}
	return nil, false, runErr(e, "type", ".%s reads fields of entities, records and maps, not %s", name, typeName(x))
}

// index evaluates x[i]. Positions outside a list, and missing keys, give
// null.
func index(e Expr, x, i value.Value) (value.Value, bool, error) {
	if value.IsNull(x) || value.IsNull(i) {
		return value.Null, false, nil
	}
	switch c := x.(type) {
	case value.List, value.Array:
		n, ok := i.(value.Int)
		if !ok {
			return nil, false, runErr(e, "type", "list positions are ints, not %s", typeName(i))
		}
		elems := seqValues(c)
		if n < 0 || int64(n) >= int64(len(elems)) {
			return value.Null, false, nil
		}
		return orNull(elems[n]), true, nil
	case value.Map:
		v, ok := c.Get(i)
		return orNull(v), ok, nil
	case value.Record:
		if t, ok := i.(value.Text); ok {
			v, ok := c.Get(t.Go())
			return orNull(v), ok, nil
		}
	}
	return nil, false, runErr(e, "type", "[ ] reads from lists, arrays and maps, not %s", typeName(x))
}

func seqValues(v value.Value) []value.Value {
	switch v := v.(type) {
	case value.List:
		return v.Values()
	case value.Array:
		return v.Values()
	case value.Set:
		return v.Values()
	case value.Queue:
		return v.Items()
	}
	return nil
}

func (ev *Evaluator) metaExpr(e *MetaExpr, f *Frame) (value.Value, error) {
	if fx, ok := e.X.(*FieldExpr); ok {
		owner, err := ev.Eval(fx.X, f)
		if err != nil {
			return nil, err
		}
		if id, ok := nodeID(owner); ok {
			v, _, err := ev.Graph.FieldMeta(id, fx.Name, e.Name)
			return orNull(v), err
		}
		if value.IsNull(owner) {
			return value.Null, nil
		}
	}
	x, err := ev.Eval(e.X, f)
	if err != nil {
		return nil, err
	}
	if id, ok := nodeID(x); ok {
		return ev.Graph.Meta(id, e.Name)
	}
	if value.IsNull(x) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "::%s reads metadata of entities, fields and relations, not %s", e.Name, typeName(x))
}

// present evaluates e and reports whether it is present, for exists():
// a field, entry, element or document step that exists, even if it holds
// null. Other expressions are present when they aren't null.
func (ev *Evaluator) present(e Expr, f *Frame) (value.Value, bool, error) {
	switch x := e.(type) {
	case *FieldExpr:
		owner, err := ev.Eval(x.X, f)
		if err != nil {
			return nil, false, err
		}
		return ev.memberOK(x, owner, x.Name)
	case *IndexExpr:
		c, err := ev.Eval(x.X, f)
		if err != nil {
			return nil, false, err
		}
		i, err := ev.Eval(x.Index, f)
		if err != nil {
			return nil, false, err
		}
		return index(x, c, i)
	case *DocPath:
		return ev.docPath(x, f)
	case *NameRef:
		if r := ev.Info.Refs[x]; r.Kind == RefField {
			switch {
			case f.Fields != nil:
				v, ok := f.Fields[r.Name]
				return orNull(v), ok && v != nil, nil
			case f.HasTarget:
				v, ok, err := ev.Graph.Field(f.Target, r.Name)
				return orNull(v), ok, err
			}
		}
	}
	v, err := ev.Eval(e, f)
	if err != nil {
		return nil, false, err
	}
	return v, !value.IsNull(v), nil
}

// typeName names a value's type for messages and typeof().
func typeName(v value.Value) string {
	if value.IsNull(v) {
		return "null"
	}
	if d, ok := v.(value.Doc); ok {
		return "doc<" + d.Format() + ">"
	}
	return v.Code().String()
}

// Assign computes the new value of an assignment (spec/rhizome.md §6.2)
// from the current value and the evaluated right side: = replaces, +=
// and friends do arithmetic, ++= joins and --= removes.
func (ev *Evaluator) Assign(as *Assign, cur, v value.Value) (value.Value, error) {
	if as.Op == "=" {
		return v, nil
	}
	cur, err := ev.Load(as, cur)
	if err != nil {
		return nil, err
	}
	if v, err = ev.Load(as, v); err != nil {
		return nil, err
	}
	switch as.Op {
	case "+=", "-=", "*=", "/=":
		return arithValues(as, as.Op[:1], orNull(cur), v)
	case "++=", "--=":
		return joinValues(as, as.Op[:2], orNull(cur), v)
	}
	return nil, runErr(as, "syntax", "unknown assignment %s", as.Op)
}
