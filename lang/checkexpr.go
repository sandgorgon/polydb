package lang

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/value"
)

// Metadata names and their types (spec/rhizome.md §2.1).
var (
	entityMeta = map[string]*value.Type{
		"id":      value.TypeID,
		"kind":    value.OptionalOf(value.TypeText), // null for kindless entities
		"created": value.TypeTime,
		"updated": value.TypeTime,
		"version": value.TypeInt,
		"labels":  value.SetOf(value.TypeText),
		"owner":   value.TypeText,
		"props":   value.MapOf(value.TypeText, value.TypeAny),
	}
	fieldMeta = map[string]*value.Type{
		"id":       value.TypeID,
		"name":     value.TypeText,
		"doc":      value.TypeText,
		"type":     value.TypeText,
		"declared": value.TypeBool,
	}
	relMeta = map[string]*value.Type{
		"id":   value.TypeID,
		"type": value.TypeText,
		"from": value.TypeID,
		"to":   value.TypeID,
	}
)

// expr checks an expression and returns its type.
func (c *checker) expr(e Expr, sc *scope) (Static, error) {
	if sc == nil {
		sc = newScope()
	}
	if sc.columns != nil {
		if _, isName := e.(*NameRef); !isName {
			if name, ok := sc.columns[FormatExpr(e)]; ok {
				t := sc.vars[name]
				c.info.Types[e] = t
				c.info.Refs[e] = Ref{RefColumn, name}
				return t, nil
			}
		}
	}
	t, err := c.expr1(e, sc)
	if err != nil {
		return t, err
	}
	c.info.Types[e] = t
	return t, nil
}

func (c *checker) expr1(e Expr, sc *scope) (Static, error) {
	switch e := e.(type) {
	case *Lit:
		return valueOf(typeOfLit(e.Val)), nil
	case *NameRef:
		return c.name(e, sc)
	case *HandleRef:
		return c.handle(e)
	case *ParamRef:
		v, ok := c.env.Params[e.Name]
		if !ok {
			return Static{}, errAt(e.P, "not_found", "parameter $%s isn't set", e.Name)
		}
		return valueOf(valueType(v)), nil
	case *MetaRef:
		if sc.target == nil {
			return Static{}, errAt(e.P, "syntax", "::%s reads metadata of the entity being updated, so it only works in update; write x::%s", e.Name, e.Name)
		}
		return c.meta(e.P, *sc.target, e.Name)
	case *Unary:
		if e.Op == "not" {
			return stBool, c.cond(e.X, sc, "not")
		}
		t, err := c.expr(e.X, sc)
		if err != nil {
			return t, err
		}
		v := value.Required(t.asValue())
		if !dynamic(v) && !isNum(v) && v.Code != value.CodeDuration {
			return t, typeErr(e.P, "- needs a number or duration, not %s", t)
		}
		return valueOf(t.asValue()), nil
	case *Binary:
		return c.binary(e, sc)
	case *Is:
		if _, err := c.expr(e.X, sc); err != nil {
			return Static{}, err
		}
		if e.Kind != "" {
			if _, err := c.kind(e.P, e.Kind); err != nil {
				return Static{}, err
			}
		}
		if e.Type != nil {
			if err := c.typeRefs(e.P, e.Type); err != nil {
				return Static{}, err
			}
		}
		return stBool, nil
	case *Between:
		x, err := c.expr(e.X, sc)
		if err != nil {
			return x, err
		}
		for _, b := range []Expr{e.Lo, e.Hi} {
			t, err := c.expr(b, sc)
			if err != nil {
				return t, err
			}
			if err := comparable(b.Position(), "<", x, t); err != nil {
				return t, err
			}
		}
		return stBool, nil
	case *FieldExpr:
		x, err := c.expr(e.X, sc)
		if err != nil {
			return x, err
		}
		return c.member(e.P, x, e.Name)
	case *MetaExpr:
		return c.metaExpr(e, sc)
	case *IndexExpr:
		x, err := c.expr(e.X, sc)
		if err != nil {
			return x, err
		}
		i, err := c.expr(e.Index, sc)
		if err != nil {
			return i, err
		}
		if x.Form != FormValue {
			return x, typeErr(e.P, "[ ] reads from lists, arrays and maps, and %s is %s", FormatExpr(e.X), x)
		}
		t, err := indexType(e.Index.Position(), x.Type, i)
		return valueOf(t), err
	case *DocPath:
		return c.docPath(e, sc)
	case *Call:
		return c.call(e, sc)
	case *If:
		if err := c.cond(e.Cond, sc, "if"); err != nil {
			return Static{}, err
		}
		a, err := c.expr(e.Then, sc)
		if err != nil {
			return a, err
		}
		b, err := c.expr(e.Else, sc)
		if err != nil {
			return b, err
		}
		if a.Form == FormEntity && b.Form == FormEntity {
			if a.Kind != b.Kind {
				a.Kind = nil
			}
			return a, nil
		}
		return valueOf(unify(a.asValue(), b.asValue())), nil
	case *Cast:
		x, err := c.expr(e.X, sc)
		if err != nil {
			return x, err
		}
		if err := c.typeRefs(e.P, e.Type); err != nil {
			return x, err
		}
		if !castable(x.asValue(), e.Type) {
			return x, typeErr(e.P, "can't cast %s to %s", x, e.Type)
		}
		return valueOf(e.Type), nil
	case *Exists:
		_, err := c.expr(e.X, sc)
		return stBool, err
	case *ListLit:
		elem, err := c.elems(e.Elems, sc)
		return valueOf(value.ListOf(elem)), err
	case *SetLit:
		elem, err := c.elems(e.Elems, sc)
		if err == nil && !dynamic(elem) && !elem.Keyable() {
			err = typeErr(e.P, "set elements must be keyable, and %s isn't", elem)
		}
		return valueOf(value.SetOf(elem)), err
	case *MapLit:
		var k, v *value.Type
		for _, en := range e.Entries {
			kt, err := c.expr(en.Key, sc)
			if err != nil {
				return kt, err
			}
			vt, err := c.expr(en.Value, sc)
			if err != nil {
				return vt, err
			}
			if kv := kt.asValue(); !dynamic(kv) && !kv.Keyable() {
				return kt, typeErr(en.Key.Position(), "map keys must be keyable, and %s isn't", kv)
			}
			k, v = unifyNext(k, kt.asValue()), unifyNext(v, vt.asValue())
		}
		if k == nil {
			k, v = value.TypeAny, value.TypeAny
		}
		return valueOf(value.MapOf(k, v)), nil
	}
	return Static{}, errAt(e.Position(), "internal", "checker: unexpected expression %T", e)
}

func unifyNext(acc, t *value.Type) *value.Type {
	if acc == nil {
		return t
	}
	return unify(acc, t)
}

// elems checks the elements of a list or set literal and returns their
// common type.
func (c *checker) elems(es []Expr, sc *scope) (*value.Type, error) {
	var elem *value.Type
	for _, x := range es {
		t, err := c.expr(x, sc)
		if err != nil {
			return nil, err
		}
		elem = unifyNext(elem, t.asValue())
	}
	if elem == nil {
		return value.TypeAny, nil
	}
	return elem, nil
}

// valueType returns the type of a parameter value. Collections are typed
// loosely; their contents are checked when used.
func valueType(v value.Value) *value.Type {
	switch v := v.(type) {
	case value.List:
		return value.ListOf(value.TypeAny)
	case value.Set:
		return value.SetOf(value.TypeAny)
	case value.Map:
		return value.MapOf(value.TypeAny, value.TypeAny)
	case value.Doc:
		return value.DocOf(v.Format())
	}
	if value.IsNull(v) {
		return typeNull
	}
	switch c := v.Code(); {
	case c <= value.CodeImage:
		return &value.Type{Code: c}
	}
	return value.TypeAny
}

// name resolves a bare name: a variable, a field of the entity being
// updated or checked, or (after group by) a mistake worth explaining.
func (c *checker) name(e *NameRef, sc *scope) (Static, error) {
	n := e.Name
	if t, ok := sc.lookup(n); ok {
		if sc.target != nil && sc.target.Kind != nil {
			if _, isField := sc.target.Kind.Field(n); isField {
				return t, errAt(e.P, "syntax", "%s is both a variable and a field of the entity being updated; write x.%s for the field", n, n)
			}
		}
		c.info.Refs[e] = Ref{RefVar, n}
		return t, nil
	}
	if sc.grouped && !sc.inAgg && sc.pre != nil {
		if _, ok := sc.pre.lookup(n); ok {
			return Static{}, errAt(e.P, "syntax", "%s isn't a group name: use it inside an aggregate such as collect(%s), or group by it", n, n)
		}
	}
	if t, ok := sc.fields[n]; ok {
		c.info.Refs[e] = Ref{RefField, n}
		return valueOf(t), nil
	}
	if sc.target != nil {
		t, err := fieldOf(e.P, sc.target.Kind, n, sc.added)
		if err != nil {
			return Static{}, err
		}
		c.info.Refs[e] = Ref{RefField, n}
		return valueOf(t), nil
	}
	if sc.noRows != "" {
		return Static{}, errAt(e.P, "syntax", "%s needs a constant value, so it can't use %s", sc.noRows, n)
	}
	if sc.fields != nil {
		return Static{}, errAt(e.P, "unknown_field", "no field %s%s", n, didYouMean(n, sc.names()))
	}
	return Static{}, errAt(e.P, "not_found", "no variable %s%s", n, didYouMean(n, sc.names()))
}

// cond checks a condition: a bool (or a value only known at run time).
func (c *checker) cond(e Expr, sc *scope, what string) error {
	t, err := c.expr(e, sc)
	if err != nil {
		return err
	}
	v := value.Required(t.asValue())
	if !dynamic(v) && v.Code != value.CodeBool {
		return typeErr(e.Position(), "%s needs a condition (true or false), not %s", what, t)
	}
	return nil
}

// ===== Members, metadata, indexes =====

// member returns the type of x.name.
func (c *checker) member(at Pos, x Static, name string) (Static, error) {
	switch x.Form {
	case FormEntity:
		t, err := fieldOf(at, x.Kind, name, nil)
		return valueOf(t), err
	case FormRelation:
		if x.Rel == nil {
			return stAny, nil
		}
		fd, ok := x.Rel.Prop(name)
		if !ok {
			return Static{}, errAt(at, "unknown_field", "relation %s has no property %s%s", x.Rel.Name, name, didYouMean(name, fieldNames(x.Rel.Props)))
		}
		return valueOf(value.OptionalOf(fd.Type)), nil
	case FormNode:
		return stAny, nil
	case FormPath:
		return Static{}, typeErr(at, "paths have no fields; use nodes(path) or rels(path)")
	}
	if r := value.Required(x.Type); r.Code == value.CodeRef && r.Name != "" && c.env.Catalog != nil {
		// Reading a field through a ref follows it to the entity.
		k, ok := c.env.Catalog.Kind(r.Name)
		if !ok {
			return stAny, nil
		}
		t, err := fieldOf(at, k, name, nil)
		if err != nil {
			return Static{}, err
		}
		return valueOf(value.OptionalOf(t)), nil
	}
	t, err := memberType(at, x.Type, name)
	return valueOf(t), err
}

// memberType returns the type of v.name for a value of type t: a record
// field, a map entry with a text key, or a field read through a ref.
func memberType(at Pos, t *value.Type, name string) (*value.Type, error) {
	r := value.Required(t)
	switch {
	case dynamic(r) || r.Code == value.CodeRef:
		return value.TypeAny, nil
	case r.Code == value.CodeRecord:
		ft, ok := recordField(r, name)
		if !ok {
			return nil, errAt(at, "unknown_field", "the record has no field %s%s", name, didYouMean(name, recordNames(r)))
		}
		if t.Optional {
			return value.OptionalOf(ft), nil
		}
		return ft, nil
	case r.Code == value.CodeMap && (r.Key.Code == value.CodeText || dynamic(r.Key)):
		return value.OptionalOf(r.Elem), nil
	}
	return nil, typeErr(at, ".%s reads fields of entities, records and maps, and this is %s", name, t)
}

// indexType returns the type of x[i].
func indexType(at Pos, x *value.Type, i Static) (*value.Type, error) {
	r := value.Required(x)
	it := value.Required(i.asValue())
	switch {
	case dynamic(r):
		return value.TypeAny, nil
	case isSeq(r.Code):
		if !dynamic(it) && it.Code != value.CodeInt {
			return nil, typeErr(at, "list positions are ints, not %s", i)
		}
		return value.OptionalOf(r.Elem), nil
	case r.Code == value.CodeMap:
		if !assignable(i.asValue(), r.Key) {
			return nil, typeErr(at, "this map's keys are %s, not %s", r.Key, i)
		}
		return value.OptionalOf(r.Elem), nil
	case r.Code == value.CodeText:
		return nil, typeErr(at, "[ ] doesn't index text; use substr(t, start, n)")
	}
	return nil, typeErr(at, "[ ] reads from lists, arrays and maps, not %s", x)
}

func (c *checker) meta(at Pos, x Static, name string) (Static, error) {
	table, what := entityMeta, "entities"
	switch x.Form {
	case FormRelation:
		table, what = relMeta, "relations"
	case FormNode:
		for _, m := range []map[string]*value.Type{entityMeta, relMeta, fieldMeta} {
			if t, ok := m[name]; ok {
				return valueOf(t), nil
			}
		}
	case FormEntity:
	default:
		return Static{}, typeErr(at, "::%s reads metadata of entities, fields and relations, not %s", name, x)
	}
	if t, ok := table[name]; ok {
		return valueOf(t), nil
	}
	return Static{}, errAt(at, "not_found", "%s have no metadata ::%s%s", what, name, didYouMean(name, slices.Collect(maps.Keys(table))))
}

// metaExpr checks x::name. When x is an entity's field (p.name::doc), the
// metadata is the field's.
func (c *checker) metaExpr(e *MetaExpr, sc *scope) (Static, error) {
	if fx, ok := e.X.(*FieldExpr); ok {
		owner, err := c.expr(fx.X, sc)
		if err != nil {
			return owner, err
		}
		if owner.Form == FormEntity || owner.Form == FormNode {
			ft, err := c.member(fx.P, owner, fx.Name)
			if err != nil {
				return ft, err
			}
			c.info.Types[fx] = ft
			if t, ok := fieldMeta[e.Name]; ok {
				return valueOf(t), nil
			}
			return Static{}, errAt(e.P, "not_found", "fields have no metadata ::%s%s", e.Name, didYouMean(e.Name, slices.Collect(maps.Keys(fieldMeta))))
		}
	}
	x, err := c.expr(e.X, sc)
	if err != nil {
		return x, err
	}
	return c.meta(e.P, x, e.Name)
}

// ===== Documents =====

// docPath checks a chain of -> steps.
func (c *checker) docPath(e *DocPath, sc *scope) (Static, error) {
	var steps []DocStep
	var base Expr = e
	for {
		d, ok := base.(*DocPath)
		if !ok {
			break
		}
		steps = append(steps, d.Step)
		base = d.X
	}
	slices.Reverse(steps)
	bt, err := c.expr(base, sc)
	if err != nil {
		return bt, err
	}
	return c.docSteps(e.P, bt, steps, sc)
}

// docSteps checks -> steps from a value of type base (spec/rhizome.md
// §8.6). Steps into a json-based derived format follow its shape and give
// typed results; other formats give any.
func (c *checker) docSteps(at Pos, base Static, steps []DocStep, sc *scope) (Static, error) {
	bt := value.Required(base.asValue())
	for _, st := range steps {
		if st.Index != nil {
			if _, err := c.expr(st.Index, sc); err != nil {
				return Static{}, err
			}
		}
	}
	if dynamic(bt) {
		return stAny, nil
	}
	if bt.Code != value.CodeDoc {
		return Static{}, typeErr(at, "-> reads inside documents, not %s", base)
	}
	baseFormat := bt.Name
	var shape *value.Type
	if c.env.Catalog != nil {
		if def, ok := c.env.Catalog.FormatDef(bt.Name); ok {
			baseFormat, shape = def.Base, def.Shape()
		}
	}
	for _, st := range steps {
		if err := c.docStepForm(at, baseFormat, st); err != nil {
			return Static{}, err
		}
	}
	if shape == nil || baseFormat != "json" {
		return stAny, nil
	}
	t := shape
	opt := base.asValue().Optional
	for _, st := range steps {
		r := value.Required(t)
		opt = opt || t.Optional
		switch {
		case dynamic(r):
			return stAny, nil
		case st.Index == nil:
			var err error
			if t, err = memberType(at, r, st.Name); err != nil {
				if e, ok := err.(*Error); ok && e.Code == "unknown_field" {
					e.Msg = fmt.Sprintf("format %s has no field %s here%s", bt.Name, st.Name, didYouMean(st.Name, recordNames(r)))
				}
				return Static{}, err
			}
		default:
			if r.Code == value.CodeRecord {
				if l, ok := st.Index.(*Lit); ok {
					if s, ok := l.Val.(value.Text); ok {
						var err error
						if t, err = memberType(at, r, s.Go()); err != nil {
							return Static{}, err
						}
						continue
					}
				}
				return Static{}, typeErr(at, "format %s has a record here: step into it by name", bt.Name)
			}
			var err error
			if t, err = indexType(st.Index.Position(), r, c.info.Types[st.Index]); err != nil {
				return Static{}, err
			}
		}
	}
	if opt {
		t = value.OptionalOf(t)
	}
	return valueOf(t), nil
}

// docStepForm checks that a step is one the base format defines.
func (c *checker) docStepForm(at Pos, format string, st DocStep) error {
	switch format {
	case "json":
		if st.Attr {
			return typeErr(at, "@%s is an XML attribute step; json documents have keys and positions", st.Name)
		}
		if st.Index != nil {
			t := value.Required(c.info.Types[st.Index].asValue())
			if !dynamic(t) && t.Code != value.CodeInt && t.Code != value.CodeText {
				return typeErr(st.Index.Position(), "json steps take a key (text) or a position (int), not %s", t)
			}
		}
	case "xml", "csv", "tsv":
		if st.Attr && format != "xml" {
			return typeErr(at, "@%s is an XML attribute step; %s documents have rows and columns", st.Name, format)
		}
		if st.Index != nil {
			t := value.Required(c.info.Types[st.Index].asValue())
			if !dynamic(t) && t.Code != value.CodeInt {
				return typeErr(st.Index.Position(), "%s position steps take an int, not %s", format, t)
			}
		}
	}
	return nil
}

// ===== Operators =====

func (c *checker) binary(e *Binary, sc *scope) (Static, error) {
	if e.Op == "and" || e.Op == "or" {
		if err := c.cond(e.X, sc, e.Op); err != nil {
			return Static{}, err
		}
		return stBool, c.cond(e.Y, sc, e.Op)
	}
	x, err := c.expr(e.X, sc)
	if err != nil {
		return x, err
	}
	y, err := c.expr(e.Y, sc)
	if err != nil {
		return y, err
	}
	xv, yv := x.asValue(), y.asValue()
	switch e.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		return stBool, comparable(e.P, e.Op, x, y)
	case "in", "not in":
		return stBool, membership(e.P, e.Op, y, x)
	case "has":
		return stBool, membership(e.P, e.Op, x, y)
	case "contains":
		if r := value.Required(xv); r.Code == value.CodeText {
			return stBool, needText(e.P, e.Op, y)
		}
		return stBool, membership(e.P, e.Op, x, y)
	case "starts with", "ends with", "matches":
		if err := needText(e.P, e.Op, x); err != nil {
			return stBool, err
		}
		if err := needText(e.P, e.Op, y); err != nil {
			return stBool, err
		}
		if l, ok := e.Y.(*Lit); ok && e.Op == "matches" {
			if s, ok := l.Val.(value.Text); ok {
				if _, err := regexp.Compile(s.Go()); err != nil {
					return stBool, typeErr(l.P, "invalid regular expression: %v", err)
				}
			}
		}
		return stBool, nil
	case "+", "-", "*", "/", "%":
		t, err := arith(e.P, e.Op, xv, yv, e.X, e.Y)
		return valueOf(t), err
	case "++", "--":
		t, err := join(e.P, e.Op, xv, yv)
		return valueOf(t), err
	case "=>":
		return valueOf(value.PairOf(xv, yv)), nil
	}
	return Static{}, errAt(e.P, "internal", "checker: unknown operator %s", e.Op)
}

func needText(at Pos, op string, x Static) error {
	r := value.Required(x.asValue())
	if dynamic(r) || r.Code == value.CodeText {
		return nil
	}
	return typeErr(at, "%s works on text, not %s", op, x)
}

// comparable checks x op y for = != < <= > >=.
func comparable(at Pos, op string, x, y Static) error {
	if x.Form == FormPath || y.Form == FormPath {
		return typeErr(at, "paths can't be compared; compare len(path) or nodes(path)")
	}
	a, b := value.Required(x.asValue()), value.Required(y.asValue())
	if dynamic(a) || dynamic(b) {
		return nil
	}
	if cmpGroup(a.Code) == cmpGroup(b.Code) {
		return nil
	}
	hint := ""
	for _, p := range [][2]*value.Type{{a, b}, {b, a}} {
		if p[1].Code == value.CodeText {
			switch p[0].Code {
			case value.CodeDate:
				hint = `; write a date as d"2026-01-31"`
			case value.CodeTime:
				hint = `; write a time as t"2026-01-31T12:00:00Z"`
			case value.CodeDuration:
				hint = "; write a duration as 90s or 1h30m"
			case value.CodeID, value.CodeRef:
				hint = "; write an id as #01JD8X3K7M0000Q2W5ZB9N4T6R"
			}
		}
	}
	return typeErr(at, "can't compare %s with %s using %s%s", x, y, op, hint)
}

// membership checks a membership test: elem in coll (or coll has elem).
func membership(at Pos, op string, coll, elem Static) error {
	ct := value.Required(coll.asValue())
	var et *value.Type
	switch {
	case dynamic(ct):
		return nil
	case isSeq(ct.Code) || ct.Code == value.CodeSet || ct.Code.IsQueue():
		et = ct.Elem
	case ct.Code == value.CodeMap:
		et = ct.Key
	case ct.Code == value.CodeText && op == "in":
		return typeErr(at, "in tests membership of collections; for text use contains")
	default:
		return typeErr(at, "%s needs a list, set or map, not %s", op, coll)
	}
	if err := comparable(at, op, valueOf(et), elem); err != nil {
		return typeErr(at, "%s: the collection holds %s, not %s", op, et, elem)
	}
	return nil
}

// arith types x op y for + - * / % (spec/types.md §10.3). xe and ye, if
// given, are used in hints.
func arith(at Pos, op string, a, b *value.Type, xe, ye Expr) (*value.Type, error) {
	x, y := value.Required(a), value.Required(b)
	if x.Code == value.CodeNull || y.Code == value.CodeNull {
		other := x
		if other.Code == value.CodeNull {
			other = y
		}
		if other.Code == value.CodeNull || dynamic(other) {
			return value.TypeAny, nil
		}
		return value.OptionalOf(other), nil
	}
	if dynamic(x) || dynamic(y) {
		return value.TypeAny, nil
	}
	res := func(t *value.Type) (*value.Type, error) {
		if a.Optional || b.Optional {
			return value.OptionalOf(t), nil
		}
		return t, nil
	}
	if isNum(x) && isNum(y) {
		fx, fy := x.Code == value.CodeFloat, y.Code == value.CodeFloat
		switch {
		case fx && fy:
			return res(value.TypeFloat)
		case fx || fy:
			exact, side := y, ye
			if fy {
				exact, side = x, xe
			}
			what := "the " + exact.String() + " side"
			if side != nil {
				what = FormatExpr(side)
			}
			return nil, typeErr(at, "%s and float don't mix (int ↔ float needs a cast): write cast(%s as float)", exact, what)
		}
		t := x
		if exactRank(y.Code) > exactRank(x.Code) {
			t = y
		}
		if t.Code == value.CodeDecimal {
			t = value.TypeDecimal
		}
		return res(t)
	}
	d, tm := value.CodeDuration, value.CodeTime
	switch op {
	case "+":
		switch {
		case x.Code == tm && y.Code == d, x.Code == d && y.Code == tm:
			return res(value.TypeTime)
		case x.Code == d && y.Code == d:
			return res(value.TypeDuration)
		}
	case "-":
		switch {
		case x.Code == tm && y.Code == d:
			return res(value.TypeTime)
		case x.Code == tm && y.Code == tm, x.Code == d && y.Code == d:
			return res(value.TypeDuration)
		}
	case "*":
		if x.Code == d && y.Code == value.CodeInt || x.Code == value.CodeInt && y.Code == d {
			return res(value.TypeDuration)
		}
	case "/":
		if x.Code == d && y.Code == value.CodeInt {
			return res(value.TypeDuration)
		}
	}
	if op == "+" {
		for _, t := range []*value.Type{x, y} {
			switch t.Code {
			case value.CodeText, value.CodeBytes, value.CodeList, value.CodeSet, value.CodeMap, value.CodeArray:
				return nil, typeErr(at, "+ adds numbers; use ++ to join %s", t)
			}
		}
	}
	return nil, typeErr(at, "can't use %s with %s and %s", op, a, b)
}

// join types x ++ y and x -- y.
func join(at Pos, op string, a, b *value.Type) (*value.Type, error) {
	x, y := value.Required(a), value.Required(b)
	if dynamic(x) || dynamic(y) {
		return value.TypeAny, nil
	}
	res := func(t *value.Type) (*value.Type, error) {
		if a.Optional || b.Optional {
			return value.OptionalOf(t), nil
		}
		return t, nil
	}
	if op == "++" {
		switch {
		case isNum(x) || isNum(y):
			return nil, typeErr(at, "++ joins text, bytes and collections; use + to add numbers")
		case x.Code == y.Code && (x.Code == value.CodeText || x.Code == value.CodeBytes):
			return res(x)
		case isSeq(x.Code) && isSeq(y.Code):
			return res(value.ListOf(unify(x.Elem, y.Elem)))
		case x.Code == value.CodeSet && y.Code == value.CodeSet:
			return res(value.SetOf(unify(x.Elem, y.Elem)))
		case x.Code == value.CodeMap && y.Code == value.CodeMap:
			return res(value.MapOf(unify(x.Key, y.Key), unify(x.Elem, y.Elem)))
		}
		return nil, typeErr(at, "can't join %s and %s with ++", a, b)
	}
	switch {
	case isNum(x) || isNum(y):
		return nil, typeErr(at, "-- removes from lists, sets and maps; use - to subtract numbers")
	case isSeq(x.Code) && (isSeq(y.Code) || y.Code == value.CodeSet):
		return res(value.ListOf(x.Elem))
	case x.Code == value.CodeSet && (y.Code == value.CodeSet || isSeq(y.Code)):
		return res(x)
	case x.Code == value.CodeMap && (y.Code == value.CodeSet || isSeq(y.Code)):
		return res(x)
	}
	return nil, typeErr(at, "can't remove %s from %s with --", b, a)
}

// castable reports whether cast(x as to) is allowed for x of type from
// (spec/types.md §10.2).
func castable(from, to *value.Type) bool {
	f, t := value.Required(from), value.Required(to)
	if dynamic(f) || t.Code == value.CodeAny || assignable(f, t) {
		return true
	}
	scalar := func(c value.Code, cs ...value.Code) bool { return slices.Contains(cs, c) }
	text := value.CodeText
	switch {
	case isNum(f) && isNum(t):
		return true
	case isNum(f) && t.Code == text, f.Code == text && isNum(t):
		return true
	case f.Code == text && scalar(t.Code, value.CodeTime, value.CodeDate, value.CodeDuration, value.CodeID,
		value.CodeRef, value.CodeDoc, value.CodeBytes, value.CodeBool):
		return true
	case t.Code == text && scalar(f.Code, value.CodeTime, value.CodeDate, value.CodeDuration, value.CodeID,
		value.CodeRef, value.CodeDoc, value.CodeBytes, value.CodeBool):
		return true
	case scalar(f.Code, value.CodeID, value.CodeRef) && scalar(t.Code, value.CodeID, value.CodeRef):
		return true
	case scalar(f.Code, value.CodeTime, value.CodeDate) && scalar(t.Code, value.CodeTime, value.CodeDate):
		return true
	case f.Code == value.CodeDoc:
		return scalar(t.Code, value.CodeRecord, value.CodeMap, value.CodeList)
	case (isSeq(f.Code) || f.Code == value.CodeSet) && (isSeq(t.Code) || t.Code == value.CodeSet):
		return castable(f.Elem, t.Elem)
	case f.Code == value.CodeMap && t.Code == value.CodeRecord, f.Code == value.CodeRecord && t.Code == value.CodeMap:
		return true
	}
	return false
}

// ===== Writes =====

// write checks that e can be stored where type dst is expected. Literals
// are converted as spec/types.md §10.1 allows (text to time, date,
// duration, id or doc; an ID to a ref), and list, set and map literals
// are checked element by element.
func (c *checker) write(e Expr, dst *value.Type, sc *scope) error {
	d := value.Required(dst)
	switch x := e.(type) {
	case *Lit:
		if ok, err := c.convertLit(x, dst); ok || err != nil {
			return err
		}
	case *ListLit:
		var elem *value.Type
		switch {
		case d.Code == value.CodeList || d.Code == value.CodeArray ||
			d.Code == value.CodeFifo || d.Code == value.CodeLifo || d.Code == value.CodeRing:
			elem = d.Elem
		case d.Code == value.CodePriority:
			elem = value.PairOf(value.TypeInt, d.Elem)
		}
		if elem != nil {
			if d.Code == value.CodeArray && len(x.Elems) != d.N {
				return typeErr(x.P, "%s needs exactly %d elements, not %d", d, d.N, len(x.Elems))
			}
			for _, el := range x.Elems {
				if err := c.write(el, elem, sc); err != nil {
					return err
				}
			}
			c.info.Types[e] = valueOf(dst)
			return nil
		}
	case *SetLit:
		if d.Code == value.CodeSet {
			for _, el := range x.Elems {
				if err := c.write(el, d.Elem, sc); err != nil {
					return err
				}
			}
			c.info.Types[e] = valueOf(dst)
			return nil
		}
	case *MapLit:
		switch {
		case d.Code == value.CodeMap:
			for _, en := range x.Entries {
				if err := c.write(en.Key, d.Key, sc); err != nil {
					return err
				}
				if err := c.write(en.Value, d.Elem, sc); err != nil {
					return err
				}
			}
			c.info.Types[e] = valueOf(dst)
			return nil
		case d.Code == value.CodeSet && len(x.Entries) == 0:
			c.info.Types[e] = valueOf(dst)
			return nil
		case d.Code == value.CodeRecord:
			return c.writeRecord(x, dst, sc)
		}
	case *Binary:
		if x.Op == "=>" && d.Code == value.CodePair {
			if err := c.write(x.X, d.Key, sc); err != nil {
				return err
			}
			if err := c.write(x.Y, d.Elem, sc); err != nil {
				return err
			}
			c.info.Types[e] = valueOf(dst)
			return nil
		}
	}
	t, err := c.expr(e, sc)
	if err != nil {
		return err
	}
	if t.Form == FormRelation || t.Form == FormPath {
		if !dynamic(d) {
			return typeErr(e.Position(), "can't store %s as %s", t, dst)
		}
	}
	if !assignable(t.asValue(), dst) {
		return typeErr(e.Position(), "%s can't be stored as %s%s", describe(e, t), dst, writeHint(t.asValue(), d))
	}
	return nil
}

func describe(e Expr, t Static) string {
	if _, ok := e.(*Lit); ok {
		return FormatExpr(e) + " (" + t.String() + ")"
	}
	return t.String()
}

func writeHint(src, dst *value.Type) string {
	s := value.Required(src)
	switch {
	case s.Code == value.CodeInt && dst.Code == value.CodeFloat:
		return ": write a float like 3.0, or cast(x as float)"
	case s.Code == value.CodeFloat && exactRank(dst.Code) >= 0:
		return ": floats don't convert implicitly; use cast(x as " + dst.String() + ")"
	case s.Code == value.CodeList && dst.Code == value.CodeSet:
		return `: write a set as {"a", "b"}`
	case src.Optional && !dst.Nullable():
		return ": the value may be null"
	}
	return ""
}

// writeRecord checks a map literal written to a record type.
func (c *checker) writeRecord(m *MapLit, dst *value.Type, sc *scope) error {
	r := value.Required(dst)
	seen := map[string]bool{}
	for _, en := range m.Entries {
		name, ok := entryName(en)
		if !ok {
			return typeErr(en.Key.Position(), "record fields are named with names or text")
		}
		ft, ok := recordField(r, name)
		if !ok {
			return errAt(en.Key.Position(), "unknown_field", "the record has no field %s%s", name, didYouMean(name, recordNames(r)))
		}
		if seen[name] {
			return errAt(en.Key.Position(), "constraint", "field %s is given twice", name)
		}
		seen[name] = true
		c.info.Types[en.Key] = stText
		if err := c.write(en.Value, ft, sc); err != nil {
			return err
		}
	}
	for _, f := range r.Fields {
		if !seen[f.Name] && !f.Type.Nullable() {
			return typeErr(m.P, "the record needs field %s", f.Name)
		}
	}
	c.info.Types[m] = valueOf(dst)
	return nil
}

// convertLit converts a literal for a write where spec/types.md §10.1
// allows it. It reports whether it handled the literal.
func (c *checker) convertLit(l *Lit, dst *value.Type) (bool, error) {
	d := value.Required(dst)
	var out value.Value
	var err error
	switch v := l.Val.(type) {
	case value.Text:
		s := v.Go()
		switch d.Code {
		case value.CodeTime:
			out, err = value.ParseTime(s)
		case value.CodeDate:
			out, err = value.ParseDate(s)
		case value.CodeDuration:
			var dur time.Duration
			if dur, err = time.ParseDuration(s); err == nil {
				out = value.Duration(dur)
			}
		case value.CodeID:
			out, err = value.ParseID(s)
		case value.CodeDoc:
			f, ok := value.Format(nil), false
			if c.env.Catalog != nil {
				f, ok = c.env.Catalog.Format(d.Name)
			} else {
				f, ok = value.LookupFormat(d.Name)
			}
			if !ok {
				return true, errAt(l.P, "not_found", "no format %s", d.Name)
			}
			out, err = value.NewDoc(f, s)
		default:
			return false, nil
		}
	case value.ID:
		if d.Code != value.CodeRef {
			return false, nil
		}
		out = value.Ref{Target: v}
	default:
		return false, nil
	}
	if err != nil {
		return true, typeErr(l.P, "%s isn't a valid %s: %v", FormatExpr(l), d, err)
	}
	c.info.Consts[l] = out
	c.info.Types[l] = valueOf(d)
	return true, nil
}
