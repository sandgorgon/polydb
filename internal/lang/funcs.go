package lang

import (
	"maps"
	"slices"
	"strconv"

	"github.com/sandgorgon/polydb/internal/fulltext"
	"github.com/sandgorgon/polydb/value"
)

// fn describes a built-in function's signature (spec/rhizome.md §8.4).
type fn struct {
	min, max int // argument counts; max -1 means any number
	check    func(c *checker, call *Call, args []Static) (Static, error)
}

// args are the call's arguments, for the check functions.
type argList struct {
	c    *checker
	call *Call
	args []Static
}

// need checks argument i against ok, and returns its value type.
func (a argList) need(i int, what string, ok func(*value.Type) bool) (*value.Type, error) {
	t := a.args[i].asValue()
	r := value.Required(t)
	if dynamic(r) || ok(r) {
		return t, nil
	}
	return nil, typeErr(a.call.Args[i].Position(), "%s needs %s as argument %d, not %s", a.call.Name, what, i+1, a.args[i])
}

func is(codes ...value.Code) func(*value.Type) bool {
	return func(t *value.Type) bool { return slices.Contains(codes, t.Code) }
}

func numeric(t *value.Type) bool { return isNum(t) }

func collection(t *value.Type) bool {
	return isSeq(t.Code) || t.Code == value.CodeSet || t.Code == value.CodeMap
}

func queue(t *value.Type) bool { return t.Code.IsQueue() }

// opt makes t optional when any of the arguments is.
func (a argList) opt(t *value.Type) Static {
	for _, s := range a.args {
		if s.Form == FormValue && (s.Type.Optional || s.Type.Code == value.CodeNull) {
			return valueOf(value.OptionalOf(t))
		}
	}
	return valueOf(t)
}

// elemOf returns the element type of a collection or queue, or any.
func elemOf(t *value.Type) *value.Type {
	r := value.Required(t)
	if dynamic(r) || r.Elem == nil {
		return value.TypeAny
	}
	return r.Elem
}

// sigNamed builds a simple fn: one check per argument, described by
// whats in messages, and a result type.
func sigNamed(result *value.Type, whats []string, params ...func(*value.Type) bool) *fn {
	return &fn{min: len(params), max: len(params), check: func(c *checker, call *Call, args []Static) (Static, error) {
		a := argList{c, call, args}
		for i, p := range params {
			what := whats[i]
			if what == "" {
				what = "a different type"
			}
			if _, err := a.need(i, what, p); err != nil {
				return Static{}, err
			}
		}
		return a.opt(result), nil
	}}
}

var (
	textT    = is(value.CodeText)
	intT     = is(value.CodeInt)
	timeT    = is(value.CodeTime)
	timeDate = is(value.CodeTime, value.CodeDate)
)

// textFn is a text function: text arguments, then ints.
func textFn(result *value.Type, nText, nInt int) *fn {
	var ps []func(*value.Type) bool
	var whats []string
	for range nText {
		ps, whats = append(ps, textT), append(whats, "text")
	}
	for range nInt {
		ps, whats = append(ps, intT), append(whats, "an int")
	}
	return sigNamed(result, whats, ps...)
}

// sameNum is a numeric function whose result has its argument's type.
func sameNum(extra ...func(*value.Type) bool) *fn {
	return &fn{min: 1, max: 1 + len(extra), check: func(c *checker, call *Call, args []Static) (Static, error) {
		a := argList{c, call, args}
		t, err := a.need(0, "a number", numeric)
		if err != nil {
			return Static{}, err
		}
		for i := range extra {
			if i+1 < len(args) {
				if _, err := a.need(i+1, "an int", extra[i]); err != nil {
					return Static{}, err
				}
			}
		}
		if r := value.Required(t); r.Code == value.CodeDecimal {
			t = value.TypeDecimal // rounding changes the scale
		}
		return valueOf(t), nil
	}}
}

var funcs map[string]*fn

func init() {
	funcs = map[string]*fn{
		// General
		"typeof": {1, 1, func(*checker, *Call, []Static) (Static, error) { return stText, nil }},
		"coalesce": {1, -1, func(c *checker, call *Call, args []Static) (Static, error) {
			var t *value.Type
			required := false
			for _, s := range args {
				v := s.asValue()
				t = unifyNext(t, v)
				required = required || !v.Nullable()
			}
			if required {
				t = value.Required(t)
			}
			return valueOf(t), nil
		}},
		"id": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			if args[0].Form != FormValue {
				return stID, nil
			}
			_, err := argList{c, call, args}.need(0, "an entity, relation, ref or id", is(value.CodeRef, value.CodeID))
			return stID, err
		}},
		// Text
		"len": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			if args[0].Form == FormPath {
				return stInt, nil
			}
			a := argList{c, call, args}
			_, err := a.need(0, "text, bytes, a collection, a queue or a path", func(t *value.Type) bool {
				return t.Code == value.CodeText || t.Code == value.CodeBytes || collection(t) || queue(t)
			})
			return a.opt(value.TypeInt), err
		}},
		"lower":   textFn(value.TypeText, 1, 0),
		"upper":   textFn(value.TypeText, 1, 0),
		"trim":    textFn(value.TypeText, 1, 0),
		"split":   textFn(value.ListOf(value.TypeText), 2, 0),
		"replace": textFn(value.TypeText, 3, 0),
		"substr": {2, 3, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			if _, err := a.need(0, "text", textT); err != nil {
				return Static{}, err
			}
			for i := 1; i < len(args); i++ {
				if _, err := a.need(i, "an int", intT); err != nil {
					return Static{}, err
				}
			}
			return a.opt(value.TypeText), nil
		}},
		"join": sigNamed(value.TypeText, []string{"a list or set of text", "text"}, func(t *value.Type) bool {
			if !isSeq(t.Code) && t.Code != value.CodeSet {
				return false
			}
			e := value.Required(t.Elem)
			return dynamic(e) || e.Code == value.CodeText
		}, textT),
		"search": {2, 2, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			for i := range 2 {
				if _, err := a.need(i, "text", textT); err != nil {
					return Static{}, err
				}
			}
			// A constant query is checked now, so its mistakes show
			// before anything runs.
			if lit, ok := call.Args[1].(*Lit); ok {
				if q, ok := lit.Val.(value.Text); ok {
					if _, err := fulltext.Parse(q.Go()); err != nil {
						return Static{}, errAt(lit.P, "syntax", "%v", err)
					}
				}
			}
			return a.opt(value.TypeBool), nil
		}},
		// Numbers
		"abs":   sameNum(),
		"round": sameNum(intT),
		"floor": sameNum(),
		"ceil":  sameNum(),
		"sqrt": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			if t := value.Required(args[0].asValue()); isNum(t) && t.Code != value.CodeFloat {
				return Static{}, typeErr(call.Args[0].Position(), "sqrt works on floats: write sqrt(cast(%s as float))", FormatExpr(call.Args[0]))
			}
			_, err := a.need(0, "a float", is(value.CodeFloat))
			return a.opt(value.TypeFloat), err
		}},
		"pow": {2, 2, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			x, err := a.need(0, "a number", numeric)
			if err != nil {
				return Static{}, err
			}
			y, err := a.need(1, "a number", numeric)
			if err != nil {
				return Static{}, err
			}
			xr, yr := value.Required(x), value.Required(y)
			switch {
			case dynamic(xr) || dynamic(yr):
				return stAny, nil
			case xr.Code == value.CodeFloat && yr.Code == value.CodeFloat:
				return a.opt(value.TypeFloat), nil
			case xr.Code != value.CodeFloat && yr.Code == value.CodeInt:
				if xr.Code == value.CodeDecimal {
					xr = value.TypeDecimal
				}
				return a.opt(xr), nil
			}
			return Static{}, typeErr(call.P, "pow takes (float, float) or an exact number and an int exponent, not (%s, %s)", x, y)
		}},
		"min": {2, -1, minMax},
		"max": {2, -1, minMax},
		// Time
		"now":   {0, 0, func(*checker, *Call, []Static) (Static, error) { return stTime, nil }},
		"today": {0, 0, func(*checker, *Call, []Static) (Static, error) { return stDate, nil }},
		"year":  sigNamed(value.TypeInt, []string{"a time or date"}, timeDate),
		"month": sigNamed(value.TypeInt, []string{"a time or date"}, timeDate),
		"day":   sigNamed(value.TypeInt, []string{"a time or date"}, timeDate),
		"add_months": {2, 2, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a time or date", timeDate)
			if err != nil {
				return Static{}, err
			}
			if _, err := a.need(1, "an int", intT); err != nil {
				return Static{}, err
			}
			return a.opt(value.Required(t)), nil
		}},
		"truncate": sigNamed(value.TypeTime, []string{"a time", "a duration"}, timeT, is(value.CodeDuration)),
		// Collections
		"keys": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a map", is(value.CodeMap))
			if err != nil {
				return Static{}, err
			}
			if r := value.Required(t); r.Code == value.CodeMap {
				return a.opt(value.ListOf(r.Key)), nil
			}
			return stAny, nil
		}},
		"values": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a map", is(value.CodeMap))
			if err != nil {
				return Static{}, err
			}
			return a.opt(value.ListOf(elemOf(t))), nil
		}},
		"first": seqFn(func(e *value.Type) *value.Type { return value.OptionalOf(e) }),
		"last":  seqFn(func(e *value.Type) *value.Type { return value.OptionalOf(e) }),
		"sort":  seqFn(value.ListOf),
		"slice": {3, 3, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a list or array", func(t *value.Type) bool { return isSeq(t.Code) })
			if err != nil {
				return Static{}, err
			}
			for i := 1; i < 3; i++ {
				if _, err := a.need(i, "an int", intT); err != nil {
					return Static{}, err
				}
			}
			return a.opt(value.ListOf(elemOf(t))), nil
		}},
		"contains": {2, 2, func(c *checker, call *Call, args []Static) (Static, error) {
			if r := value.Required(args[0].asValue()); r.Code == value.CodeText {
				return stBool, needText(call.Args[1].Position(), "contains", args[1])
			}
			return stBool, membership(call.P, "contains", args[0], args[1])
		}},
		// Queues
		"peek": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			t, err := argList{c, call, args}.need(0, "a queue", queue)
			if err != nil {
				return Static{}, err
			}
			return valueOf(value.OptionalOf(elemOf(t))), nil
		}},
		"items": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a queue", queue)
			if err != nil {
				return Static{}, err
			}
			return a.opt(value.ListOf(elemOf(t))), nil
		}},
		// Graph
		"degree": {1, 3, func(c *checker, call *Call, args []Static) (Static, error) {
			if s := args[0].Form; s != FormEntity && s != FormNode {
				if _, err := (argList{c, call, args}).need(0, "an entity", is(value.CodeRef, value.CodeID)); err != nil {
					return Static{}, err
				}
			}
			a := argList{c, call, args}
			for i := 1; i < len(args); i++ {
				if _, err := a.need(i, "text", textT); err != nil {
					return Static{}, err
				}
			}
			if len(args) == 3 {
				if l, ok := call.Args[2].(*Lit); ok {
					if s, ok := l.Val.(value.Text); ok && !slices.Contains([]string{"out", "in", "both"}, s.Go()) {
						return Static{}, typeErr(l.P, `degree's direction is "out", "in" or "both", not %s`, l.Val)
					}
				}
			}
			return stInt, nil
		}},
		"nodes": pathFn(value.ListOf(value.TypeRef)),
		"rels":  pathFn(value.ListOf(value.TypeID)),
		// Docs
		"parse": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			t, err := a.need(0, "a doc", is(value.CodeDoc))
			if err != nil {
				return Static{}, err
			}
			if r := value.Required(t); r.Code == value.CodeDoc && c.env.Catalog != nil {
				if def, ok := c.env.Catalog.FormatDef(r.Name); ok {
					return a.opt(def.Shape()), nil
				}
			}
			return stAny, nil
		}},
		"valid": {2, 2, func(c *checker, call *Call, args []Static) (Static, error) {
			a := argList{c, call, args}
			for i := range 2 {
				if _, err := a.need(i, "text", textT); err != nil {
					return Static{}, err
				}
			}
			if l, ok := call.Args[1].(*Lit); ok && c.env.Catalog != nil {
				if s, ok := l.Val.(value.Text); ok {
					if _, ok := c.env.Catalog.Format(s.Go()); !ok {
						return Static{}, errAt(l.P, "not_found", "no format %s", s.Go())
					}
				}
			}
			return stBool, nil
		}},
		"inner_text": sigNamed(value.TypeText, []string{"an XML element"}, is(value.CodeRecord)),
	}
}

func minMax(c *checker, call *Call, args []Static) (Static, error) {
	t := args[0]
	for i := 1; i < len(args); i++ {
		if err := comparable(call.Args[i].Position(), call.Name, t, args[i]); err != nil {
			return Static{}, err
		}
	}
	var u *value.Type
	for _, a := range args {
		u = unifyNext(u, a.asValue())
	}
	return valueOf(u), nil
}

func seqFn(result func(elem *value.Type) *value.Type) *fn {
	return &fn{1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		a := argList{c, call, args}
		t, err := a.need(0, "a list, array or set", func(t *value.Type) bool {
			return isSeq(t.Code) || t.Code == value.CodeSet
		})
		if err != nil {
			return Static{}, err
		}
		return valueOf(result(elemOf(t))), nil
	}}
}

func pathFn(result *value.Type) *fn {
	return &fn{1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		if args[0].Form != FormPath && !dynamic(value.Required(args[0].asValue())) {
			return Static{}, typeErr(call.Args[0].Position(), "%s needs a path (bound with follow path p = …), not %s", call.Name, args[0])
		}
		return valueOf(result), nil
	}}
}

// aggs are the aggregate functions. min and max are aggregates with one
// argument and ordinary functions with more.
var aggs = map[string]*fn{
	"count": {0, 1, func(*checker, *Call, []Static) (Static, error) { return stInt, nil }},
	"sum": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		t, err := argList{c, call, args}.need(0, "numbers or durations", func(t *value.Type) bool {
			return isNum(t) || t.Code == value.CodeDuration
		})
		if err != nil {
			return Static{}, err
		}
		r := value.Required(t)
		if r.Code == value.CodeDecimal {
			r = value.TypeDecimal
		}
		return valueOf(r), nil
	}},
	"avg": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		t, err := argList{c, call, args}.need(0, "numbers or durations", func(t *value.Type) bool {
			return isNum(t) || t.Code == value.CodeDuration
		})
		if err != nil {
			return Static{}, err
		}
		switch r := value.Required(t); {
		case dynamic(r):
			return stAny, nil
		case r.Code == value.CodeFloat, r.Code == value.CodeDuration:
			return valueOf(value.OptionalOf(r)), nil
		}
		return valueOf(value.OptionalOf(value.TypeDecimal)), nil // exact numbers average exactly
	}},
	"min": {1, 1, aggMinMax},
	"max": {1, 1, aggMinMax},
	"collect": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		return valueOf(value.ListOf(args[0].asValue())), nil
	}},
	"collect_set": {1, 1, func(c *checker, call *Call, args []Static) (Static, error) {
		t := args[0].asValue()
		if r := value.Required(t); !dynamic(r) && !r.Keyable() {
			return Static{}, typeErr(call.Args[0].Position(), "collect_set needs keyable values, and %s isn't; use collect", t)
		}
		return valueOf(value.SetOf(t)), nil
	}},
}

func aggMinMax(c *checker, call *Call, args []Static) (Static, error) {
	if args[0].Form == FormPath {
		return Static{}, typeErr(call.Args[0].Position(), "paths can't be compared")
	}
	return valueOf(value.OptionalOf(args[0].asValue())), nil
}

// isAggCall reports whether call is an aggregate.
func isAggCall(call *Call) bool {
	if _, ok := aggs[call.Name]; !ok {
		return false
	}
	return (call.Name != "min" && call.Name != "max") || len(call.Args) == 1
}

// call checks a function or aggregate call.
func (c *checker) call(call *Call, sc *scope) (Static, error) {
	agg := isAggCall(call)
	f := funcs[call.Name]
	if agg {
		f = aggs[call.Name]
		switch {
		case sc.inAgg:
			return Static{}, errAt(call.P, "syntax", "aggregates can't be nested")
		case !sc.grouped:
			return Static{}, errAt(call.P, "syntax", "%s() is an aggregate: use it after group by, or in show or let to aggregate all rows", call.Name)
		}
	}
	if f == nil {
		names := slices.Collect(maps.Keys(funcs))
		names = append(names, slices.Collect(maps.Keys(aggs))...)
		return Static{}, errAt(call.P, "not_found", "no function %s%s", call.Name, didYouMean(call.Name, names))
	}
	if len(call.Args) < f.min || f.max >= 0 && len(call.Args) > f.max {
		return Static{}, typeErr(call.P, "%s takes %s, not %d", call.Name, argCount(f), len(call.Args))
	}
	asc := sc
	if agg {
		asc = sc.pre.clone()
		asc.inAgg = true
	}
	args := make([]Static, len(call.Args))
	for i, a := range call.Args {
		var err error
		if args[i], err = c.expr(a, asc); err != nil {
			return Static{}, err
		}
	}
	return f.check(c, call, args)
}

func argCount(f *fn) string {
	plural := func(n int) string {
		if n == 1 {
			return "1 argument"
		}
		return itoa(n) + " arguments"
	}
	switch {
	case f.max < 0:
		return "at least " + plural(f.min)
	case f.min == f.max:
		return plural(f.min)
	}
	return itoa(f.min) + " to " + plural(f.max)
}

func itoa(n int) string { return strconv.Itoa(n) }
