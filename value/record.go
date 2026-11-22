package value

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
)

// Record is a value with a fixed set of named fields in declared order
// (spec/types.md §9.1). It lives inside one field and has no ID.
type Record struct {
	names []string
	vals  []Value
	d     int
}

// NewRecord returns a record with the given field names and values, in
// that order. Names must be valid identifiers and distinct.
func NewRecord(names []string, vals []Value) (Record, error) {
	if len(names) != len(vals) {
		return Record{}, errors.New("record needs the same number of names and values")
	}
	if len(names) == 0 {
		return Record{}, errors.New("record must have at least one field")
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if err := ValidateName("record field", n); err != nil {
			return Record{}, err
		}
		if seen[n] {
			return Record{}, fmt.Errorf("duplicate record field %q", n)
		}
		seen[n] = true
	}
	vals = slices.Clone(vals)
	d, err := checkElems("record", vals)
	if err != nil {
		return Record{}, err
	}
	return Record{slices.Clone(names), vals, d}, nil
}

func (Record) Code() Code   { return CodeRecord }
func (r Record) depth() int { return max(r.d, 1) }

// Len returns the number of fields.
func (r Record) Len() int { return len(r.names) }

// Field returns the name and value of the i'th field.
func (r Record) Field(i int) (string, Value) { return r.names[i], r.vals[i] }

// Get returns the value of the named field.
func (r Record) Get(name string) (Value, bool) {
	if i := slices.Index(r.names, name); i >= 0 {
		return r.vals[i], true
	}
	return nil, false
}

// All iterates over the fields in declared order.
func (r Record) All() iter.Seq2[string, Value] {
	return func(yield func(string, Value) bool) {
		for i, n := range r.names {
			if !yield(n, r.vals[i]) {
				return
			}
		}
	}
}

// Names returns the field names in declared order.
func (r Record) Names() []string { return slices.Clone(r.names) }

// String uses map literal syntax, which the declared type turns into a
// record.
func (r Record) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range r.names {
		if i > 0 {
			b.WriteString(", ")
		}
		if isBareKey(n) {
			b.WriteString(n)
		} else {
			b.WriteString(QuoteText(n))
		}
		b.WriteString(": ")
		writeOperand(&b, r.vals[i])
	}
	b.WriteByte('}')
	return b.String()
}

// isBareKey reports whether n can be written unquoted as a key in a brace
// literal: an identifier that isn't a reserved Rhizome keyword.
func isBareKey(n string) bool { return IsIdentifier(n) && !reservedWords[n] }

// reservedWords are Rhizome's reserved keywords (spec/rhizome.md §1.5),
// which can't be bare names.
var reservedWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		alter and as asc begin between bind by cast commit create database default
		define delete desc describe distinct doc drop exists explain false find follow
		for format from grant group has having history if in index inf is keep kind
		let limit match matches nan not null of on or password pop priority push read
		relate relation restrict revoke role rollback set show skip sort to true union
		unique unrelate unset update use user where with write
		rule infer derive when then trigger view extends trait`) {
		m[w] = true
	}
	return m
}()
