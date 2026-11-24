// Package value implements PolyDB's value types: the type system, values,
// equality and ordering, conversions and the binary encodings defined in
// spec/types.md.
//
// Values are immutable. Constructors validate and normalize their input
// (for example, text is checked for valid UTF-8 and converted to NFC), so a
// Value that exists is always well-formed.
package value

import (
	"strconv"
	"strings"
)

// Code identifies a type family.
type Code uint8

// Type families. The order of these constants is not significant; the
// cross-type sort order is defined by group (see group in compare.go).
const (
	CodeNull Code = iota
	CodeBool
	CodeInt
	CodeBigInt
	CodeFloat
	CodeDecimal
	CodeText
	CodeBytes
	CodeTime
	CodeDate
	CodeDuration
	CodeID
	CodeRef
	CodeImage
	CodeDoc
	CodeList
	CodeArray
	CodeSet
	CodeMap
	CodePair
	CodeFifo
	CodeLifo
	CodeRing
	CodePriority
	CodeRecord
	CodeUnion
	CodeAny
	// CodeBlobRef is a BlobRef, which only stored records hold.
	CodeBlobRef
)

var codeNames = [...]string{
	CodeNull:     "null",
	CodeBool:     "bool",
	CodeInt:      "int",
	CodeBigInt:   "bigint",
	CodeFloat:    "float",
	CodeDecimal:  "decimal",
	CodeText:     "text",
	CodeBytes:    "bytes",
	CodeTime:     "time",
	CodeDate:     "date",
	CodeDuration: "duration",
	CodeID:       "id",
	CodeRef:      "ref",
	CodeImage:    "image",
	CodeDoc:      "doc",
	CodeList:     "list",
	CodeArray:    "array",
	CodeSet:      "set",
	CodeMap:      "map",
	CodePair:     "pair",
	CodeFifo:     "queue.fifo",
	CodeLifo:     "queue.lifo",
	CodeRing:     "queue.ring",
	CodePriority: "queue.priority",
	CodeRecord:   "record",
	CodeUnion:    "union",
	CodeAny:      "any",
	CodeBlobRef:  "blobref",
}

func (c Code) String() string {
	if int(c) < len(codeNames) {
		return codeNames[c]
	}
	return "code(" + strconv.Itoa(int(c)) + ")"
}

// IsQueue reports whether c is one of the queue families.
func (c Code) IsQueue() bool {
	return c == CodeFifo || c == CodeLifo || c == CodeRing || c == CodePriority
}

// IsNumeric reports whether c is one of the numeric families.
func (c Code) IsNumeric() bool {
	return c == CodeInt || c == CodeBigInt || c == CodeFloat || c == CodeDecimal
}

// Type is a PolyDB type, as written in a type expression (spec/types.md §2).
// Types are immutable once built; build them with the constructors in this
// file or with ParseType.
type Type struct {
	Code Code
	// Optional means the type also accepts null (written T?).
	Optional bool
	// Elem is the element type of lists, arrays, sets and queues, and the
	// value type of maps and pairs.
	Elem *Type
	// Key is the key type of maps and pairs.
	Key *Type
	// N is the length of an array, the capacity of a ring queue, or the
	// maximum size of a fifo, lifo or priority queue (0 means unbounded).
	N int
	// Precision and Scale constrain a decimal; Precision 0 means
	// unconstrained.
	Precision, Scale int
	// Name is the kind name of a ref<Kind> or the format name of a doc<F>.
	Name string
	// LowFirst makes a priority queue pop the smallest priority first.
	LowFirst bool
	// Fields are the fields of a record, in declared order.
	Fields []RecordField
	// Alts are the alternatives of a union.
	Alts []*Type
}

// RecordField is one field of a record type.
type RecordField struct {
	Name string
	Type *Type
	Doc  string
}

// Predeclared scalar types.
var (
	TypeBool     = &Type{Code: CodeBool}
	TypeInt      = &Type{Code: CodeInt}
	TypeBigInt   = &Type{Code: CodeBigInt}
	TypeFloat    = &Type{Code: CodeFloat}
	TypeDecimal  = &Type{Code: CodeDecimal}
	TypeText     = &Type{Code: CodeText}
	TypeBytes    = &Type{Code: CodeBytes}
	TypeTime     = &Type{Code: CodeTime}
	TypeDate     = &Type{Code: CodeDate}
	TypeDuration = &Type{Code: CodeDuration}
	TypeID       = &Type{Code: CodeID}
	TypeRef      = &Type{Code: CodeRef}
	TypeImage    = &Type{Code: CodeImage}
	TypeAny      = &Type{Code: CodeAny}
)

// OptionalOf returns t made optional (T?). It returns t itself if t is
// already optional or is any.
func OptionalOf(t *Type) *Type {
	if t.Optional || t.Code == CodeAny {
		return t
	}
	c := *t
	c.Optional = true
	return &c
}

// Required returns t without the optional marker.
func Required(t *Type) *Type {
	if !t.Optional {
		return t
	}
	c := *t
	c.Optional = false
	return &c
}

// DecimalOf returns decimal(precision, scale).
func DecimalOf(precision, scale int) *Type {
	return &Type{Code: CodeDecimal, Precision: precision, Scale: scale}
}

// RefTo returns ref<kind>. An empty kind gives plain ref.
func RefTo(kind string) *Type { return &Type{Code: CodeRef, Name: kind} }

// DocOf returns doc<format>.
func DocOf(format string) *Type { return &Type{Code: CodeDoc, Name: format} }

// ListOf returns list<elem>.
func ListOf(elem *Type) *Type { return &Type{Code: CodeList, Elem: elem} }

// ArrayOf returns array<elem, n>.
func ArrayOf(elem *Type, n int) *Type { return &Type{Code: CodeArray, Elem: elem, N: n} }

// SetOf returns set<elem>.
func SetOf(elem *Type) *Type { return &Type{Code: CodeSet, Elem: elem} }

// MapOf returns map<key, val>.
func MapOf(key, val *Type) *Type { return &Type{Code: CodeMap, Key: key, Elem: val} }

// PairOf returns pair<key, val>.
func PairOf(key, val *Type) *Type { return &Type{Code: CodePair, Key: key, Elem: val} }

// QueueOf returns a fifo, lifo or priority queue of elem with maximum size
// max (0 for unbounded). For a ring queue use RingOf.
func QueueOf(code Code, elem *Type, max int) *Type {
	return &Type{Code: code, Elem: elem, N: max}
}

// RingOf returns queue.ring<elem, capacity>.
func RingOf(elem *Type, capacity int) *Type { return &Type{Code: CodeRing, Elem: elem, N: capacity} }

// RecordOf returns record{fields...}.
func RecordOf(fields ...RecordField) *Type { return &Type{Code: CodeRecord, Fields: fields} }

// UnionOf returns union<alts...>.
func UnionOf(alts ...*Type) *Type { return &Type{Code: CodeUnion, Alts: alts} }

// Nullable reports whether t accepts null.
func (t *Type) Nullable() bool { return t.Optional || t.Code == CodeAny || t.Code == CodeNull }

// Keyable reports whether values of t may be set elements or map keys
// (spec/types.md §7).
func (t *Type) Keyable() bool {
	switch t.Code {
	case CodeBool, CodeInt, CodeBigInt, CodeFloat, CodeDecimal, CodeText, CodeBytes,
		CodeTime, CodeDate, CodeDuration, CodeID, CodeRef:
		return true
	case CodePair:
		return t.Key.Keyable() && t.Elem.Keyable()
	case CodeArray:
		return t.Elem.Keyable()
	case CodeRecord:
		for _, f := range t.Fields {
			if !f.Type.Keyable() {
				return false
			}
		}
		return true
	case CodeUnion:
		for _, a := range t.Alts {
			if !a.Keyable() {
				return false
			}
		}
		return true
	}
	return false
}

// Equal reports whether t and u are the same type. Record field docs are
// not part of a type's identity.
func (t *Type) Equal(u *Type) bool {
	if t == u {
		return true
	}
	if t == nil || u == nil {
		return false
	}
	if t.Code != u.Code || t.Optional != u.Optional || t.N != u.N ||
		t.Precision != u.Precision || t.Scale != u.Scale || t.Name != u.Name ||
		t.LowFirst != u.LowFirst || len(t.Fields) != len(u.Fields) || len(t.Alts) != len(u.Alts) {
		return false
	}
	if (t.Elem == nil) != (u.Elem == nil) || (t.Elem != nil && !t.Elem.Equal(u.Elem)) {
		return false
	}
	if (t.Key == nil) != (u.Key == nil) || (t.Key != nil && !t.Key.Equal(u.Key)) {
		return false
	}
	for i := range t.Fields {
		if t.Fields[i].Name != u.Fields[i].Name || !t.Fields[i].Type.Equal(u.Fields[i].Type) {
			return false
		}
	}
	for i := range t.Alts {
		if !t.Alts[i].Equal(u.Alts[i]) {
			return false
		}
	}
	return true
}

// String returns the canonical type expression for t; ParseType accepts it.
func (t *Type) String() string {
	var b strings.Builder
	t.write(&b)
	return b.String()
}

func (t *Type) write(b *strings.Builder) {
	switch t.Code {
	case CodeDecimal:
		b.WriteString("decimal")
		if t.Precision > 0 {
			b.WriteString("(" + strconv.Itoa(t.Precision) + "," + strconv.Itoa(t.Scale) + ")")
		}
	case CodeRef:
		b.WriteString("ref")
		if t.Name != "" {
			b.WriteString("<" + quoteName(t.Name) + ">")
		}
	case CodeDoc:
		b.WriteString("doc<" + quoteName(t.Name) + ">")
	case CodeList, CodeSet:
		b.WriteString(t.Code.String() + "<")
		t.Elem.write(b)
		b.WriteString(">")
	case CodeArray, CodeRing:
		b.WriteString(t.Code.String() + "<")
		t.Elem.write(b)
		b.WriteString(", " + strconv.Itoa(t.N) + ">")
	case CodeMap, CodePair:
		b.WriteString(t.Code.String() + "<")
		t.Key.write(b)
		b.WriteString(", ")
		t.Elem.write(b)
		b.WriteString(">")
	case CodeFifo, CodeLifo, CodePriority:
		b.WriteString(t.Code.String() + "<")
		t.Elem.write(b)
		if t.Code == CodePriority && t.LowFirst {
			b.WriteString(", low")
		}
		if t.N > 0 {
			b.WriteString(", max " + strconv.Itoa(t.N))
		}
		b.WriteString(">")
	case CodeRecord:
		b.WriteString("record{")
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteName(f.Name) + ": ")
			f.Type.write(b)
		}
		b.WriteString("}")
	case CodeUnion:
		b.WriteString("union<")
		for i, a := range t.Alts {
			if i > 0 {
				b.WriteString(" | ")
			}
			a.write(b)
		}
		b.WriteString(">")
	default:
		b.WriteString(t.Code.String())
	}
	if t.Optional {
		b.WriteString("?")
	}
}

// quoteName writes a name in backticks when it isn't a plain identifier.
func quoteName(s string) string {
	if IsIdentifier(s) {
		return s
	}
	return "`" + s + "`"
}
