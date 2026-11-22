package value

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// AppendType appends a compact binary encoding of t to dst. It is used to
// store types in the catalog and in added-field records. Layout: code
// byte, flags byte (1 optional, 2 low-first), then by code:
//
//	decimal                    uvarint precision, uvarint scale
//	ref, doc                   str name
//	list, set                  elem
//	array, ring, fifo, lifo,   uvarint N, elem
//	priority
//	map, pair                  key, elem
//	record                     uvarint count, then (str name, str doc, type)
//	union                      uvarint count, then the alternatives
//
// Names are written as given and not validated on decoding, so the catalog
// can store kind and format IDs in place of names (see MapNames).
func AppendType(dst []byte, t *Type) []byte {
	var flags byte
	if t.Optional {
		flags |= 1
	}
	if t.LowFirst {
		flags |= 2
	}
	dst = append(dst, byte(t.Code), flags)
	switch t.Code {
	case CodeDecimal:
		dst = binary.AppendUvarint(dst, uint64(t.Precision))
		dst = binary.AppendUvarint(dst, uint64(t.Scale))
	case CodeRef, CodeDoc:
		dst = appendStr(dst, t.Name)
	case CodeList, CodeSet:
		dst = AppendType(dst, t.Elem)
	case CodeArray, CodeRing, CodeFifo, CodeLifo, CodePriority:
		dst = binary.AppendUvarint(dst, uint64(t.N))
		dst = AppendType(dst, t.Elem)
	case CodeMap, CodePair:
		dst = AppendType(AppendType(dst, t.Key), t.Elem)
	case CodeRecord:
		dst = binary.AppendUvarint(dst, uint64(len(t.Fields)))
		for _, f := range t.Fields {
			dst = AppendType(appendStr(appendStr(dst, f.Name), f.Doc), f.Type)
		}
	case CodeUnion:
		dst = binary.AppendUvarint(dst, uint64(len(t.Alts)))
		for _, a := range t.Alts {
			dst = AppendType(dst, a)
		}
	}
	return dst
}

// DecodeType decodes a type written by AppendType that fills all of b. It
// checks the structure (known codes, nesting depth, counts) but not names
// or the other rules of Validate.
func DecodeType(b []byte) (*Type, error) {
	d := decoder{b: b}
	t, err := d.typ(1)
	if err == nil && d.pos != len(b) {
		err = errors.New("extra bytes after type")
	}
	if err != nil {
		return nil, fmt.Errorf("decode type: %w", err)
	}
	return t, nil
}

func (d *decoder) small() (int, error) {
	u, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if u > math.MaxInt32 {
		return 0, errors.New("number out of range")
	}
	return int(u), nil
}

func (d *decoder) typ(depth int) (*Type, error) {
	if depth > MaxNesting {
		return nil, fmt.Errorf("type nested more than %d levels deep", MaxNesting)
	}
	c, err := d.byte()
	if err != nil {
		return nil, err
	}
	flags, err := d.byte()
	if err != nil {
		return nil, err
	}
	if Code(c) > CodeAny || Code(c) == CodeNull || flags > 3 {
		return nil, fmt.Errorf("bad type code %d or flags %d", c, flags)
	}
	t := &Type{Code: Code(c), Optional: flags&1 != 0, LowFirst: flags&2 != 0}
	switch t.Code {
	case CodeDecimal:
		if t.Precision, err = d.small(); err != nil {
			return nil, err
		}
		t.Scale, err = d.small()
	case CodeRef, CodeDoc:
		var s []byte
		s, err = d.str(MaxNameLen * 4)
		t.Name = string(s)
	case CodeList, CodeSet:
		t.Elem, err = d.typ(depth + 1)
	case CodeArray, CodeRing, CodeFifo, CodeLifo, CodePriority:
		if t.N, err = d.small(); err != nil {
			return nil, err
		}
		t.Elem, err = d.typ(depth + 1)
	case CodeMap, CodePair:
		if t.Key, err = d.typ(depth + 1); err != nil {
			return nil, err
		}
		t.Elem, err = d.typ(depth + 1)
	case CodeRecord:
		var n int
		if n, err = d.count(); err != nil {
			return nil, err
		}
		t.Fields = make([]RecordField, n)
		for i := range t.Fields {
			var name, doc []byte
			if name, err = d.str(MaxNameLen * 4); err != nil {
				return nil, err
			}
			if doc, err = d.str(64 << 10); err != nil {
				return nil, err
			}
			t.Fields[i].Name, t.Fields[i].Doc = string(name), string(doc)
			if t.Fields[i].Type, err = d.typ(depth + 1); err != nil {
				return nil, err
			}
		}
	case CodeUnion:
		var n int
		if n, err = d.count(); err != nil {
			return nil, err
		}
		t.Alts = make([]*Type, n)
		for i := range t.Alts {
			if t.Alts[i], err = d.typ(depth + 1); err != nil {
				return nil, err
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// MapNames returns a copy of t in which the name of every ref<Kind> and
// doc<Format> is replaced by fn(code, name). fn may return an error, which
// stops the walk.
func (t *Type) MapNames(fn func(c Code, name string) (string, error)) (*Type, error) {
	if t == nil {
		return nil, nil
	}
	c := *t
	var err error
	if (c.Code == CodeRef && c.Name != "") || c.Code == CodeDoc {
		if c.Name, err = fn(c.Code, c.Name); err != nil {
			return nil, err
		}
	}
	if c.Elem, err = t.Elem.MapNames(fn); err != nil {
		return nil, err
	}
	if c.Key, err = t.Key.MapNames(fn); err != nil {
		return nil, err
	}
	if t.Fields != nil {
		c.Fields = make([]RecordField, len(t.Fields))
		for i, f := range t.Fields {
			c.Fields[i] = f
			if c.Fields[i].Type, err = f.Type.MapNames(fn); err != nil {
				return nil, err
			}
		}
	}
	if t.Alts != nil {
		c.Alts = make([]*Type, len(t.Alts))
		for i, a := range t.Alts {
			if c.Alts[i], err = a.MapNames(fn); err != nil {
				return nil, err
			}
		}
	}
	return &c, nil
}

// Names calls fn for the name of every ref<Kind> and doc<Format> in t.
func (t *Type) Names(fn func(c Code, name string)) {
	t.MapNames(func(c Code, name string) (string, error) {
		fn(c, name)
		return name, nil
	})
}
