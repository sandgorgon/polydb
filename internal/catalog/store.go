package catalog

import (
	"fmt"
	"time"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/value"
)

// Definitions are stored as value-encoded lists whose first item is the
// name, so Load can learn every name before decoding types that refer to
// them.

func text(s string) value.Value { return value.MustText(s) }

func listOf(vs ...value.Value) value.List { return must(value.NewList(vs...)) }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func encodeFields(fields []model.FieldDef, tc keys.TypeCodec) (value.Value, error) {
	vs := make([]value.Value, len(fields))
	for i := range fields {
		v, err := keys.EncodeFieldDef(&fields[i], tc)
		if err != nil {
			return nil, err
		}
		vs[i] = v
	}
	return value.NewList(vs...)
}

func decodeFields(v value.Value, tc keys.TypeCodec) ([]model.FieldDef, error) {
	l, ok := v.(value.List)
	if !ok {
		return nil, fmt.Errorf("field list is %s", v.Code())
	}
	out := make([]model.FieldDef, l.Len())
	for i := range out {
		fd, err := keys.DecodeFieldDef(l.At(i), tc)
		if err != nil {
			return nil, err
		}
		out[i] = *fd
	}
	return out, nil
}

func encodeKind(k *model.Kind, tc keys.TypeCodec) ([]byte, error) {
	fields, err := encodeFields(k.Fields, tc)
	if err != nil {
		return nil, err
	}
	return value.Encode(listOf(text(k.Name), text(k.Doc), value.Bool(k.Closed),
		value.Bool(k.History.On), value.Duration(k.History.Keep), fields)), nil
}

func encodeRelation(r *model.RelationType, tc keys.TypeCodec) ([]byte, error) {
	props, err := encodeFields(r.Props, tc)
	if err != nil {
		return nil, err
	}
	return value.Encode(listOf(text(r.Name), value.Bool(r.Defined), text(r.Doc),
		value.Int(r.From.Mode), r.From.Kind, value.Int(r.To.Mode), r.To.Kind,
		value.Int(r.MaxOut), value.Int(r.MaxIn), value.Int(r.OnDelete), props)), nil
}

func encodeFormat(f *model.Format, tc keys.TypeCodec) ([]byte, error) {
	fields, err := encodeFields(f.Fields, tc)
	if err != nil {
		return nil, err
	}
	return value.Encode(listOf(text(f.Name), text(f.Base), text(f.Doc), value.Bool(f.Header), fields)), nil
}

func encodeIndex(ix *model.Index) []byte {
	ks := make([]value.Value, len(ix.Keys))
	for i, k := range ix.Keys {
		steps := make([]value.Value, len(k.Path))
		for j, s := range k.Path {
			steps[j] = listOf(text(s.Name), value.Bool(s.Attr), value.Bool(s.IsPos), value.Int(s.Pos))
		}
		ks[i] = listOf(text(k.Field), listOf(steps...))
	}
	return value.Encode(listOf(text(ix.Name), ix.Kind, value.Bool(ix.Unique), value.Bool(ix.Text), listOf(ks...)))
}

// rec reads items of a stored definition, remembering the first mismatch.
type rec struct {
	vs  []value.Value
	i   int
	err error
}

func item[T value.Value](r *rec) T {
	var zero T
	if r.i >= len(r.vs) {
		r.err = fmt.Errorf("definition too short")
		return zero
	}
	v := r.vs[r.i]
	r.i++
	t, ok := v.(T)
	if !ok && r.err == nil {
		r.err = fmt.Errorf("definition item %d is %s", r.i-1, v.Code())
	}
	return t
}

func (r *rec) text() string { return item[value.Text](r).Go() }
func (r *rec) bool() bool   { return bool(item[value.Bool](r)) }
func (r *rec) int() int     { return int(item[value.Int](r)) }

type rawDef struct {
	typ  byte
	id   value.ID
	name string
	vs   []value.Value
}

// Load reads the catalog stored in r. seqVersion becomes the catalog's
// version.
func Load(r storage.Reader, version uint64) (*Catalog, error) {
	it := r.Iter(keys.Range(keys.Prefix(keys.PrefixCatalog)))
	defer it.Close()
	var raws []rawDef
	for it.Next() {
		typ, id, err := keys.ParseCatalog(it.Key())
		if err != nil {
			return nil, err
		}
		v, err := value.Decode(it.Value())
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		l, ok := v.(value.List)
		if !ok || l.Len() == 0 {
			return nil, fmt.Errorf("catalog: definition %s is not a list", id)
		}
		name, ok := l.At(0).(value.Text)
		if !ok {
			return nil, fmt.Errorf("catalog: definition %s has no name", id)
		}
		raws = append(raws, rawDef{typ, id, name.Go(), l.Values()[1:]})
	}
	if err := it.Err(); err != nil {
		return nil, err
	}

	// First pass: names, so types can be decoded.
	c := New()
	c.version = version
	for _, d := range raws {
		switch d.typ {
		case defKind:
			c.kinds.byID[d.id] = &model.Kind{ID: d.id, Name: d.name}
			c.kinds.byName[d.name] = d.id
		case defRelation:
			c.relations.byID[d.id] = &model.RelationType{ID: d.id, Name: d.name}
			c.relations.byName[d.name] = d.id
		case defFormat:
			c.formats.byID[d.id] = &model.Format{ID: d.id, Name: d.name}
			c.formats.byName[d.name] = d.id
		case defIndex:
			c.indexes.byID[d.id] = &model.Index{ID: d.id, Name: d.name}
			c.indexes.byName[d.name] = d.id
		default:
			return nil, fmt.Errorf("catalog: unknown definition type %q", d.typ)
		}
	}

	// Second pass: the full definitions.
	for _, d := range raws {
		r := &rec{vs: d.vs}
		var err error
		switch d.typ {
		case defKind:
			k := c.kinds.byID[d.id]
			k.Doc, k.Closed = r.text(), r.bool()
			k.History.On = r.bool()
			k.History.Keep = durationOf(item[value.Duration](r))
			if r.err == nil {
				k.Fields, err = decodeFields(item[value.List](r), c)
			}
		case defRelation:
			rt := c.relations.byID[d.id]
			rt.Defined, rt.Doc = r.bool(), r.text()
			rt.From = model.Endpoint{Mode: model.EndpointMode(r.int()), Kind: item[value.ID](r)}
			rt.To = model.Endpoint{Mode: model.EndpointMode(r.int()), Kind: item[value.ID](r)}
			rt.MaxOut, rt.MaxIn = r.int(), r.int()
			rt.OnDelete = model.RelAction(r.int())
			if r.err == nil {
				rt.Props, err = decodeFields(item[value.List](r), c)
			}
		case defFormat:
			f := c.formats.byID[d.id]
			f.Base, f.Doc, f.Header = r.text(), r.text(), r.bool()
			if r.err == nil {
				f.Fields, err = decodeFields(item[value.List](r), c)
			}
		case defIndex:
			ix := c.indexes.byID[d.id]
			ix.Kind, ix.Unique, ix.Text = item[value.ID](r), r.bool(), r.bool()
			for _, kv := range item[value.List](r).Values() {
				kr := &rec{vs: listValues(kv)}
				key := model.IndexKey{Field: kr.text()}
				for _, sv := range item[value.List](kr).Values() {
					sr := &rec{vs: listValues(sv)}
					key.Path = append(key.Path, model.PathStep{Name: sr.text(), Attr: sr.bool(), IsPos: sr.bool(), Pos: sr.int()})
					if sr.err != nil {
						r.err = sr.err
					}
				}
				if kr.err != nil {
					r.err = kr.err
				}
				ix.Keys = append(ix.Keys, key)
			}
		}
		if err == nil {
			err = r.err
		}
		if err == nil && r.i != len(r.vs) {
			err = fmt.Errorf("definition has %d extra items", len(r.vs)-r.i)
		}
		if err != nil {
			return nil, fmt.Errorf("catalog: %s: %w", d.name, err)
		}
	}
	c.derived = buildDerived(c.formats)
	return c, nil
}

func listValues(v value.Value) []value.Value {
	if l, ok := v.(value.List); ok {
		return l.Values()
	}
	return nil
}

func durationOf(d value.Duration) time.Duration { return d.Go() }
