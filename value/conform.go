package value

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// Conform checks that v is a valid value of type t and returns it converted
// as spec/types.md §10.1 allows:
//
//   - int to bigint or decimal, and bigint to decimal
//   - decimal rounded to decimal(P,S), failing if it doesn't fit
//   - a list to an array of the right length, or to a queue (in pop order;
//     a priority queue takes priority => element pairs)
//   - a map with text keys to a record; an empty map to an empty set
//   - any value to any, or to a union that accepts it
//   - null to any optional type
//
// int and float never convert to each other. Conversions from text literals
// (to doc, time, date, duration and id) depend on the value being a literal
// and are done by the language layer. A ref's target kind is checked by the
// catalog, not here.
func Conform(v Value, t *Type) (Value, error) {
	return conform(v, t)
}

func conform(v Value, t *Type) (Value, error) {
	if v == nil {
		v = Null
	}
	if IsNull(v) {
		if t.Nullable() {
			return Null, nil
		}
		return nil, fmt.Errorf("null is not allowed for %s", t)
	}
	if _, ok := v.(BlobRef); ok {
		return nil, errors.New("a blob reference is not a value")
	}
	switch t.Code {
	case CodeAny:
		return v, nil
	case CodeUnion:
		return conformUnion(v, t)
	case CodeBigInt:
		if i, ok := v.(Int); ok {
			return BigInt{big.NewInt(int64(i))}, nil
		}
	case CodeDecimal:
		var d Decimal
		switch x := v.(type) {
		case Int:
			d = DecimalFromInt(int64(x))
		case BigInt:
			d = Decimal{x.Go(), 0}
		case Decimal:
			d = x
		default:
			return nil, mismatch(v, t)
		}
		if t.Precision > 0 {
			return d.Fit(t.Precision, t.Scale)
		}
		return d, nil
	case CodeDoc:
		if d, ok := v.(Doc); ok && d.format != t.Name {
			return nil, fmt.Errorf("doc<%s> value is not a %s", d.format, t)
		}
	case CodeList:
		if l, ok := v.(List); ok {
			elems, changed, err := conformAll(l.elems, t.Elem)
			if err != nil || !changed {
				return l, err
			}
			return NewList(elems...)
		}
	case CodeArray:
		var elems []Value
		switch x := v.(type) {
		case Array:
			elems = x.elems
		case List:
			elems = x.elems
		default:
			return nil, mismatch(v, t)
		}
		if len(elems) != t.N {
			return nil, fmt.Errorf("%s needs exactly %d elements, not %d", t, t.N, len(elems))
		}
		elems, _, err := conformAll(elems, t.Elem)
		if err != nil {
			return nil, err
		}
		return NewArray(elems...)
	case CodeSet:
		switch x := v.(type) {
		case Set:
			elems, changed, err := conformAll(x.elems, t.Elem)
			if err != nil || !changed {
				return x, err
			}
			return NewSet(elems...)
		case Map:
			if x.Len() == 0 {
				return Set{}, nil
			}
		}
	case CodeMap:
		if m, ok := v.(Map); ok {
			keys, kc, err := conformAll(m.keys, t.Key)
			if err != nil {
				return nil, err
			}
			vals, vc, err := conformAll(m.vals, t.Elem)
			if err != nil || !kc && !vc {
				return m, err
			}
			return NewMap(keys, vals)
		}
	case CodePair:
		if p, ok := v.(Pair); ok {
			k, err := conform(p.Key(), t.Key)
			if err != nil {
				return nil, err
			}
			val, err := conform(p.Value(), t.Elem)
			if err != nil {
				return nil, err
			}
			return NewPair(k, val)
		}
	case CodeRecord:
		return conformRecord(v, t)
	case CodeFifo, CodeLifo, CodeRing, CodePriority:
		return conformQueue(v, t)
	}
	if v.Code() != t.Code {
		return nil, mismatch(v, t)
	}
	return v, nil
}

func mismatch(v Value, t *Type) error {
	return fmt.Errorf("%s value is not a %s", v.Code(), t)
}

// conformAll conforms each element of vs to t. changed reports whether any
// element was converted; if not, vs is returned unchanged.
func conformAll(vs []Value, t *Type) (out []Value, changed bool, err error) {
	for i, e := range vs {
		c, err := conform(e, t)
		if err != nil {
			return nil, false, err
		}
		if !changed && !identical(c, e) {
			out = slices.Clone(vs)
			changed = true
		}
		if changed {
			out[i] = c
		}
	}
	if !changed {
		return vs, false, nil
	}
	return out, true, nil
}

// identical reports whether a and b are the same value with the same types
// throughout, unlike Equal, which treats 1 and 1.0 as equal. Conform uses it
// to avoid rebuilding values that needed no conversion.
func identical(a, b Value) bool {
	if code(a) != code(b) {
		return false
	}
	switch x := a.(type) {
	case Decimal:
		y := b.(Decimal)
		return x.scale == y.scale && x.Cmp(y) == 0
	case List:
		return identicalAll(x.elems, b.(List).elems)
	case Array:
		return identicalAll(x.elems, b.(Array).elems)
	case Set:
		return identicalAll(x.elems, b.(Set).elems)
	case Map:
		y := b.(Map)
		return identicalAll(x.keys, y.keys) && identicalAll(x.vals, y.vals)
	case Pair:
		y := b.(Pair)
		return identical(x.Key(), y.Key()) && identical(x.Value(), y.Value())
	case Record:
		y := b.(Record)
		return slices.Equal(x.names, y.names) && identicalAll(x.vals, y.vals)
	case Queue:
		y := b.(Queue)
		return x.lowFirst == y.lowFirst && x.limit == y.limit &&
			slices.Equal(x.prios, y.prios) && identicalAll(x.items, y.items)
	}
	return Equal(a, b)
}

func identicalAll(a, b []Value) bool {
	return slices.EqualFunc(a, b, identical)
}

func conformUnion(v Value, t *Type) (Value, error) {
	// An alternative of the value's own type family wins, so union<int |
	// decimal> holding 1 stays an int.
	for _, alt := range t.Alts {
		if alt.Code == v.Code() {
			if c, err := conform(v, alt); err == nil {
				return c, nil
			}
		}
	}
	for _, alt := range t.Alts {
		if c, err := conform(v, alt); err == nil {
			return c, nil
		}
	}
	return nil, mismatch(v, t)
}

func conformRecord(v Value, t *Type) (Value, error) {
	get := func(string) (Value, bool) { return nil, false }
	var given []string
	switch x := v.(type) {
	case Record:
		get, given = x.Get, x.names
	case Map:
		for k := range x.All() {
			kt, ok := k.(Text)
			if !ok {
				return nil, fmt.Errorf("record field names must be text, not %s", k.Code())
			}
			given = append(given, kt.s)
		}
		get = func(n string) (Value, bool) { return x.Get(Text{n}) }
	default:
		return nil, mismatch(v, t)
	}
	for _, n := range given {
		if !slices.ContainsFunc(t.Fields, func(f RecordField) bool { return f.Name == n }) {
			return nil, fmt.Errorf("%s has no field %q", t, n)
		}
	}
	names := make([]string, len(t.Fields))
	vals := make([]Value, len(t.Fields))
	for i, f := range t.Fields {
		names[i] = f.Name
		fv, ok := get(f.Name)
		if !ok {
			if !f.Type.Nullable() {
				return nil, fmt.Errorf("record field %q is missing", f.Name)
			}
			fv = Null
		}
		c, err := conform(fv, f.Type)
		if err != nil {
			return nil, fmt.Errorf("record field %q: %w", f.Name, err)
		}
		vals[i] = c
	}
	return NewRecord(names, vals)
}

func conformQueue(v Value, t *Type) (Value, error) {
	var items []Value
	var prios []int64
	switch x := v.(type) {
	case Queue:
		if x.code != t.Code {
			return nil, mismatch(v, t)
		}
		items, prios = x.items, x.prios
		if x.code == CodePriority && prios == nil {
			prios = []int64{}
		}
	case List:
		if t.Code != CodePriority {
			items = x.elems
			break
		}
		prios = make([]int64, len(x.elems))
		for i, e := range x.elems {
			p, ok := e.(Pair)
			pr, isInt := p.Key().(Int)
			if !ok || !isInt {
				return nil, fmt.Errorf("%s is written as a list of priority => element pairs", t)
			}
			items = append(items, p.Value())
			prios[i] = int64(pr)
		}
	default:
		return nil, mismatch(v, t)
	}
	items, _, err := conformAll(items, t.Elem)
	if err != nil {
		return nil, err
	}
	return NewQueueOf(t, items, prios)
}
