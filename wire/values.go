package wire

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/sandgorgon/polydb/value"
)

// Result-only values (spec/protocol.md §6.5). They appear in result rows
// and in entity field values, use tags outside the stored value encoding,
// and are never stored.
const (
	tagBlobRef  = 0x22
	tagList     = 0x50
	tagEntity   = 0x80
	tagField    = 0x81
	tagRelation = 0x82
	tagPath     = 0x83
)

// Entity is a whole entity in a result.
type Entity struct {
	ID     value.ID
	Kind   string // "" for kindless
	Meta   Meta
	Fields []*Field
}

// Meta is an entity's metadata.
type Meta struct {
	Created, Updated value.Time
	Version          int64
	Labels           value.Set
	Owner            string
	Props            value.Map
}

// Field is one field of an entity in a result.
type Field struct {
	ID       value.ID
	Name     string
	Type     string // type expression
	Doc      string
	Declared bool
	Value    any // value.Value, *BlobRef, or nil when absent
}

// Relation is a whole relation in a result.
type Relation struct {
	ID       value.ID
	Type     string
	From, To value.ID
	Props    value.Map
}

// Path is a path in a result: nodes (entities, or field IDs) and the
// relations between them.
type Path struct {
	Nodes []any
	Rels  []*Relation
}

// BlobRef stands for a value too large to send inline (spec/protocol.md
// §7). The client fetches its bytes with BLOB_READ.
type BlobRef struct {
	Type      string // the value's type family: "text", "bytes" or "image"
	Hash      []byte // content hash
	Length    int64  // total length in bytes
	MediaType string
	Token     []byte // lease token
}

// AppendAny appends a result value: a value.Value, one of the result-only
// types, nil (encoded as null), or a []any list (rows and cells that
// contain result-only values).
func AppendAny(dst []byte, v any) []byte {
	if isNilRef(v) {
		return value.AppendValue(dst, value.Null)
	}
	switch x := v.(type) {
	case nil:
		return value.AppendValue(dst, value.Null)
	case value.Value:
		return value.AppendValue(dst, x)
	case []any:
		dst = binary.AppendUvarint(append(dst, tagList), uint64(len(x)))
		for _, e := range x {
			dst = AppendAny(dst, e)
		}
		return dst
	case *Entity:
		dst = append(append(dst, tagEntity), x.ID[:]...)
		var kind value.Value = value.Null
		if x.Kind != "" {
			kind = value.MustText(x.Kind)
		}
		dst = value.AppendValue(dst, kind)
		dst = value.AppendValue(dst, metaRecord(x.Meta))
		dst = binary.AppendUvarint(dst, uint64(len(x.Fields)))
		for _, f := range x.Fields {
			dst = AppendAny(dst, f)
		}
		return dst
	case *Field:
		dst = append(append(dst, tagField), x.ID[:]...)
		dst = appendStr(appendStr(appendStr(dst, x.Name), x.Type), x.Doc)
		dst = append(dst, boolByte(x.Declared))
		if x.Value == nil {
			return append(dst, 0)
		}
		return AppendAny(append(dst, 1), x.Value)
	case *Relation:
		dst = append(append(dst, tagRelation), x.ID[:]...)
		dst = appendStr(dst, x.Type)
		dst = append(append(dst, x.From[:]...), x.To[:]...)
		return value.AppendValue(dst, x.Props)
	case *Path:
		dst = binary.AppendUvarint(append(dst, tagPath), uint64(len(x.Nodes)))
		for _, n := range x.Nodes {
			dst = AppendAny(dst, n)
		}
		dst = binary.AppendUvarint(dst, uint64(len(x.Rels)))
		for _, r := range x.Rels {
			dst = AppendAny(dst, r)
		}
		return dst
	case *BlobRef:
		dst = appendStr(append(dst, tagBlobRef), x.Type)
		dst = appendStr(dst, string(x.Hash))
		dst = binary.AppendUvarint(dst, uint64(x.Length))
		dst = appendStr(dst, x.MediaType)
		return appendStr(dst, string(x.Token))
	}
	panic(fmt.Sprintf("wire: can't encode %T", v))
}

// isNilRef reports whether v is a nil pointer to a result-only type,
// which encodes as null.
func isNilRef(v any) bool {
	switch x := v.(type) {
	case *Entity:
		return x == nil
	case *Field:
		return x == nil
	case *Relation:
		return x == nil
	case *Path:
		return x == nil
	case *BlobRef:
		return x == nil
	}
	return false
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func appendStr(dst []byte, s string) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(s))), s...)
}

var metaNames = []string{"created", "updated", "version", "labels", "owner", "props"}

func metaRecord(m Meta) value.Record {
	r, err := value.NewRecord(metaNames, []value.Value{m.Created, m.Updated, value.Int(m.Version), m.Labels,
		value.MustText(m.Owner), m.Props})
	if err != nil {
		panic(err) // the names are fixed and the values well-formed
	}
	return r
}

// DecodeAny decodes one result value from b and returns it with the
// number of bytes read. Lists holding only plain values decode to
// value.List; lists holding result-only values decode to []any.
func DecodeAny(b []byte) (any, int, error) {
	d := &anyDecoder{b: b}
	v, err := d.any(0)
	return v, d.i, err
}

type anyDecoder struct {
	b []byte
	i int
}

var errShort = errors.New("wire: value truncated")

func (d *anyDecoder) byte() (byte, error) {
	if d.i >= len(d.b) {
		return 0, errShort
	}
	d.i++
	return d.b[d.i-1], nil
}

func (d *anyDecoder) uvarint() (uint64, error) {
	u, n := binary.Uvarint(d.b[d.i:])
	if n <= 0 {
		return 0, errors.New("wire: bad varint")
	}
	d.i += n
	return u, nil
}

// count reads an element count, bounded by the bytes left (every element
// takes at least one byte).
func (d *anyDecoder) count() (int, error) {
	n, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(len(d.b)-d.i) {
		return 0, errShort
	}
	return int(n), nil
}

func (d *anyDecoder) str() (string, error) {
	n, err := d.count()
	if err != nil {
		return "", err
	}
	s := string(d.b[d.i : d.i+n])
	d.i += n
	return s, nil
}

func (d *anyDecoder) id() (value.ID, error) {
	var id value.ID
	if len(d.b)-d.i < len(id) {
		return id, errShort
	}
	copy(id[:], d.b[d.i:])
	d.i += len(id)
	return id, nil
}

func (d *anyDecoder) plain() (value.Value, error) {
	v, n, err := value.DecodeValue(d.b[d.i:])
	if err != nil {
		return nil, err
	}
	d.i += n
	return v, nil
}

const maxDepth = value.MaxNesting + 4

func (d *anyDecoder) any(depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("wire: value nested too deeply")
	}
	if d.i >= len(d.b) {
		return nil, errShort
	}
	switch d.b[d.i] {
	case tagList:
		d.i++
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		elems := make([]any, n)
		plain := true
		for i := range elems {
			if elems[i], err = d.any(depth + 1); err != nil {
				return nil, err
			}
			_, isValue := elems[i].(value.Value)
			plain = plain && isValue
		}
		if !plain {
			return elems, nil
		}
		vs := make([]value.Value, n)
		for i, e := range elems {
			vs[i] = e.(value.Value)
		}
		return value.NewList(vs...)
	case tagEntity:
		d.i++
		return d.entity(depth)
	case tagField:
		d.i++
		return d.field(depth)
	case tagRelation:
		d.i++
		return d.relation()
	case tagPath:
		d.i++
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		p := &Path{Nodes: make([]any, n)}
		for i := range p.Nodes {
			if p.Nodes[i], err = d.any(depth + 1); err != nil {
				return nil, err
			}
		}
		m, err := d.count()
		if err != nil {
			return nil, err
		}
		for range m {
			r, err := d.any(depth + 1)
			if err != nil {
				return nil, err
			}
			rel, ok := r.(*Relation)
			if !ok {
				return nil, errors.New("wire: a path's relations must be relations")
			}
			p.Rels = append(p.Rels, rel)
		}
		return p, nil
	case tagBlobRef:
		d.i++
		r := &BlobRef{}
		var err error
		if r.Type, err = d.str(); err != nil {
			return nil, err
		}
		h, err := d.str()
		if err != nil {
			return nil, err
		}
		r.Hash = []byte(h)
		n, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		r.Length = int64(n)
		if r.MediaType, err = d.str(); err != nil {
			return nil, err
		}
		t, err := d.str()
		r.Token = []byte(t)
		return r, err
	}
	return d.plain()
}

func (d *anyDecoder) entity(depth int) (*Entity, error) {
	e := &Entity{}
	var err error
	if e.ID, err = d.id(); err != nil {
		return nil, err
	}
	kind, err := d.plain()
	if err != nil {
		return nil, err
	}
	switch k := kind.(type) {
	case value.Text:
		e.Kind = k.Go()
	default:
		if !value.IsNull(kind) {
			return nil, errors.New("wire: an entity's kind must be text or null")
		}
	}
	mv, err := d.plain()
	if err != nil {
		return nil, err
	}
	if e.Meta, err = metaOf(mv); err != nil {
		return nil, err
	}
	n, err := d.count()
	if err != nil {
		return nil, err
	}
	for range n {
		f, err := d.any(depth + 1)
		if err != nil {
			return nil, err
		}
		fld, ok := f.(*Field)
		if !ok {
			return nil, errors.New("wire: an entity's fields must be fields")
		}
		e.Fields = append(e.Fields, fld)
	}
	return e, nil
}

func metaOf(v value.Value) (Meta, error) {
	r, ok := v.(value.Record)
	bad := errors.New("wire: bad entity metadata")
	if !ok || r.Len() != len(metaNames) {
		return Meta{}, bad
	}
	var m Meta
	for i, n := range metaNames {
		name, fv := r.Field(i)
		if name != n {
			return Meta{}, bad
		}
		switch n {
		case "created", "updated":
			t, ok := fv.(value.Time)
			if !ok {
				return Meta{}, bad
			}
			if n == "created" {
				m.Created = t
			} else {
				m.Updated = t
			}
		case "version":
			iv, ok := fv.(value.Int)
			if !ok {
				return Meta{}, bad
			}
			m.Version = int64(iv)
		case "labels":
			if m.Labels, ok = fv.(value.Set); !ok {
				return Meta{}, bad
			}
		case "owner":
			t, ok := fv.(value.Text)
			if !ok {
				return Meta{}, bad
			}
			m.Owner = t.Go()
		case "props":
			if m.Props, ok = fv.(value.Map); !ok {
				return Meta{}, bad
			}
		}
	}
	return m, nil
}

func (d *anyDecoder) field(depth int) (*Field, error) {
	f := &Field{}
	var err error
	if f.ID, err = d.id(); err != nil {
		return nil, err
	}
	if f.Name, err = d.str(); err != nil {
		return nil, err
	}
	if f.Type, err = d.str(); err != nil {
		return nil, err
	}
	if f.Doc, err = d.str(); err != nil {
		return nil, err
	}
	declared, err := d.byte()
	if err != nil {
		return nil, err
	}
	present, err := d.byte()
	if err != nil {
		return nil, err
	}
	if declared > 1 || present > 1 {
		return nil, errors.New("wire: bad field flags")
	}
	f.Declared = declared == 1
	if present == 1 {
		if f.Value, err = d.any(depth + 1); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (d *anyDecoder) relation() (*Relation, error) {
	r := &Relation{}
	var err error
	if r.ID, err = d.id(); err != nil {
		return nil, err
	}
	if r.Type, err = d.str(); err != nil {
		return nil, err
	}
	if r.From, err = d.id(); err != nil {
		return nil, err
	}
	if r.To, err = d.id(); err != nil {
		return nil, err
	}
	p, err := d.plain()
	if err != nil {
		return nil, err
	}
	m, ok := p.(value.Map)
	if !ok {
		return nil, errors.New("wire: a relation's properties must be a map")
	}
	r.Props = m
	return r, nil
}
