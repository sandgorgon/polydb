package lang

import (
	"errors"
	"time"

	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/value"
)

// Compiled is a checked default or check expression from a definition,
// ready to evaluate outside any statement. The engine uses these to apply
// defaults on create and checks on every write.
type Compiled struct {
	Src  string
	Expr Expr
	Info *Info
}

// CompileDefault parses and checks a default expression: a constant that
// can be stored as t.
func CompileDefault(cat *catalog.Catalog, src string, t *value.Type) (*Compiled, error) {
	e, err := ParseExpr(src)
	if err != nil {
		return nil, err
	}
	c := newChecker(&Env{Catalog: cat})
	sc := newScope()
	sc.noRows = "a default"
	if err := c.write(e, t, sc); err != nil {
		return nil, err
	}
	return &Compiled{src, e, c.info}, nil
}

// CompileCheck parses and checks a check expression: a condition that
// reads the given fields by name.
func CompileCheck(cat *catalog.Catalog, src string, fields map[string]*value.Type) (*Compiled, error) {
	e, err := ParseExpr(src)
	if err != nil {
		return nil, err
	}
	c := newChecker(&Env{Catalog: cat})
	sc := newScope()
	sc.fields = fields
	if err := c.cond(e, sc, "check"); err != nil {
		return nil, err
	}
	return &Compiled{src, e, c.info}, nil
}

// Eval evaluates the expression. fields are the field values a check
// reads; a missing or nil entry is an absent field. It may load up to
// maxLoad bytes of large values (Evaluator.MaxLoad).
func (c *Compiled) Eval(cat *catalog.Catalog, now time.Time, fields map[string]value.Value, maxLoad int64) (value.Value, error) {
	if fields == nil {
		fields = map[string]value.Value{}
	}
	ev := &Evaluator{Info: c.Info, Graph: catalogGraph{cat}, Now: now, MaxLoad: maxLoad}
	return ev.Eval(c.Expr, &Frame{Fields: fields})
}

// Passes evaluates a check: it passes unless it is false, so a check on
// an optional field that is absent or null passes.
func (c *Compiled) Passes(cat *catalog.Catalog, now time.Time, fields map[string]value.Value, maxLoad int64) (bool, error) {
	v, err := c.Eval(cat, now, fields, maxLoad)
	if err != nil {
		return false, err
	}
	b, isBool := v.(value.Bool)
	if !isBool && !value.IsNull(v) {
		return false, &Error{Pos: c.Expr.Position(), Code: "type", Msg: "a check must be true or false, not " + typeName(v)}
	}
	return !isBool || bool(b), nil
}

// catalogGraph serves expressions that only read field values given to
// them: it knows formats, but not the stored graph.
type catalogGraph struct{ cat *catalog.Catalog }

var errNoGraph = errors.New("defaults and checks can only read the entity's own fields")

func (catalogGraph) Field(value.ID, string) (value.Value, bool, error) {
	return nil, false, errNoGraph
}

func (catalogGraph) FieldMeta(value.ID, string, string) (value.Value, bool, error) {
	return nil, false, errNoGraph
}

func (catalogGraph) Meta(value.ID, string) (value.Value, error) { return nil, errNoGraph }

func (catalogGraph) Degree(value.ID, string, string) (int, error) { return 0, errNoGraph }

func (g catalogGraph) Format(name string) (value.Format, bool) {
	if g.cat == nil {
		return value.LookupFormat(name)
	}
	return g.cat.Format(name)
}
