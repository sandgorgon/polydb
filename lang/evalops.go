package lang

import (
	"math"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/fulltext"
	"github.com/sandgorgon/polydb/value"
)

// Operators (spec/rhizome.md §8.1–8.3).

func not(e Node, x value.Value) (value.Value, error) {
	switch x := x.(type) {
	case value.Bool:
		return !x, nil
	}
	if value.IsNull(x) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "not needs true or false, not %s", typeName(x))
}

func neg(e Node, x value.Value) (value.Value, error) {
	switch x := x.(type) {
	case value.Int:
		if x == math.MinInt64 {
			return nil, overflow(e, "int")
		}
		return -x, nil
	case value.BigInt:
		return built(value.NewBigInt(new(big.Int).Neg(x.Go())))
	case value.Float:
		return value.NewFloat(-x.Go()), nil
	case value.Decimal:
		return x.Neg(), nil
	case value.Duration:
		if x == math.MinInt64 {
			return nil, overflow(e, "duration")
		}
		return -x, nil
	}
	if value.IsNull(x) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "- needs a number or duration, not %s", typeName(x))
}

func overflow(e Node, what string) *Error {
	return runErr(e, "limit", "%s overflow: use bigint or decimal for larger numbers", what)
}

func (ev *Evaluator) binary(e *Binary, f *Frame) (value.Value, error) {
	if e.Op == "and" || e.Op == "or" {
		return ev.logic(e, f)
	}
	x, err := ev.EvalLazy(e.X, f)
	if err != nil {
		return nil, err
	}
	y, err := ev.EvalLazy(e.Y, f)
	if err != nil {
		return nil, err
	}
	if isLazy(x, y) {
		switch e.Op {
		case "=", "!=":
			if eq, ok := lazyEqual(x, y); ok {
				return value.Bool(eq == (e.Op == "=")), nil
			}
			fallthrough
		case "<", "<=", ">", ">=":
			b, err := ev.compareLarge(e, e.Op, x, y)
			return value.Bool(b), err
		case "starts with", "ends with", "contains", "matches":
			if v, ok, err := ev.textOpLarge(e, x, y); ok || err != nil {
				return v, err
			}
		}
	}
	if x, err = ev.Load(e.X, x); err != nil {
		return nil, err
	}
	if y, err = ev.Load(e.Y, y); err != nil {
		return nil, err
	}
	switch e.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		b, err := compare(e, e.Op, x, y)
		return value.Bool(b), err
	case "in":
		return contains(e, y, x)
	case "not in":
		v, err := contains(e, y, x)
		if err != nil || value.IsNull(v) {
			return v, err
		}
		return !v.(value.Bool), nil
	case "has":
		return contains(e, x, y)
	case "contains":
		if xt, ok := x.(value.Text); ok {
			yt, ok := y.(value.Text)
			if !ok {
				return textNull(e, y)
			}
			return value.Bool(strings.Contains(xt.Go(), yt.Go())), nil
		}
		return contains(e, x, y)
	case "starts with", "ends with", "matches":
		if value.IsNull(x) || value.IsNull(y) {
			return value.Null, nil
		}
		xt, ok1 := x.(value.Text)
		yt, ok2 := y.(value.Text)
		if !ok1 || !ok2 {
			return nil, runErr(e, "type", "%s works on text, not %s and %s", e.Op, typeName(x), typeName(y))
		}
		switch e.Op {
		case "starts with":
			return value.Bool(strings.HasPrefix(xt.Go(), yt.Go())), nil
		case "ends with":
			return value.Bool(strings.HasSuffix(xt.Go(), yt.Go())), nil
		}
		re, err := ev.regexp(yt.Go())
		if err != nil {
			return nil, runErr(e.Y, "type", "invalid regular expression: %v", err)
		}
		return value.Bool(re.MatchString(xt.Go())), nil
	case "+", "-", "*", "/", "%":
		return arithValues(e, e.Op, x, y)
	case "++", "--":
		return joinValues(e, e.Op, x, y)
	case "=>":
		return built(value.NewPair(x, y))
	}
	return nil, runErr(e, "internal", "evaluator: unknown operator %s", e.Op)
}

func textNull(e Node, y value.Value) (value.Value, error) {
	if value.IsNull(y) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "contains on text needs text, not %s", typeName(y))
}

// searchQuery parses a search() query, cached for the statement.
func (ev *Evaluator) searchQuery(q string) (*fulltext.Query, error) {
	if pq, ok := ev.queries[q]; ok {
		return pq, nil
	}
	pq, err := fulltext.Parse(q)
	if err != nil {
		return nil, err
	}
	if ev.queries == nil {
		ev.queries = map[string]*fulltext.Query{}
	}
	ev.queries[q] = pq
	return pq, nil
}

func (ev *Evaluator) regexp(pattern string) (*regexp.Regexp, error) {
	if re, ok := ev.regexps[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if ev.regexps == nil {
		ev.regexps = map[string]*regexp.Regexp{}
	}
	ev.regexps[pattern] = re
	return re, nil
}

// logic evaluates and/or with three-valued logic: false and null is
// false, true or null is true, otherwise null wins.
func (ev *Evaluator) logic(e *Binary, f *Frame) (value.Value, error) {
	stop := value.Bool(e.Op == "or") // the value that decides the result
	x, err := ev.Eval(e.X, f)
	if err != nil {
		return nil, err
	}
	xb, err := boolOrNull(e.X, x, e.Op)
	if err != nil {
		return nil, err
	}
	if xb == stop {
		return stop, nil
	}
	y, err := ev.Eval(e.Y, f)
	if err != nil {
		return nil, err
	}
	yb, err := boolOrNull(e.Y, y, e.Op)
	if err != nil {
		return nil, err
	}
	if yb == stop {
		return stop, nil
	}
	if value.IsNull(xb) || value.IsNull(yb) {
		return value.Null, nil
	}
	return !stop, nil
}

func boolOrNull(e Node, v value.Value, op string) (value.Value, error) {
	if b, ok := v.(value.Bool); ok {
		return b, nil
	}
	if value.IsNull(v) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "%s needs true or false, not %s", op, typeName(v))
}

// sameKey makes IDs and refs comparable: a ref compares as its target ID.
func sameKey(x, y value.Value) (value.Value, value.Value) {
	if r, ok := x.(value.Ref); ok {
		if _, ok := y.(value.ID); ok {
			x = r.Target
		}
	}
	if r, ok := y.(value.Ref); ok {
		if _, ok := x.(value.ID); ok {
			y = r.Target
		}
	}
	return x, y
}

func equal(x, y value.Value) bool {
	x, y = sameKey(x, y)
	return value.Equal(x, y)
}

// compare evaluates a comparison. Ordering with a null side is false;
// ordering values of different type groups is a type error.
func compare(e Node, op string, x, y value.Value) (bool, error) {
	x, y = sameKey(x, y)
	switch op {
	case "=":
		return value.Equal(x, y), nil
	case "!=":
		return !value.Equal(x, y), nil
	}
	if value.IsNull(x) || value.IsNull(y) {
		return false, nil
	}
	if !value.SameGroup(x, y) {
		return false, runErr(e, "type", "can't compare %s with %s using %s", typeName(x), typeName(y), op)
	}
	c := value.Compare(x, y)
	switch op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	}
	return c >= 0, nil
}

func (ev *Evaluator) between(e *Between, f *Frame) (value.Value, error) {
	vs := make([]value.Value, 3)
	for i, x := range []Expr{e.X, e.Lo, e.Hi} {
		var err error
		if vs[i], err = ev.EvalLazy(x, f); err != nil {
			return nil, err
		}
	}
	cmp := compare
	if isLazy(vs...) {
		cmp = ev.compareLarge
	}
	lo, err := cmp(e, ">=", vs[0], vs[1])
	if err != nil {
		return nil, err
	}
	hi, err := cmp(e, "<=", vs[0], vs[2])
	if err != nil {
		return nil, err
	}
	return value.Bool((lo && hi) != e.Not), nil
}

// contains tests whether coll holds elem: an element of a list, array,
// set or queue, or a key of a map.
func contains(e Node, coll, elem value.Value) (value.Value, error) {
	switch c := coll.(type) {
	case value.Map:
		_, ok := c.Get(elem)
		if !ok {
			// A ref also finds its ID as a key, and an ID its ref.
			switch k := elem.(type) {
			case value.Ref:
				_, ok = c.Get(k.Target)
			case value.ID:
				_, ok = c.Get(value.Ref{Target: k})
			}
		}
		return value.Bool(ok), nil
	case value.List, value.Array, value.Set, value.Queue:
		for _, v := range seqValues(c) {
			if equal(v, elem) {
				return value.Bool(true), nil
			}
		}
		return value.Bool(false), nil
	}
	if value.IsNull(coll) {
		return value.Null, nil
	}
	return nil, runErr(e, "type", "membership needs a list, set or map, not %s", typeName(coll))
}

// ===== Arithmetic =====

// arithValues evaluates x op y for + - * / % (spec/types.md §10.3 and
// the time rules of spec/rhizome.md §8.2).
func arithValues(e Node, op string, x, y value.Value) (value.Value, error) {
	if value.IsNull(x) || value.IsNull(y) {
		return value.Null, nil
	}
	xc, yc := x.Code(), y.Code()
	if xc.IsNumeric() && yc.IsNumeric() {
		switch {
		case xc == value.CodeFloat && yc == value.CodeFloat:
			return floatOp(op, x.(value.Float).Go(), y.(value.Float).Go()), nil
		case xc == value.CodeFloat || yc == value.CodeFloat:
			return nil, runErr(e, "type", "%s and %s don't mix: cast one side, for example cast(x as float)", xc, yc)
		case xc == value.CodeInt && yc == value.CodeInt:
			return intOp(e, op, int64(x.(value.Int)), int64(y.(value.Int)))
		case xc == value.CodeDecimal || yc == value.CodeDecimal:
			return decimalOp(e, op, toDecimal(x), toDecimal(y))
		}
		return bigOp(e, op, toBig(x), toBig(y))
	}
	switch {
	case xc == value.CodeTime && yc == value.CodeDuration && (op == "+" || op == "-"):
		d := time.Duration(y.(value.Duration))
		if op == "-" {
			d = -d
		}
		return built(value.NewTime(x.(value.Time).Go().Add(d)))
	case xc == value.CodeDuration && yc == value.CodeTime && op == "+":
		return built(value.NewTime(y.(value.Time).Go().Add(time.Duration(x.(value.Duration)))))
	case xc == value.CodeTime && yc == value.CodeTime && op == "-":
		a, b := x.(value.Time).Go(), y.(value.Time).Go()
		d := a.Sub(b)
		if d == math.MaxInt64 || d == math.MinInt64 {
			return nil, overflow(e, "duration")
		}
		return value.Duration(d), nil
	case xc == value.CodeDuration && yc == value.CodeDuration && (op == "+" || op == "-"):
		v, err := intOp(e, op, int64(x.(value.Duration)), int64(y.(value.Duration)))
		if err != nil {
			return nil, overflow(e, "duration")
		}
		return value.Duration(v.(value.Int)), nil
	case xc == value.CodeDuration && yc == value.CodeInt && (op == "*" || op == "/"),
		xc == value.CodeInt && yc == value.CodeDuration && op == "*":
		a, b := int64(0), int64(0)
		if xc == value.CodeDuration {
			a, b = int64(x.(value.Duration)), int64(y.(value.Int))
		} else {
			a, b = int64(y.(value.Duration)), int64(x.(value.Int))
		}
		v, err := intOp(e, op, a, b)
		if err != nil {
			if ce, ok := err.(*Error); ok && ce.Code == "limit" {
				return nil, overflow(e, "duration")
			}
			return nil, err
		}
		return value.Duration(v.(value.Int)), nil
	}
	if op == "+" {
		switch xc {
		case value.CodeText, value.CodeBytes, value.CodeList, value.CodeSet, value.CodeMap:
			return nil, runErr(e, "type", "+ adds numbers; use ++ to join %s", xc)
		}
	}
	return nil, runErr(e, "type", "can't use %s with %s and %s", op, typeName(x), typeName(y))
}

func floatOp(op string, a, b float64) value.Value {
	switch op {
	case "+":
		return value.NewFloat(a + b)
	case "-":
		return value.NewFloat(a - b)
	case "*":
		return value.NewFloat(a * b)
	case "/":
		return value.NewFloat(a / b)
	}
	return value.NewFloat(math.Mod(a, b))
}

func divZero(e Node) *Error { return runErr(e, "type", "division by zero") }

// intOp does int arithmetic, failing on overflow. Division rounds toward
// zero, and % takes the sign of the dividend.
func intOp(e Node, op string, a, b int64) (value.Value, error) {
	var r int64
	switch op {
	case "+":
		r = a + b
		if (a > 0 && b > 0 && r < 0) || (a < 0 && b < 0 && r >= 0) {
			return nil, overflow(e, "int")
		}
	case "-":
		r = a - b
		if (a >= 0 && b < 0 && r < 0) || (a < 0 && b > 0 && r >= 0) {
			return nil, overflow(e, "int")
		}
	case "*":
		if a != 0 && b != 0 {
			r = a * b
			if r/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
				return nil, overflow(e, "int")
			}
		}
	case "/", "%":
		if b == 0 {
			return nil, divZero(e)
		}
		if a == math.MinInt64 && b == -1 {
			if op == "%" {
				return value.Int(0), nil
			}
			return nil, overflow(e, "int")
		}
		if op == "/" {
			r = a / b
		} else {
			r = a % b
		}
	}
	return value.Int(r), nil
}

func toBig(v value.Value) *big.Int {
	switch v := v.(type) {
	case value.Int:
		return big.NewInt(int64(v))
	case value.BigInt:
		return v.Go()
	}
	return nil
}

func toDecimal(v value.Value) value.Decimal {
	switch v := v.(type) {
	case value.Int:
		return value.DecimalFromInt(int64(v))
	case value.BigInt:
		d, _ := value.NewDecimal(v.Go(), 0)
		return d
	case value.Decimal:
		return v
	}
	return value.Decimal{}
}

func bigOp(e Node, op string, a, b *big.Int) (value.Value, error) {
	r := new(big.Int)
	switch op {
	case "+":
		r.Add(a, b)
	case "-":
		r.Sub(a, b)
	case "*":
		r.Mul(a, b)
	case "/", "%":
		if b.Sign() == 0 {
			return nil, divZero(e)
		}
		if op == "/" {
			r.Quo(a, b)
		} else {
			r.Rem(a, b)
		}
	}
	v, err := value.NewBigInt(r)
	if err != nil {
		return nil, overflow(e, "bigint")
	}
	return v, nil
}

func decimalOp(e Node, op string, a, b value.Decimal) (value.Value, error) {
	var r value.Decimal
	var err error
	switch op {
	case "+":
		r, err = a.Add(b)
	case "-":
		r, err = a.Sub(b)
	case "*":
		r, err = a.Mul(b)
	case "/":
		r, err = a.Quo(b)
	case "%":
		r, err = a.Rem(b)
	}
	switch {
	case err == value.ErrDivZero:
		return nil, divZero(e)
	case err != nil:
		return nil, overflow(e, "decimal")
	}
	return r, nil
}

// ===== Joining and removing =====

func joinValues(e Node, op string, x, y value.Value) (value.Value, error) {
	if value.IsNull(x) || value.IsNull(y) {
		return value.Null, nil
	}
	if op == "++" {
		switch a := x.(type) {
		case value.Text:
			if b, ok := y.(value.Text); ok {
				return built(value.NewText(a.Go() + b.Go()))
			}
		case value.Bytes:
			if b, ok := y.(value.Bytes); ok {
				return built(value.NewBytes(append(a.Go(), b.Go()...), a.MediaType()))
			}
		case value.List, value.Array:
			if isSeqValue(y) {
				return built(value.NewList(append(seqValues(a), seqValues(y)...)...))
			}
		case value.Set:
			if b, ok := y.(value.Set); ok {
				return built(value.NewSet(append(a.Values(), b.Values()...)...))
			}
		case value.Map:
			if b, ok := y.(value.Map); ok {
				return mergeMaps(e, a, b)
			}
		}
		if x.Code().IsNumeric() {
			return nil, runErr(e, "type", "++ joins text, bytes and collections; use + to add numbers")
		}
		return nil, runErr(e, "type", "can't join %s and %s with ++", typeName(x), typeName(y))
	}
	var remove []value.Value
	switch y.(type) {
	case value.List, value.Array, value.Set:
		remove = seqValues(y)
	default:
		return nil, runErr(e, "type", "-- needs a list or set of what to remove, not %s", typeName(y))
	}
	gone := func(v value.Value) bool {
		for _, r := range remove {
			if equal(v, r) {
				return true
			}
		}
		return false
	}
	switch a := x.(type) {
	case value.List, value.Array:
		var keep []value.Value
		for _, v := range seqValues(a) {
			if !gone(v) {
				keep = append(keep, v)
			}
		}
		return built(value.NewList(keep...))
	case value.Set:
		var keep []value.Value
		for _, v := range a.Values() {
			if !gone(v) {
				keep = append(keep, v)
			}
		}
		return built(value.NewSet(keep...))
	case value.Map:
		var ks, vs []value.Value
		for k, v := range a.All() {
			if !gone(k) {
				ks, vs = append(ks, k), append(vs, v)
			}
		}
		return built(value.NewMap(ks, vs))
	}
	return nil, runErr(e, "type", "-- removes from lists, sets and maps, not %s", typeName(x))
}

func isSeqValue(v value.Value) bool {
	switch v.(type) {
	case value.List, value.Array:
		return true
	}
	return false
}

// mergeMaps returns a with b's entries added; b wins on equal keys.
func mergeMaps(e Node, a, b value.Map) (value.Value, error) {
	var ks, vs []value.Value
	for k, v := range a.All() {
		if _, ok := b.Get(k); !ok {
			ks, vs = append(ks, k), append(vs, v)
		}
	}
	for k, v := range b.All() {
		ks, vs = append(ks, k), append(vs, v)
	}
	return built(value.NewMap(ks, vs))
}

// ===== Type tests =====

// lazyEqual decides x = y without loading when a large value is
// involved and the references tell: two values of the same type with
// the same content have the same hash, and values of different lengths
// differ. ok is false when the content must be compared.
func lazyEqual(x, y value.Value) (eq, ok bool) {
	lx, xok := x.(value.Lazy)
	ly, yok := y.(value.Lazy)
	if !xok && !yok {
		return false, false
	}
	if !xok {
		x, y, lx = y, x, ly
	}
	if x.Code() != y.Code() {
		return false, false // the usual rules decide, after loading
	}
	if ly, ok := y.(value.Lazy); ok {
		return lx.Ref.Hash == ly.Ref.Hash, true
	}
	if n := value.LargeLen(y); n >= 0 && int64(n) != lx.Len() {
		return false, true
	}
	return false, false
}

func (ev *Evaluator) is(e *Is, f *Frame) (value.Value, error) {
	x, err := ev.EvalLazy(e.X, f)
	if err != nil {
		return nil, err
	}
	var r bool
	switch {
	case e.Null:
		r = value.IsNull(x)
	case e.Kind != "":
		if id, ok := nodeID(x); ok {
			k, err := ev.Graph.Meta(id, "kind")
			if err != nil {
				return nil, err
			}
			t, ok := k.(value.Text)
			r = ok && t.Go() == e.Kind
		}
	default:
		if r, err = ev.isType(x, e.Type); err != nil {
			return nil, err
		}
	}
	return value.Bool(r != e.Not), nil
}

// isType reports whether v is a value of type t, without conversions:
// 1 is an int, not a decimal.
func (ev *Evaluator) isType(v value.Value, t *value.Type) (bool, error) {
	if value.IsNull(v) {
		return t.Nullable(), nil
	}
	switch t.Code {
	case value.CodeAny:
		return true, nil
	case value.CodeUnion:
		for _, a := range t.Alts {
			if ok, err := ev.isType(v, a); ok || err != nil {
				return ok, err
			}
		}
		return false, nil
	}
	if v.Code() != t.Code {
		return false, nil
	}
	all := func(vs []value.Value, et *value.Type) (bool, error) {
		for _, x := range vs {
			if ok, err := ev.isType(x, et); !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	}
	switch x := v.(type) {
	case value.Decimal:
		if t.Precision > 0 {
			d, err := x.Fit(t.Precision, t.Scale)
			return err == nil && d.Cmp(x) == 0, nil
		}
	case value.Ref:
		if t.Name != "" {
			k, err := ev.Graph.Meta(x.Target, "kind")
			if err != nil {
				return false, nil // a dangling ref has no kind
			}
			kt, ok := k.(value.Text)
			return ok && kt.Go() == t.Name, nil
		}
	case value.Doc:
		return x.Format() == t.Name, nil
	case value.List, value.Set:
		return all(seqValues(x), t.Elem)
	case value.Array:
		if x.Len() != t.N {
			return false, nil
		}
		return all(x.Values(), t.Elem)
	case value.Queue:
		if t.Code == value.CodePriority && x.LowFirst() != t.LowFirst || x.Limit() != t.N {
			return false, nil
		}
		return all(x.Items(), t.Elem)
	case value.Map:
		if ok, err := all(x.Keys(), t.Key); !ok || err != nil {
			return ok, err
		}
		var vs []value.Value
		for _, v := range x.All() {
			vs = append(vs, v)
		}
		return all(vs, t.Elem)
	case value.Pair:
		if ok, err := ev.isType(x.Key(), t.Key); !ok || err != nil {
			return ok, err
		}
		return ev.isType(x.Value(), t.Elem)
	case value.Record:
		if x.Len() != len(t.Fields) {
			return false, nil
		}
		for _, fd := range t.Fields {
			fv, ok := x.Get(fd.Name)
			if !ok {
				return false, nil
			}
			if ok, err := ev.isType(orNull(fv), fd.Type); !ok || err != nil {
				return ok, err
			}
		}
	}
	return true, nil
}
