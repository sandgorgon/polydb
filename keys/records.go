package keys

import (
	"errors"
	"fmt"
	"time"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Records are stored as lists of values in the value encoding, by
// position, so they decode with the same strict checks as any value.

// TypeCodec stores types. The catalog implements it, replacing kind and
// format names with their IDs so renames don't touch stored data.
type TypeCodec interface {
	EncodeType(*value.Type) ([]byte, error)
	DecodeType([]byte) (*value.Type, error)
}

// fields decodes a record list and checks its length.
func fields(b []byte, what string, n int) ([]value.Value, error) {
	v, err := value.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("keys: %s record: %w", what, err)
	}
	l, ok := v.(value.List)
	if !ok || l.Len() != n {
		return nil, fmt.Errorf("keys: %s record is not a list of %d", what, n)
	}
	return l.Values(), nil
}

func list(vs ...value.Value) []byte { return value.Encode(must(value.NewList(vs...))) }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// reader takes typed values from a record list, remembering the first
// type mismatch.
type reader struct {
	what string
	vs   []value.Value
	i    int
	err  error
}

func (r *reader) next() value.Value {
	v := r.vs[r.i]
	r.i++
	return v
}

func get[T value.Value](r *reader) T {
	v := r.next()
	t, ok := v.(T)
	if !ok && r.err == nil {
		r.err = fmt.Errorf("keys: %s record: item %d is %s", r.what, r.i-1, v.Code())
	}
	return t
}

func (r *reader) optID() value.ID {
	v := r.next()
	if value.IsNull(v) {
		return value.ID{}
	}
	id, ok := v.(value.ID)
	if !ok && r.err == nil {
		r.err = fmt.Errorf("keys: %s record: item %d is %s", r.what, r.i-1, v.Code())
	}
	return id
}

func optID(id value.ID) value.Value {
	if id.IsZero() {
		return value.Null
	}
	return id
}

// EncodeEntity encodes an entity header.
func EncodeEntity(e *model.Entity) []byte {
	return list(optID(e.Kind), e.Meta.Created, e.Meta.Updated, value.Int(e.Meta.Version),
		e.Meta.Labels, value.MustText(e.Meta.Owner), e.Meta.Props)
}

// DecodeEntity decodes an entity header.
func DecodeEntity(id value.ID, b []byte) (*model.Entity, error) {
	vs, err := fields(b, "entity", 7)
	if err != nil {
		return nil, err
	}
	r := &reader{what: "entity", vs: vs}
	e := &model.Entity{ID: id, Kind: r.optID()}
	e.Meta.Created = get[value.Time](r)
	e.Meta.Updated = get[value.Time](r)
	e.Meta.Version = uint64(get[value.Int](r))
	e.Meta.Labels = get[value.Set](r)
	e.Meta.Owner = get[value.Text](r).Go()
	e.Meta.Props = get[value.Map](r)
	return e, r.err
}

// EncodeFieldDef encodes a field definition as a value, for field records
// and catalog definitions.
func EncodeFieldDef(fd *model.FieldDef, tc TypeCodec) (value.Value, error) {
	tb, err := tc.EncodeType(fd.Type)
	if err != nil {
		return nil, err
	}
	col := value.Int(-1)
	if fd.Column != nil {
		col = value.Int(*fd.Column)
	}
	var dv value.Value = value.Null
	if fd.DefaultValue != nil {
		dv = fd.DefaultValue
	}
	return value.NewList(value.MustText(fd.Name), must(value.NewBytes(tb, "")), value.MustText(fd.Doc),
		value.Bool(fd.Unique), value.MustText(fd.Default), value.MustText(fd.Check),
		value.Int(fd.OnDelete), value.Bool(fd.History.On), value.Duration(fd.History.Keep), col, dv)
}

// DecodeFieldDef decodes a field definition written by EncodeFieldDef.
func DecodeFieldDef(v value.Value, tc TypeCodec) (*model.FieldDef, error) {
	l, ok := v.(value.List)
	if !ok || l.Len() != 11 {
		return nil, errors.New("keys: field definition is not a list of 11")
	}
	r := &reader{what: "field definition", vs: l.Values()}
	fd := &model.FieldDef{Name: get[value.Text](r).Go()}
	tb := get[value.Bytes](r)
	fd.Doc = get[value.Text](r).Go()
	fd.Unique = bool(get[value.Bool](r))
	fd.Default = get[value.Text](r).Go()
	fd.Check = get[value.Text](r).Go()
	fd.OnDelete = model.RefAction(get[value.Int](r))
	fd.History.On = bool(get[value.Bool](r))
	fd.History.Keep = time.Duration(get[value.Duration](r))
	if col := get[value.Int](r); col >= 0 {
		c := int(col)
		fd.Column = &c
	}
	if dv := r.vs[10]; !value.IsNull(dv) {
		fd.DefaultValue = dv
	}
	if r.err != nil {
		return nil, r.err
	}
	t, err := tc.DecodeType(tb.Go())
	if err != nil {
		return nil, err
	}
	fd.Type = t
	return fd, nil
}

// EncodeField encodes a field record. The field's entity and name are in
// its key.
func EncodeField(f *model.Field, tc TypeCodec) ([]byte, error) {
	var def value.Value = value.Null
	if !f.Declared {
		if f.Def == nil {
			return nil, fmt.Errorf("keys: added field %q has no definition", f.Name)
		}
		var err error
		if def, err = EncodeFieldDef(f.Def, tc); err != nil {
			return nil, err
		}
	}
	var v value.Value = value.Null
	if f.Value != nil {
		v = f.Value
	}
	return list(f.ID, value.Bool(f.Declared), value.Bool(f.Value != nil), v, def), nil
}

// DecodeField decodes a field record.
func DecodeField(entity value.ID, name string, b []byte, tc TypeCodec) (*model.Field, error) {
	vs, err := fields(b, "field", 5)
	if err != nil {
		return nil, err
	}
	r := &reader{what: "field", vs: vs}
	f := &model.Field{Entity: entity, Name: name}
	f.ID = get[value.ID](r)
	f.Declared = bool(get[value.Bool](r))
	hasValue := bool(get[value.Bool](r))
	f.Value = r.next()
	if !hasValue {
		f.Value = nil
	}
	def := r.next()
	if r.err != nil {
		return nil, r.err
	}
	if f.Declared != value.IsNull(def) {
		return nil, fmt.Errorf("keys: field %q: definition doesn't match declared flag", name)
	}
	if !f.Declared {
		if f.Def, err = DecodeFieldDef(def, tc); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// NodeRef is what an ID names: an entity, a relation, or a field together
// with its entity and name.
type NodeRef struct {
	Kind   model.NodeKind
	Entity value.ID // fields only
	Name   string   // fields only
}

// EncodeNode encodes a node record.
func EncodeNode(n NodeRef) []byte {
	b := []byte{byte(n.Kind)}
	if n.Kind == model.NodeField {
		b = append(append(b, n.Entity[:]...), n.Name...)
	}
	return b
}

// DecodeNode decodes a node record.
func DecodeNode(b []byte) (NodeRef, error) {
	if len(b) == 0 {
		return NodeRef{}, errors.New("keys: empty node record")
	}
	n := NodeRef{Kind: model.NodeKind(b[0])}
	switch {
	case n.Kind == model.NodeField && len(b) > 1+idLen:
		n.Entity = value.ID(b[1 : 1+idLen])
		n.Name = string(b[1+idLen:])
	case (n.Kind == model.NodeEntity || n.Kind == model.NodeRelation) && len(b) == 1:
	default:
		return NodeRef{}, fmt.Errorf("keys: bad node record %x", b)
	}
	return n, nil
}

// EncodeRelation encodes a relation record.
func EncodeRelation(rel *model.Relation) []byte {
	return list(rel.Type, rel.From, rel.To, rel.Props)
}

// DecodeRelation decodes a relation record.
func DecodeRelation(id value.ID, b []byte) (*model.Relation, error) {
	vs, err := fields(b, "relation", 4)
	if err != nil {
		return nil, err
	}
	r := &reader{what: "relation", vs: vs}
	rel := &model.Relation{ID: id}
	rel.Type = get[value.ID](r)
	rel.From = get[value.ID](r)
	rel.To = get[value.ID](r)
	rel.Props = get[value.Map](r)
	return rel, r.err
}
