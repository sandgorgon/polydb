// Package model defines PolyDB's data model: entities with fields,
// relations between entities and fields, and the catalog definitions that
// describe them (kinds, relation types, derived formats and indexes).
//
// The types here are plain data. Package catalog stores and validates
// definitions as a whole; package keys stores entities, fields and
// relations; the engine applies the rules that depend on stored data.
package model

import (
	"time"

	"github.com/sandgorgon/polydb/value"
)

// --- data ---

// Entity is a node in the graph: an ID, an optional kind and metadata. Its
// fields are stored as separate Field records.
type Entity struct {
	ID   value.ID
	Kind value.ID // zero for a kindless entity
	Meta Meta
}

// Meta is an entity's metadata (spec/rhizome.md §2.1: p::created and so
// on).
type Meta struct {
	Created value.Time
	Updated value.Time
	Version uint64 // increases by one on every change to the entity or its fields
	Labels  value.Set
	Owner   string    // the user that created the entity
	Props   value.Map // custom metadata, map<text, any>
}

// Field is one field of one entity. A field is a graph node too: it has
// its own ID and relations can start or end on it.
type Field struct {
	ID     value.ID
	Entity value.ID
	Name   string
	Value  value.Value
	// Declared is true for fields the entity's kind declares. Their type,
	// doc and options come from the kind.
	Declared bool
	// Def describes an added field (Declared false): its type, doc and
	// options. It is nil for declared fields.
	Def *FieldDef
}

// Relation is a directed edge between two nodes (entities or fields).
type Relation struct {
	ID    value.ID
	Type  value.ID // relation type
	From  value.ID
	To    value.ID
	Props value.Map // map<text, any>; empty if none
}

// NodeKind says what an ID names.
type NodeKind uint8

const (
	NodeEntity NodeKind = iota + 1
	NodeField
	NodeRelation
)

func (k NodeKind) String() string {
	switch k {
	case NodeEntity:
		return "entity"
	case NodeField:
		return "field"
	case NodeRelation:
		return "relation"
	}
	return "unknown"
}

// --- definitions ---

// FieldDef defines a field: of a kind, a relation type's properties, a
// derived format's shape, or a field added to one entity.
type FieldDef struct {
	Name string
	Type *value.Type
	Doc  string
	// Unique and Default are only allowed on kind fields.
	Unique  bool
	Default string // a Rhizome expression, "" if none
	Check   string // a Rhizome boolean expression, "" if none
	// OnDelete applies to ref fields: what happens when the target is
	// deleted (spec/types.md §4.12).
	OnDelete RefAction
	History  History
}

// RefAction is what happens to a ref when its target is deleted.
type RefAction uint8

const (
	RefDangle   RefAction = iota // the ref is left as is (default)
	RefRestrict                  // the delete fails
)

// History configures keeping old versions of a field.
type History struct {
	On   bool
	Keep time.Duration // 0 keeps everything
}

// Kind is an entity schema.
type Kind struct {
	ID      value.ID
	Name    string
	Doc     string
	Closed  bool    // entities may not add their own fields
	History History // history for every field of the kind
	Fields  []FieldDef
}

// Field returns the declared field with the given name.
func (k *Kind) Field(name string) (*FieldDef, bool) {
	for i := range k.Fields {
		if k.Fields[i].Name == name {
			return &k.Fields[i], true
		}
	}
	return nil, false
}

// RelationType describes relations with one type name. Undefined types
// are registered automatically the first time they are used, with Defined
// false: any endpoints, no limits, cascade on delete.
type RelationType struct {
	ID       value.ID
	Name     string
	Defined  bool
	Doc      string
	From, To Endpoint
	MaxOut   int // 0 means no limit
	MaxIn    int
	OnDelete RelAction
	Props    []FieldDef
}

// Prop returns the property definition with the given name.
func (r *RelationType) Prop(name string) (*FieldDef, bool) {
	for i := range r.Props {
		if r.Props[i].Name == name {
			return &r.Props[i], true
		}
	}
	return nil, false
}

// RelAction is what happens to relations when an entity at either end is
// deleted.
type RelAction uint8

const (
	RelCascade  RelAction = iota // the relations are deleted too (default)
	RelRestrict                  // the delete fails
)

// Endpoint restricts what a relation may start or end on.
type Endpoint struct {
	Mode EndpointMode
	Kind value.ID // EndpointKind only
}

// EndpointMode is the kind of endpoint restriction.
type EndpointMode uint8

const (
	EndpointAny   EndpointMode = iota // any entity or field
	EndpointKind                      // an entity of one kind
	EndpointField                     // any field
)

// Format is a derived document format: a base format plus a shape
// (spec/types.md §6.1).
type Format struct {
	ID     value.ID
	Name   string
	Base   string // a base format: json, xml, csv, tsv or one registered in Go
	Doc    string
	Fields []FieldDef
}

// Shape returns the record type documents of the format must match.
func (f *Format) Shape() *value.Type {
	fields := make([]value.RecordField, len(f.Fields))
	for i, fd := range f.Fields {
		fields[i] = value.RecordField{Name: fd.Name, Type: fd.Type, Doc: fd.Doc}
	}
	return value.RecordOf(fields...)
}

// Index is a secondary index on one kind.
type Index struct {
	ID     value.ID
	Name   string
	Kind   value.ID
	Unique bool
	Text   bool // full-text index
	Keys   []IndexKey
}

// IndexKey is one indexed field, optionally a path inside a doc field.
type IndexKey struct {
	Field string
	Path  []PathStep
}

// PathStep is one constant "->" step inside a document (spec/rhizome.md
// §8.6).
type PathStep struct {
	Name  string // a key or element name (when not Attr and not IsPos)
	Attr  bool   // an XML attribute: ->@Name
	IsPos bool   // a position: ->[Pos]
	Pos   int
}
