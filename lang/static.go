package lang

import (
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Form says what kind of thing an expression denotes.
type Form uint8

const (
	FormValue    Form = iota // a value of Static.Type
	FormEntity               // an entity; Static.Kind when the kind is known
	FormRelation             // a relation; Static.Rel when the type is known and defined
	FormNode                 // an entity, field or relation known only at run time (an #id)
	FormPath                 // a path bound with "follow path"
)

// Static is the type the checker computes for an expression.
type Static struct {
	Form Form
	Type *value.Type         // FormValue
	Kind *model.Kind         // FormEntity: nil if any kind
	Rel  *model.RelationType // FormRelation: nil if any type
}

func valueOf(t *value.Type) Static { return Static{Type: t} }

var (
	typeNull = &value.Type{Code: value.CodeNull}

	stAny      = valueOf(value.TypeAny)
	stNull     = valueOf(typeNull)
	stBool     = valueOf(value.TypeBool)
	stInt      = valueOf(value.TypeInt)
	stText     = valueOf(value.TypeText)
	stTime     = valueOf(value.TypeTime)
	stDate     = valueOf(value.TypeDate)
	stDuration = valueOf(value.TypeDuration)
	stID       = valueOf(value.TypeID)
	stFloat    = valueOf(value.TypeFloat)
	stEntity   = Static{Form: FormEntity}
)

func (s Static) String() string {
	switch s.Form {
	case FormEntity:
		if s.Kind != nil {
			return "a " + s.Kind.Name + " entity"
		}
		return "an entity"
	case FormRelation:
		if s.Rel != nil {
			return "a " + s.Rel.Name + " relation"
		}
		return "a relation"
	case FormNode:
		return "a node"
	case FormPath:
		return "a path"
	}
	return s.Type.String()
}

// asValue returns the value type s has where a value is needed: an entity
// becomes a ref to it, a relation or node its ID.
func (s Static) asValue() *value.Type {
	switch s.Form {
	case FormEntity:
		if s.Kind != nil {
			return value.RefTo(s.Kind.Name)
		}
		return value.TypeRef
	case FormRelation, FormNode:
		return value.TypeID
	case FormPath:
		return value.TypeAny
	}
	return s.Type
}

// dynamic reports whether t's values are only known at run time, so checks
// on it are deferred: any, unions and null.
func dynamic(t *value.Type) bool {
	return t.Code == value.CodeAny || t.Code == value.CodeUnion || t.Code == value.CodeNull
}

func isNum(t *value.Type) bool { return t.Code.IsNumeric() }

// exactRank orders the exact numeric types for mixed arithmetic
// (spec/types.md §10.3); float is -1.
func exactRank(c value.Code) int {
	switch c {
	case value.CodeInt:
		return 0
	case value.CodeBigInt:
		return 1
	case value.CodeDecimal:
		return 2
	}
	return -1
}

func isSeq(c value.Code) bool { return c == value.CodeList || c == value.CodeArray }

// cmpGroup is a type's comparison group (spec/types.md §11.2). Values in
// different groups are never equal, and ordering them is a type error.
// IDs and refs share a group so that p.manager = #01… works.
func cmpGroup(c value.Code) int {
	switch c {
	case value.CodeBool:
		return 1
	case value.CodeInt, value.CodeBigInt, value.CodeFloat, value.CodeDecimal:
		return 2
	case value.CodeText:
		return 3
	case value.CodeBytes:
		return 4
	case value.CodeDate:
		return 5
	case value.CodeTime:
		return 6
	case value.CodeDuration:
		return 7
	case value.CodeID, value.CodeRef:
		return 8
	case value.CodeDoc:
		return 9
	case value.CodeImage:
		return 10
	case value.CodePair, value.CodeArray, value.CodeList:
		return 11
	case value.CodeRecord:
		return 12
	case value.CodeSet:
		return 13
	case value.CodeMap:
		return 14
	}
	if c.IsQueue() {
		return 15
	}
	return 0
}

// unify returns a type covering both a and b, for list literals, if and
// coalesce: the same type, the wider exact number, T? with null, or any.
func unify(a, b *value.Type) *value.Type {
	switch {
	case a.Equal(b):
		return a
	case a.Code == value.CodeNull:
		return value.OptionalOf(b)
	case b.Code == value.CodeNull:
		return value.OptionalOf(a)
	case a.Code == value.CodeAny || b.Code == value.CodeAny:
		return value.TypeAny
	}
	opt := a.Optional || b.Optional
	ra, rb := value.Required(a), value.Required(b)
	var t *value.Type
	switch {
	case ra.Equal(rb):
		t = ra
	case exactRank(ra.Code) >= 0 && exactRank(rb.Code) >= 0:
		t = ra
		if exactRank(rb.Code) > exactRank(ra.Code) {
			t = rb
		}
		if t.Code == value.CodeDecimal {
			t = value.TypeDecimal // precision may differ
		}
	case ra.Code == value.CodeRef && rb.Code == value.CodeRef:
		t = value.TypeRef
	case ra.Code == rb.Code && (ra.Code == value.CodeList || ra.Code == value.CodeSet):
		t = &value.Type{Code: ra.Code, Elem: unify(ra.Elem, rb.Elem)}
	case ra.Code == rb.Code && ra.Code == value.CodeMap:
		t = value.MapOf(unify(ra.Key, rb.Key), unify(ra.Elem, rb.Elem))
	default:
		return value.TypeAny
	}
	if opt {
		return value.OptionalOf(t)
	}
	return t
}

// assignable reports whether a value of type src may be written where dst
// is expected, mirroring value.Conform (spec/types.md §10.1). Anything
// only known at run time passes; Conform checks it then.
func assignable(src, dst *value.Type) bool {
	if dst.Code == value.CodeAny || src.Code == value.CodeAny || src.Code == value.CodeUnion {
		return true
	}
	if src.Code == value.CodeNull {
		return dst.Nullable()
	}
	if src.Optional && !dst.Nullable() {
		// T? into T fails only if the value is null; that is a run-time
		// check, like a required field read from an optional one.
		src = value.Required(src)
	}
	if dst.Code == value.CodeUnion {
		for _, a := range dst.Alts {
			if assignable(src, a) {
				return true
			}
		}
		return false
	}
	switch dst.Code {
	case value.CodeBigInt:
		return src.Code == value.CodeInt || src.Code == value.CodeBigInt
	case value.CodeDecimal:
		return exactRank(src.Code) >= 0
	case value.CodeRef:
		return src.Code == value.CodeRef && (src.Name == "" || dst.Name == "" || src.Name == dst.Name)
	case value.CodeDoc:
		return src.Code == value.CodeDoc && src.Name == dst.Name
	case value.CodeList:
		return src.Code == value.CodeList && assignable(src.Elem, dst.Elem)
	case value.CodeArray:
		return isSeq(src.Code) && assignable(src.Elem, dst.Elem) && (src.Code != value.CodeArray || src.N == dst.N)
	case value.CodeSet:
		return src.Code == value.CodeSet && assignable(src.Elem, dst.Elem)
	case value.CodeMap:
		return src.Code == value.CodeMap && assignable(src.Key, dst.Key) && assignable(src.Elem, dst.Elem)
	case value.CodePair:
		return src.Code == value.CodePair && assignable(src.Key, dst.Key) && assignable(src.Elem, dst.Elem)
	case value.CodeRecord:
		switch src.Code {
		case value.CodeMap:
			return src.Key.Code == value.CodeText || dynamic(src.Key)
		case value.CodeRecord:
			return recordAssignable(src, dst)
		}
		return false
	case value.CodeFifo, value.CodeLifo, value.CodeRing:
		return (src.Code == dst.Code || src.Code == value.CodeList) && assignable(src.Elem, dst.Elem)
	case value.CodePriority:
		if src.Code == value.CodeList {
			return assignable(src.Elem, value.PairOf(value.TypeInt, dst.Elem))
		}
		return src.Code == dst.Code && assignable(src.Elem, dst.Elem)
	}
	return src.Code == dst.Code
}

func recordAssignable(src, dst *value.Type) bool {
	has := map[string]*value.Type{}
	for _, f := range src.Fields {
		has[f.Name] = f.Type
	}
	for _, f := range dst.Fields {
		t, ok := has[f.Name]
		if !ok {
			if !f.Type.Nullable() {
				return false
			}
			continue
		}
		if !assignable(t, f.Type) {
			return false
		}
		delete(has, f.Name)
	}
	return len(has) == 0
}

// typeOfLit returns the type of a literal value.
func typeOfLit(v value.Value) *value.Type {
	if value.IsNull(v) {
		return typeNull
	}
	return &value.Type{Code: v.Code()}
}

// recordField returns the field of record type t named name.
func recordField(t *value.Type, name string) (*value.Type, bool) {
	for _, f := range t.Fields {
		if f.Name == name {
			return f.Type, true
		}
	}
	return nil, false
}

func recordNames(t *value.Type) []string {
	out := make([]string, len(t.Fields))
	for i, f := range t.Fields {
		out[i] = f.Name
	}
	return out
}

func fieldNames(fs []model.FieldDef) []string {
	out := make([]string, len(fs))
	for i := range fs {
		out[i] = fs[i].Name
	}
	return out
}
