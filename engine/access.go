package engine

import (
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// An Access is an ordered index on a kind that the query planner can
// read: a ready non-unique value index, a unique index, or a unique
// field. Its entries are ordered by the key encoding of their values
// (value.Key), then, for non-unique indexes, by entity ID. An entity has
// an entry only when every key has a value that isn't null and can be a
// key.
type Access struct {
	Name   string // the index's name, or the unique field's
	Index  *model.Index
	Keys   []model.IndexKey
	Unique bool
	prefix []byte
	entry  bool // the entity ID ends the key (non-unique), not the value
}

// Accesses returns the ordered indexes the planner may use on a kind:
// its ready value indexes, unique indexes and unique fields.
func (tx *Tx) Accesses(k *model.Kind) ([]*Access, error) {
	var out []*Access
	for i := range k.Fields {
		if fd := &k.Fields[i]; fd.Unique {
			out = append(out, &Access{
				Name:   fd.Name,
				Keys:   []model.IndexKey{{Field: fd.Name}},
				Unique: true,
				prefix: keys.UniquePrefix(k.ID, fd.Name),
			})
		}
	}
	for _, ix := range tx.Catalog().IndexesOn(k.ID) {
		if ix.Text {
			continue
		}
		a := &Access{Name: ix.Name, Index: ix, Keys: ix.Keys, Unique: ix.Unique}
		if ix.Unique {
			a.prefix = keys.UniquePrefix(ix.ID, "")
		} else {
			st, err := tx.IndexState(ix)
			if err != nil {
				return nil, err
			}
			if !st.Ready {
				continue
			}
			a.prefix, a.entry = keys.IndexPrefix(ix.ID), true
		}
		out = append(out, a)
	}
	return out, nil
}

// TextIndexes returns the ready text indexes on a kind.
func (tx *Tx) TextIndexes(k *model.Kind) ([]*model.Index, error) {
	var out []*model.Index
	for _, ix := range tx.Catalog().IndexesOn(k.ID) {
		if !ix.Text {
			continue
		}
		st, err := tx.IndexState(ix)
		if err != nil {
			return nil, err
		}
		if st.Ready {
			out = append(out, ix)
		}
	}
	return out, nil
}

// ScanAccess calls fn with the entities whose concatenated value keys are
// in [lower, upper), in key order or, with reverse, the opposite. A nil
// bound is open. It stops when fn returns false.
func (tx *Tx) ScanAccess(a *Access, lower, upper []byte, reverse bool, fn func(id value.ID) bool) error {
	opts := keys.Range(a.prefix)
	opts.Reverse = reverse
	if lower != nil {
		opts.Lower = append(append([]byte(nil), a.prefix...), lower...)
	}
	if upper != nil {
		opts.Upper = append(append([]byte(nil), a.prefix...), upper...)
	}
	it := tx.t.Iter(opts)
	defer it.Close()
	for it.Next() {
		var id value.ID
		if a.entry {
			k := it.Key()
			if len(k) < len(a.prefix)+16 {
				return errorf(CodeInternal, "index %s: short entry", a.Name)
			}
			copy(id[:], k[len(k)-16:])
		} else {
			v := it.Value()
			if len(v) != 16 {
				return errorf(CodeInternal, "unique %s: bad entry", a.Name)
			}
			copy(id[:], v)
		}
		if !fn(id) {
			return nil
		}
	}
	return wrap(CodeInternal, it.Err())
}
