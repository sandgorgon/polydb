package value

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
)

// Composite values record their nesting depth (a scalar has depth 1) so the
// 32-level limit (spec/types.md §7) is enforced when they are built.

func depthOf(v Value) int {
	if d, ok := v.(interface{ depth() int }); ok {
		return d.depth()
	}
	return 1
}

// checkElems validates elements of a new composite value and returns its
// depth. nil elements are replaced by Null in place.
func checkElems(what string, elems []Value) (int, error) {
	if uint64(len(elems)) > MaxCollectionCount {
		return 0, fmt.Errorf("%s has more than %d elements", what, uint64(MaxCollectionCount))
	}
	d := 0
	for i, e := range elems {
		if e == nil {
			elems[i] = Null
		}
		d = max(d, depthOf(elems[i]))
	}
	if d+1 > MaxNesting {
		return 0, fmt.Errorf("%s nested more than %d levels deep", what, MaxNesting)
	}
	return d + 1, nil
}

// IsKeyable reports whether v can be a set element or map key (spec/types.md
// §7): any scalar except image and doc, or a pair, record or array made only
// of keyable values. Null is keyable, so set<int?> can hold null.
func IsKeyable(v Value) bool {
	switch x := v.(type) {
	case Pair:
		return IsKeyable(x.k) && IsKeyable(x.v)
	case Record:
		return allKeyable(x.vals)
	case Array:
		return allKeyable(x.elems)
	case Lazy:
		return false // keyed by content, which isn't loaded
	}
	switch code(v) {
	case CodeImage, CodeDoc, CodeList, CodeSet, CodeMap, CodeFifo, CodeLifo, CodeRing, CodePriority:
		return false
	}
	return true
}

func allKeyable(vs []Value) bool {
	for _, v := range vs {
		if !IsKeyable(v) {
			return false
		}
	}
	return true
}

// writeElems writes vs separated by ", ".
func writeElems(b *strings.Builder, vs []Value) {
	for i, v := range vs {
		if i > 0 {
			b.WriteString(", ")
		}
		writeOperand(b, v)
	}
}

// writeOperand writes v, adding parentheses around a pair so that it parses
// back as one element ("=>" binds loosely).
func writeOperand(b *strings.Builder, v Value) {
	if _, ok := v.(Pair); ok {
		b.WriteByte('(')
		b.WriteString(v.String())
		b.WriteByte(')')
		return
	}
	b.WriteString(v.String())
}

// --- list ---

// List is an ordered sequence of values that may contain duplicates.
type List struct {
	elems []Value
	d     int
}

// NewList returns a list of the given elements (copied). nil elements become
// null.
func NewList(elems ...Value) (List, error) {
	elems = slices.Clone(elems)
	d, err := checkElems("list", elems)
	if err != nil {
		return List{}, err
	}
	return List{elems, d}, nil
}

func (List) Code() Code       { return CodeList }
func (l List) depth() int     { return max(l.d, 1) }
func (l List) Len() int       { return len(l.elems) }
func (l List) At(i int) Value { return l.elems[i] }

// All iterates over the elements with their positions.
func (l List) All() iter.Seq2[int, Value] { return slices.All(l.elems) }

// Values returns a copy of the elements.
func (l List) Values() []Value { return slices.Clone(l.elems) }

func (l List) String() string {
	var b strings.Builder
	b.WriteByte('[')
	writeElems(&b, l.elems)
	b.WriteByte(']')
	return b.String()
}

// --- array ---

// Array is a fixed-length sequence. Its length is part of its type
// (array<T, N>); the value just holds exactly N elements.
type Array struct {
	elems []Value
	d     int
}

// NewArray returns an array of the given elements (copied). It must have at
// least one element.
func NewArray(elems ...Value) (Array, error) {
	if len(elems) == 0 {
		return Array{}, errors.New("array must have at least one element")
	}
	elems = slices.Clone(elems)
	d, err := checkElems("array", elems)
	if err != nil {
		return Array{}, err
	}
	return Array{elems, d}, nil
}

func (Array) Code() Code                   { return CodeArray }
func (a Array) depth() int                 { return max(a.d, 1) }
func (a Array) Len() int                   { return len(a.elems) }
func (a Array) At(i int) Value             { return a.elems[i] }
func (a Array) All() iter.Seq2[int, Value] { return slices.All(a.elems) }
func (a Array) Values() []Value            { return slices.Clone(a.elems) }

// String uses list syntax; the declared type turns it into an array.
func (a Array) String() string { return List{elems: a.elems}.String() }

// --- set ---

// Set is a collection of distinct keyable values, kept in sort order.
type Set struct {
	elems []Value // sorted, no duplicates
	d     int
}

// NewSet returns a set of the given elements. Duplicates are dropped. When
// values of different numeric types are equal (1 and 1.0), the one with the
// lowest type code is kept, so the result doesn't depend on input order.
func NewSet(elems ...Value) (Set, error) {
	elems = slices.Clone(elems)
	d, err := checkElems("set", elems)
	if err != nil {
		return Set{}, err
	}
	for _, e := range elems {
		if !IsKeyable(e) {
			return Set{}, fmt.Errorf("%s value can't be a set element", e.Code())
		}
	}
	slices.SortStableFunc(elems, compareThenCode)
	elems = slices.CompactFunc(elems, Equal)
	return Set{elems, d}, nil
}

// compareThenCode orders by value, then by type code among equal values.
func compareThenCode(a, b Value) int {
	if c := Compare(a, b); c != 0 {
		return c
	}
	return cmpInt(int(code(a)), int(code(b)))
}

func (Set) Code() Code   { return CodeSet }
func (s Set) depth() int { return max(s.d, 1) }
func (s Set) Len() int   { return len(s.elems) }

// Has reports whether the set contains a value equal to v.
func (s Set) Has(v Value) bool {
	_, ok := slices.BinarySearchFunc(s.elems, v, Compare)
	return ok
}

// All iterates over the elements in sort order.
func (s Set) All() iter.Seq[Value] { return slices.Values(s.elems) }

// Values returns the elements in sort order.
func (s Set) Values() []Value { return slices.Clone(s.elems) }

func (s Set) String() string {
	var b strings.Builder
	b.WriteByte('{')
	writeElems(&b, s.elems)
	b.WriteByte('}')
	return b.String()
}

// --- map ---

// Map is a collection of key-value entries with distinct keyable keys, kept
// in key sort order.
type Map struct {
	keys, vals []Value // sorted by key
	d          int
}

// NewMap returns a map with the given keys and values (keys[i] maps to
// vals[i]). Keys must be keyable and distinct.
func NewMap(keys, vals []Value) (Map, error) {
	if len(keys) != len(vals) {
		return Map{}, errors.New("map needs the same number of keys and values")
	}
	keys, vals = slices.Clone(keys), slices.Clone(vals)
	if _, err := checkElems("map", keys); err != nil {
		return Map{}, err
	}
	d, err := checkElems("map", vals)
	if err != nil {
		return Map{}, err
	}
	for _, k := range keys {
		if !IsKeyable(k) {
			return Map{}, fmt.Errorf("%s value can't be a map key", k.Code())
		}
		d = max(d, depthOf(k)+1)
	}
	if d > MaxNesting {
		return Map{}, fmt.Errorf("map nested more than %d levels deep", MaxNesting)
	}
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return Compare(keys[a], keys[b]) })
	m := Map{make([]Value, len(keys)), make([]Value, len(keys)), d}
	for i, j := range idx {
		if i > 0 && Equal(keys[idx[i-1]], keys[j]) {
			return Map{}, fmt.Errorf("duplicate map key %s", keys[j])
		}
		m.keys[i], m.vals[i] = keys[j], vals[j]
	}
	return m, nil
}

// MapOfText builds a map from Go strings to values; a convenience for
// map<text, T>.
func MapOfText(m map[string]Value) (Map, error) {
	keys := make([]Value, 0, len(m))
	vals := make([]Value, 0, len(m))
	for k, v := range m {
		t, err := NewText(k)
		if err != nil {
			return Map{}, err
		}
		keys = append(keys, t)
		vals = append(vals, v)
	}
	return NewMap(keys, vals)
}

func (Map) Code() Code   { return CodeMap }
func (m Map) depth() int { return max(m.d, 1) }
func (m Map) Len() int   { return len(m.keys) }

// Get returns the value for key k.
func (m Map) Get(k Value) (Value, bool) {
	i, ok := slices.BinarySearchFunc(m.keys, k, Compare)
	if !ok {
		return nil, false
	}
	return m.vals[i], true
}

// All iterates over the entries in key sort order.
func (m Map) All() iter.Seq2[Value, Value] {
	return func(yield func(Value, Value) bool) {
		for i, k := range m.keys {
			if !yield(k, m.vals[i]) {
				return
			}
		}
	}
}

// Keys returns the keys in sort order.
func (m Map) Keys() []Value { return slices.Clone(m.keys) }

func (m Map) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		writeOperand(&b, k) // a quoted or computed key is an expression
		b.WriteString(": ")
		writeOperand(&b, m.vals[i])
	}
	b.WriteByte('}')
	return b.String()
}

// --- pair ---

// Pair is one key and one value.
type Pair struct {
	k, v Value
	d    int
}

// NewPair returns the pair k => v. nil becomes null.
func NewPair(k, v Value) (Pair, error) {
	kv := []Value{k, v}
	d, err := checkElems("pair", kv)
	if err != nil {
		return Pair{}, err
	}
	return Pair{kv[0], kv[1], d}, nil
}

func (Pair) Code() Code     { return CodePair }
func (p Pair) depth() int   { return max(p.d, 1) }
func (p Pair) Key() Value   { return orNull(p.k) }
func (p Pair) Value() Value { return orNull(p.v) }

func orNull(v Value) Value {
	if v == nil {
		return Null
	}
	return v
}

func (p Pair) String() string {
	var b strings.Builder
	writeOperand(&b, p.Key())
	b.WriteString(" => ")
	writeOperand(&b, p.Value())
	return b.String()
}
