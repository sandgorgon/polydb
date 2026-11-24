package lang

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sandgorgon/polydb/value"
)

// call evaluates a function or aggregate call (spec/rhizome.md §8.4).
func (ev *Evaluator) call(e *Call, f *Frame) (value.Value, error) {
	if isAggCall(e) {
		return ev.aggregate(e, f)
	}
	switch e.Name {
	case "coalesce":
		for _, a := range e.Args {
			v, err := ev.EvalLazy(a, f)
			if err != nil || !value.IsNull(v) {
				return v, err
			}
		}
		return value.Null, nil
	case "typeof":
		v, err := ev.EvalLazy(e.Args[0], f)
		if err != nil {
			return nil, err
		}
		return value.MustText(typeName(v)), nil
	case "len":
		// The length of a large value needs no loading: bytes have it,
		// and text counts its characters a piece at a time.
		v, err := ev.EvalLazy(e.Args[0], f)
		if err != nil {
			return nil, err
		}
		if l, ok := v.(value.Lazy); ok {
			switch l.Code() {
			case value.CodeBytes:
				return value.Int(l.Len()), nil
			case value.CodeText:
				n, err := l.RuneCount()
				if err != nil {
					return nil, runErr(e, "internal", "%v", err)
				}
				return value.Int(n), nil
			}
			if v, err = ev.Load(e.Args[0], v); err != nil {
				return nil, err
			}
		}
		return ev.callWith(e, []value.Value{v})
	case "now":
		return built(value.NewTime(ev.Now))
	case "today":
		y, m, d := ev.Now.UTC().Date()
		return built(value.NewDate(y, m, d))
	}
	args, err := ev.evalAll(e.Args, f)
	if err != nil {
		return nil, err
	}
	return ev.callWith(e, args)
}

// callWith calls a function with its evaluated arguments.
func (ev *Evaluator) callWith(e *Call, args []value.Value) (value.Value, error) {
	// Functions return null when an argument is null (§8.3).
	for _, a := range args {
		if value.IsNull(a) {
			return value.Null, nil
		}
	}
	v, err := ev.builtin(e, args)
	if err != nil {
		if _, ok := err.(*Error); !ok {
			err = runErr(e, "type", "%s: %v", e.Name, err)
		}
		return nil, err
	}
	return v, nil
}

// argErr reports an argument of the wrong type, found at run time.
func argErr(e *Call, i int, want string, got value.Value) *Error {
	return runErr(e.Args[i], "type", "%s needs %s as argument %d, not %s", e.Name, want, i+1, typeName(got))
}

func textArg(e *Call, args []value.Value, i int) (string, error) {
	if t, ok := args[i].(value.Text); ok {
		return t.Go(), nil
	}
	return "", argErr(e, i, "text", args[i])
}

func intArg(e *Call, args []value.Value, i int) (int64, error) {
	if n, ok := args[i].(value.Int); ok {
		return int64(n), nil
	}
	return 0, argErr(e, i, "an int", args[i])
}

func text(s string) (value.Value, error) { return value.NewText(s) }

func (ev *Evaluator) builtin(e *Call, args []value.Value) (value.Value, error) {
	a0 := args[0]
	switch e.Name {
	case "id":
		if id, ok := nodeID(a0); ok {
			return id, nil
		}
		return nil, argErr(e, 0, "an entity, relation, ref or id", a0)
	case "len":
		if ty, ok := ev.Info.Types[e.Args[0]]; ok && ty.Form == FormPath {
			return value.Int(len(seqValues(a0)) / 2), nil
		}
		switch x := a0.(type) {
		case value.Text:
			return value.Int(x.Len()), nil
		case value.Bytes:
			return value.Int(x.Len()), nil
		case value.Map:
			return value.Int(x.Len()), nil
		case value.List, value.Array, value.Set, value.Queue:
			return value.Int(len(seqValues(x))), nil
		}
		return nil, argErr(e, 0, "text, bytes, a collection or a queue", a0)
	case "lower", "upper", "trim":
		s, err := textArg(e, args, 0)
		if err != nil {
			return nil, err
		}
		switch e.Name {
		case "lower":
			return text(strings.ToLower(s))
		case "upper":
			return text(strings.ToUpper(s))
		}
		return text(strings.TrimSpace(s))
	case "substr":
		s, err := textArg(e, args, 0)
		if err != nil {
			return nil, err
		}
		rs := []rune(s)
		start, err := intArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		start = min(max(start, 0), int64(len(rs)))
		end := int64(len(rs))
		if len(args) == 3 {
			n, err := intArg(e, args, 2)
			if err != nil {
				return nil, err
			}
			end = min(start+max(n, 0), end)
		}
		return text(string(rs[start:end]))
	case "split":
		s, err := textArg(e, args, 0)
		if err != nil {
			return nil, err
		}
		sep, err := textArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		var out []value.Value
		for _, p := range strings.Split(s, sep) {
			out = append(out, value.MustText(p)) // pieces of valid text split at code points
		}
		return value.NewList(out...)
	case "join":
		sep, err := textArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		var parts []string
		for _, v := range seqValues(a0) {
			switch t := v.(type) {
			case value.Text:
				parts = append(parts, t.Go())
			default:
				if !value.IsNull(v) {
					return nil, runErr(e.Args[0], "type", "join needs text elements, not %s", typeName(v))
				}
			}
		}
		if !isSeqValue(a0) && a0.Code() != value.CodeSet {
			return nil, argErr(e, 0, "a list or set of text", a0)
		}
		return text(strings.Join(parts, sep))
	case "replace":
		var s [3]string
		for i := range s {
			var err error
			if s[i], err = textArg(e, args, i); err != nil {
				return nil, err
			}
		}
		return text(strings.ReplaceAll(s[0], s[1], s[2]))
	case "abs":
		switch x := a0.(type) {
		case value.Int:
			if x < 0 {
				return neg(e, x)
			}
			return x, nil
		case value.BigInt:
			return value.NewBigInt(new(big.Int).Abs(x.Go()))
		case value.Float:
			return value.NewFloat(math.Abs(x.Go())), nil
		case value.Decimal:
			return x.Abs(), nil
		}
		return nil, argErr(e, 0, "a number", a0)
	case "round":
		digits := int64(0)
		if len(args) == 2 {
			var err error
			if digits, err = intArg(e, args, 1); err != nil {
				return nil, err
			}
		}
		return roundNum(e, a0, digits)
	case "floor", "ceil":
		switch x := a0.(type) {
		case value.Int, value.BigInt:
			return x, nil
		case value.Float:
			if e.Name == "floor" {
				return value.NewFloat(math.Floor(x.Go())), nil
			}
			return value.NewFloat(math.Ceil(x.Go())), nil
		case value.Decimal:
			if e.Name == "floor" {
				return x.Floor(), nil
			}
			return x.Ceil(), nil
		}
		return nil, argErr(e, 0, "a number", a0)
	case "min", "max":
		best := a0
		for i, v := range args[1:] {
			if !value.SameGroup(best, v) {
				return nil, runErr(e.Args[i+1], "type", "%s can't compare %s with %s", e.Name, typeName(best), typeName(v))
			}
			c := value.Compare(v, best)
			if e.Name == "min" && c < 0 || e.Name == "max" && c > 0 {
				best = v
			}
		}
		return best, nil
	case "sqrt":
		if x, ok := a0.(value.Float); ok {
			return value.NewFloat(math.Sqrt(x.Go())), nil
		}
		return nil, argErr(e, 0, "a float", a0)
	case "pow":
		return pow(e, a0, args[1])
	case "year", "month", "day":
		var t time.Time
		switch x := a0.(type) {
		case value.Time:
			t = x.Go()
		case value.Date:
			t = x.Go()
		default:
			return nil, argErr(e, 0, "a time or date", a0)
		}
		switch e.Name {
		case "year":
			return value.Int(t.Year()), nil
		case "month":
			return value.Int(t.Month()), nil
		}
		return value.Int(t.Day()), nil
	case "add_months":
		n, err := intArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		switch x := a0.(type) {
		case value.Date:
			t := addMonths(x.Go(), n)
			return value.NewDate(t.Year(), t.Month(), t.Day())
		case value.Time:
			return value.NewTime(addMonths(x.Go(), n))
		}
		return nil, argErr(e, 0, "a time or date", a0)
	case "truncate":
		t, ok := a0.(value.Time)
		if !ok {
			return nil, argErr(e, 0, "a time", a0)
		}
		d, ok := args[1].(value.Duration)
		if !ok || d <= 0 {
			return nil, runErr(e.Args[1], "type", "truncate needs a positive duration")
		}
		return value.NewTime(t.Go().Truncate(time.Duration(d)))
	case "keys", "values":
		m, ok := a0.(value.Map)
		if !ok {
			return nil, argErr(e, 0, "a map", a0)
		}
		var out []value.Value
		for k, v := range m.All() {
			if e.Name == "keys" {
				out = append(out, k)
			} else {
				out = append(out, orNull(v))
			}
		}
		return value.NewList(out...)
	case "first", "last", "sort":
		switch a0.(type) {
		case value.List, value.Array, value.Set:
		default:
			return nil, argErr(e, 0, "a list, array or set", a0)
		}
		vs := seqValues(a0)
		switch {
		case e.Name == "sort":
			slices.SortStableFunc(vs, value.Compare)
			return value.NewList(vs...)
		case len(vs) == 0:
			return value.Null, nil
		case e.Name == "first":
			return orNull(vs[0]), nil
		}
		return orNull(vs[len(vs)-1]), nil
	case "slice":
		if !isSeqValue(a0) {
			return nil, argErr(e, 0, "a list or array", a0)
		}
		vs := seqValues(a0)
		lo, err := intArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		hi, err := intArg(e, args, 2)
		if err != nil {
			return nil, err
		}
		n := int64(len(vs))
		lo, hi = min(max(lo, 0), n), min(max(hi, 0), n)
		if hi < lo {
			hi = lo
		}
		return value.NewList(vs[lo:hi]...)
	case "contains":
		if t, ok := a0.(value.Text); ok {
			s, err := textArg(e, args, 1)
			if err != nil {
				return nil, err
			}
			return value.Bool(strings.Contains(t.Go(), s)), nil
		}
		return contains(e, a0, args[1])
	case "peek", "items":
		q, ok := a0.(value.Queue)
		if !ok {
			return nil, argErr(e, 0, "a queue", a0)
		}
		if e.Name == "peek" {
			return orNull(q.Peek()), nil
		}
		return value.NewList(q.Items()...)
	case "degree":
		id, ok := nodeID(a0)
		if !ok {
			return nil, argErr(e, 0, "an entity", a0)
		}
		typ, dir := "", "both"
		if len(args) > 1 {
			var err error
			if typ, err = textArg(e, args, 1); err != nil {
				return nil, err
			}
		}
		if len(args) > 2 {
			var err error
			if dir, err = textArg(e, args, 2); err != nil {
				return nil, err
			}
			if dir != "out" && dir != "in" && dir != "both" {
				return nil, runErr(e.Args[2], "type", `degree's direction is "out", "in" or "both", not %q`, dir)
			}
		}
		n, err := ev.Graph.Degree(id, typ, dir)
		return value.Int(n), err
	case "nodes", "rels":
		vs := seqValues(a0)
		var out []value.Value
		start := 0
		if e.Name == "rels" {
			start = 1
		}
		for i := start; i < len(vs); i += 2 {
			out = append(out, vs[i])
		}
		return value.NewList(out...)
	case "parse":
		d, ok := a0.(value.Doc)
		if !ok {
			return nil, argErr(e, 0, "a doc", a0)
		}
		v, _, err := ev.parsed(e.Args[0], d)
		return v, err
	case "valid":
		s, err := textArg(e, args, 0)
		if err != nil {
			return nil, err
		}
		name, err := textArg(e, args, 1)
		if err != nil {
			return nil, err
		}
		fm, ok := ev.Graph.Format(name)
		if !ok {
			return nil, runErr(e.Args[1], "not_found", "no format %s", name)
		}
		_, err = fm.Parse(s)
		return value.Bool(err == nil), nil
	case "inner_text":
		return text(innerText(a0))
	case "search":
		t, ok := a0.(value.Text)
		if !ok {
			return nil, argErr(e, 0, "text", a0)
		}
		qt, ok := args[1].(value.Text)
		if !ok {
			return nil, argErr(e, 1, "text", args[1])
		}
		q, err := ev.searchQuery(qt.Go())
		if err != nil {
			return nil, runErr(e.Args[1], "syntax", "%v", err)
		}
		return value.Bool(q.Match(t.Go())), nil
	}
	return nil, runErr(e, "not_found", "no function %s", e.Name)
}

// addMonths adds n calendar months, keeping the day of the month but
// clamping it to the last day of a shorter month: Jan 31 + 1 is Feb 28.
func addMonths(t time.Time, n int64) time.Time {
	y, m, d := t.Date()
	total := int64(y)*12 + int64(m-1) + n
	ny, nm := int(total/12), time.Month(total%12+1)
	if last := time.Date(ny, nm+1, 0, 0, 0, 0, 0, time.UTC).Day(); d > last {
		d = last
	}
	return time.Date(ny, nm, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func roundNum(e *Call, v value.Value, digits int64) (value.Value, error) {
	if digits > int64(value.MaxDecimalDigits) || digits < -int64(value.MaxDecimalDigits) {
		return v, nil
	}
	switch x := v.(type) {
	case value.Float:
		p := math.Pow(10, float64(digits))
		return value.NewFloat(math.Round(x.Go()*p) / p), nil
	case value.Decimal:
		if digits >= int64(x.Scale()) {
			return x, nil
		}
		return roundExact(x.Rat(), digits, max(digits, 0))
	case value.Int, value.BigInt:
		if digits >= 0 {
			return x, nil
		}
		d, err := roundExact(new(big.Rat).SetInt(toBig(x)), digits, 0)
		if err != nil {
			return nil, err
		}
		n := d.(value.Decimal).Coefficient()
		if _, isInt := x.(value.Int); isInt {
			if !n.IsInt64() {
				return nil, overflow(e, "int")
			}
			return value.Int(n.Int64()), nil
		}
		return value.NewBigInt(n)
	}
	return nil, argErr(e, 0, "a number", v)
}

// roundExact rounds r to digits after the point (before it, if negative),
// half away from zero, and returns a decimal with the given scale.
func roundExact(r *big.Rat, digits int64, scale int64) (value.Value, error) {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(abs64(digits)), nil)
	x := new(big.Rat).Set(r)
	if digits >= 0 {
		x.Mul(x, new(big.Rat).SetInt(p))
	} else {
		x.Quo(x, new(big.Rat).SetInt(p))
	}
	n := roundRat(x) // in units of 10^-digits
	if digits < 0 {
		n.Mul(n, p)
	}
	return value.NewDecimal(n, int(scale))
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func pow(e *Call, x, y value.Value) (value.Value, error) {
	if xf, ok := x.(value.Float); ok {
		yf, ok := y.(value.Float)
		if !ok {
			return nil, argErr(e, 1, "a float", y)
		}
		return value.NewFloat(math.Pow(xf.Go(), yf.Go())), nil
	}
	n, ok := y.(value.Int)
	if !ok {
		return nil, argErr(e, 1, "an int exponent", y)
	}
	if n < 0 {
		return nil, runErr(e.Args[1], "type", "pow with an exact number needs an exponent ≥ 0; use floats for negative powers")
	}
	// Results grow with the exponent, so large exponents only work for
	// bases whose powers stay small: 0, 1 and -1.
	small := func(base *big.Int, scale int) bool {
		return scale == 0 && base.CmpAbs(big.NewInt(1)) <= 0
	}
	switch b := x.(type) {
	case value.Int, value.BigInt:
		base := toBig(b)
		if n > 1<<16 && !small(base, 0) {
			return nil, overflow(e, "bigint")
		}
		r := new(big.Int).Exp(base, big.NewInt(int64(n)), nil)
		if _, isInt := b.(value.Int); isInt {
			if !r.IsInt64() {
				return nil, overflow(e, "int")
			}
			return value.Int(r.Int64()), nil
		}
		v, err := value.NewBigInt(r)
		if err != nil {
			return nil, overflow(e, "bigint")
		}
		return v, nil
	case value.Decimal:
		coef, scale := b.Coefficient(), b.Scale()
		digits := int64(len(new(big.Int).Abs(coef).String()))
		if !small(coef, scale) && (digits*int64(n) > 2*value.MaxDecimalDigits || int64(scale)*int64(n) > value.MaxDecimalDigits) {
			return nil, overflow(e, "decimal")
		}
		if small(coef, scale) {
			scale = 0
		}
		r, err := value.NewDecimal(new(big.Int).Exp(coef, big.NewInt(int64(n)), nil), scale*int(n))
		if err != nil {
			return nil, overflow(e, "decimal")
		}
		return r, nil
	}
	return nil, argErr(e, 0, "a number", x)
}

// ===== Aggregates =====

// aggregate evaluates an aggregate over the rows of the current group.
// Nulls are skipped; over no values, count gives 0 and the others null.
func (ev *Evaluator) aggregate(e *Call, f *Frame) (value.Value, error) {
	var vals []value.Value
	for _, r := range f.Group {
		if len(e.Args) == 0 {
			vals = append(vals, value.Bool(true))
			continue
		}
		eval := ev.Eval
		if e.Name == "count" {
			eval = ev.EvalLazy // counting needs no content
		}
		v, err := eval(e.Args[0], &Frame{Row: r, Target: f.Target, HasTarget: f.HasTarget})
		if err != nil {
			return nil, err
		}
		if !value.IsNull(v) {
			vals = append(vals, v)
		}
	}
	switch e.Name {
	case "count":
		return value.Int(len(vals)), nil
	case "collect":
		return built(value.NewList(vals...))
	case "collect_set":
		return built(value.NewSet(vals...))
	}
	if len(vals) == 0 {
		return value.Null, nil
	}
	switch e.Name {
	case "min", "max":
		best := vals[0]
		for _, v := range vals[1:] {
			if !value.SameGroup(best, v) {
				return nil, runErr(e, "type", "%s can't compare %s with %s", e.Name, typeName(best), typeName(v))
			}
			if c := value.Compare(v, best); e.Name == "min" && c < 0 || e.Name == "max" && c > 0 {
				best = v
			}
		}
		return best, nil
	case "sum", "avg":
		sum := vals[0]
		for _, v := range vals[1:] {
			var err error
			if sum, err = arithValues(e, "+", sum, v); err != nil {
				return nil, err
			}
		}
		if e.Name == "sum" {
			return sum, nil
		}
		n := int64(len(vals))
		switch s := sum.(type) {
		case value.Float:
			return value.NewFloat(s.Go() / float64(n)), nil
		case value.Duration:
			return s / value.Duration(n), nil
		case value.Int, value.BigInt, value.Decimal:
			return decimalOp(e, "/", toDecimal(s), value.DecimalFromInt(n))
		}
		return nil, runErr(e, "type", "avg needs numbers or durations, not %s", typeName(sum))
	}
	return nil, runErr(e, "not_found", "no aggregate %s", e.Name)
}

// ===== Casts =====

// cast converts v to t (spec/types.md §10.2).
func (ev *Evaluator) cast(v value.Value, t *value.Type) (value.Value, error) {
	if value.IsNull(v) {
		return value.Null, nil
	}
	if t.Code == value.CodeAny {
		return v, nil
	}
	if c, err := value.Conform(v, t); err == nil {
		return c, nil
	}
	r := value.Required(t)
	out, err := ev.castTo(v, r)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("can't cast %s %s to %s", typeName(v), v, t)
	}
	return value.Conform(out, t) // applies decimal(P,S) and element types
}

func (ev *Evaluator) castTo(v value.Value, t *value.Type) (value.Value, error) {
	bad := func(err error) error { return fmt.Errorf("can't cast %s to %s: %v", v, t, err) }
	switch t.Code {
	case value.CodeInt, value.CodeBigInt, value.CodeFloat, value.CodeDecimal:
		return castNum(v, t, bad)
	case value.CodeText:
		s, ok, err := textOf(v)
		if err != nil || !ok {
			return nil, err
		}
		return value.NewText(s)
	case value.CodeBool:
		if s, ok := v.(value.Text); ok {
			switch s.Go() {
			case "true":
				return value.Bool(true), nil
			case "false":
				return value.Bool(false), nil
			}
			return nil, bad(fmt.Errorf("want true or false"))
		}
	case value.CodeTime:
		switch x := v.(type) {
		case value.Text:
			t, err := value.ParseTime(x.Go())
			if err != nil {
				return nil, bad(err)
			}
			return t, nil
		case value.Date:
			return value.NewTime(x.Go())
		}
	case value.CodeDate:
		switch x := v.(type) {
		case value.Text:
			d, err := value.ParseDate(x.Go())
			if err != nil {
				return nil, bad(err)
			}
			return d, nil
		case value.Time:
			y, m, d := x.Go().Date()
			return value.NewDate(y, m, d)
		}
	case value.CodeDuration:
		if s, ok := v.(value.Text); ok {
			d, err := time.ParseDuration(s.Go())
			if err != nil {
				return nil, bad(err)
			}
			return value.Duration(d), nil
		}
	case value.CodeID, value.CodeRef:
		var id value.ID
		switch x := v.(type) {
		case value.Text:
			var err error
			if id, err = value.ParseID(x.Go()); err != nil {
				return nil, bad(err)
			}
		case value.ID:
			id = x
		case value.Ref:
			id = x.Target
		default:
			return nil, nil
		}
		if t.Code == value.CodeRef {
			return value.Ref{Target: id}, nil
		}
		return id, nil
	case value.CodeBytes:
		if s, ok := v.(value.Text); ok {
			return value.NewBytes([]byte(s.Go()), "text/plain; charset=utf-8")
		}
	case value.CodeDoc:
		if s, ok := v.(value.Text); ok {
			f, ok := ev.Graph.Format(t.Name)
			if !ok {
				return nil, fmt.Errorf("no format %s", t.Name)
			}
			d, err := value.NewDoc(f, s.Go())
			if err != nil {
				return nil, bad(err)
			}
			return d, nil
		}
	case value.CodeList, value.CodeSet, value.CodeArray:
		if d, ok := v.(value.Doc); ok {
			p, _, err := ev.parsed(&Lit{}, d)
			if err != nil {
				return nil, err
			}
			return ev.cast(p, t)
		}
		var vs []value.Value
		switch v.(type) {
		case value.List, value.Array, value.Set:
			vs = seqValues(v)
		default:
			return nil, nil
		}
		out := make([]value.Value, len(vs))
		for i, x := range vs {
			var err error
			if out[i], err = ev.cast(x, t.Elem); err != nil {
				return nil, err
			}
		}
		switch t.Code {
		case value.CodeSet:
			return value.NewSet(out...)
		case value.CodeArray:
			return value.NewArray(out...)
		}
		return value.NewList(out...)
	case value.CodeRecord, value.CodeMap:
		switch x := v.(type) {
		case value.Doc:
			p, _, err := ev.parsed(&Lit{}, x)
			if err != nil {
				return nil, err
			}
			return ev.cast(p, t)
		case value.Record:
			if t.Code == value.CodeMap {
				var ks, vs []value.Value
				for n, fv := range x.All() {
					ks, vs = append(ks, value.MustText(n)), append(vs, orNull(fv))
				}
				return value.NewMap(ks, vs)
			}
		}
	}
	return nil, nil
}

// textOf returns the text form of a value for cast(x as text).
func textOf(v value.Value) (string, bool, error) {
	switch x := v.(type) {
	case value.Text:
		return x.Go(), true, nil
	case value.Int:
		return strconv.FormatInt(int64(x), 10), true, nil
	case value.BigInt:
		return x.Go().String(), true, nil
	case value.Float:
		return strconv.FormatFloat(x.Go(), 'g', -1, 64), true, nil
	case value.Decimal:
		return x.Plain(), true, nil
	case value.Bool:
		return strconv.FormatBool(bool(x)), true, nil
	case value.Time:
		return x.Go().Format(time.RFC3339Nano), true, nil
	case value.Date:
		return x.Go().Format("2006-01-02"), true, nil
	case value.Duration:
		return x.Go().String(), true, nil
	case value.ID:
		return x.Base32(), true, nil
	case value.Ref:
		return x.Target.Base32(), true, nil
	case value.Doc:
		return x.Text(), true, nil
	case value.Bytes:
		b := x.Go()
		if !utf8.Valid(b) {
			return "", false, fmt.Errorf("can't cast bytes to text: not valid UTF-8")
		}
		return string(b), true, nil
	}
	return "", false, nil
}

// castNum converts between numbers, and from text.
func castNum(v value.Value, t *value.Type, bad func(error) error) (value.Value, error) {
	var r *big.Rat
	isFloat := false
	var f float64
	switch x := v.(type) {
	case value.Int:
		r = new(big.Rat).SetInt64(int64(x))
	case value.BigInt:
		r = new(big.Rat).SetInt(x.Go())
	case value.Decimal:
		r = x.Rat()
	case value.Float:
		isFloat, f = true, x.Go()
	case value.Text:
		s := x.Go()
		switch t.Code {
		case value.CodeInt:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, bad(err)
			}
			return value.Int(n), nil
		case value.CodeBigInt:
			b, err := value.ParseBigInt(s)
			if err != nil {
				return nil, bad(err)
			}
			return b, nil
		case value.CodeFloat:
			n, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, bad(err)
			}
			return value.NewFloat(n), nil
		}
		d, err := value.ParseDecimal(s)
		if err != nil {
			return nil, bad(err)
		}
		return d, nil
	default:
		return nil, nil
	}
	if t.Code == value.CodeFloat {
		if isFloat {
			return value.NewFloat(f), nil
		}
		fl, _ := r.Float64()
		return value.NewFloat(fl), nil
	}
	if isFloat {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, bad(fmt.Errorf("%v has no exact value", f))
		}
		if t.Code == value.CodeDecimal {
			d, err := value.ParseDecimal(strconv.FormatFloat(f, 'f', -1, 64))
			if err != nil {
				return nil, bad(err)
			}
			return d, nil
		}
		r = new(big.Rat)
		r.SetFloat64(math.Round(f))
	}
	if t.Code == value.CodeDecimal {
		d, err := ratDecimal(r)
		if err != nil {
			return nil, bad(err)
		}
		return d, nil
	}
	// To an integer: round half away from zero.
	n := roundRat(r)
	if t.Code == value.CodeInt {
		if !n.IsInt64() {
			return nil, bad(fmt.Errorf("out of range for int"))
		}
		return value.Int(n.Int64()), nil
	}
	b, err := value.NewBigInt(n)
	if err != nil {
		return nil, bad(err)
	}
	return b, nil
}

func roundRat(r *big.Rat) *big.Int {
	num, den := new(big.Int).Abs(r.Num()), r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Lsh(m, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if r.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// ratDecimal converts an exact rational with a terminating decimal
// expansion (all inputs here are ints or decimals) to a Decimal.
func ratDecimal(r *big.Rat) (value.Decimal, error) {
	num, den := value.DecimalFromInt(0), value.DecimalFromInt(1)
	var err error
	if num, err = value.NewDecimal(r.Num(), 0); err != nil {
		return value.Decimal{}, err
	}
	if den, err = value.NewDecimal(r.Denom(), 0); err != nil {
		return value.Decimal{}, err
	}
	return num.Quo(den)
}
