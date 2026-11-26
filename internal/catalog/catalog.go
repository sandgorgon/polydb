// Package catalog holds a database's definitions: kinds, relation types,
// derived formats and indexes.
//
// A Catalog is an immutable snapshot. Transactions read one snapshot;
// changes are made through an Edit, which validates them, writes them into
// the same storage batch as the data changes they go with, and produces
// the next snapshot. Definitions returned by a Catalog are shared and must
// not be modified.
//
// Stored definitions refer to kinds and derived formats by ID, so renaming
// a kind rewrites only its own definition.
package catalog

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// Definition types, the second byte of catalog keys.
const (
	defKind     = 'k'
	defRelation = 'r'
	defFormat   = 'f'
	defIndex    = 'i'
)

// table is one namespace of definitions, by ID and by name.
type table[T any] struct {
	byID   map[value.ID]*T
	byName map[string]value.ID
}

func newTable[T any]() table[T] {
	return table[T]{map[value.ID]*T{}, map[string]value.ID{}}
}

func (t table[T]) clone() table[T] {
	return table[T]{maps.Clone(t.byID), maps.Clone(t.byName)}
}

func (t table[T]) get(name string) (*T, bool) {
	id, ok := t.byName[name]
	if !ok {
		return nil, false
	}
	return t.byID[id], true
}

func sorted[T any](t table[T], name func(*T) string) []*T {
	out := slices.Collect(maps.Values(t.byID))
	slices.SortFunc(out, func(a, b *T) int { return cmp.Compare(name(a), name(b)) })
	return out
}

// Catalog is an immutable snapshot of a database's definitions.
type Catalog struct {
	version   uint64
	kinds     table[model.Kind]
	relations table[model.RelationType]
	formats   table[model.Format]
	indexes   table[model.Index]
	derived   map[string]*value.DerivedFormat
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{
		kinds:     newTable[model.Kind](),
		relations: newTable[model.RelationType](),
		formats:   newTable[model.Format](),
		indexes:   newTable[model.Index](),
		derived:   map[string]*value.DerivedFormat{},
	}
}

// Version increases by one with every committed edit.
func (c *Catalog) Version() uint64 { return c.version }

func (c *Catalog) Kind(name string) (*model.Kind, bool)     { return c.kinds.get(name) }
func (c *Catalog) KindByID(id value.ID) (*model.Kind, bool) { k, ok := c.kinds.byID[id]; return k, ok }

// Kinds returns all kinds sorted by name.
func (c *Catalog) Kinds() []*model.Kind {
	return sorted(c.kinds, func(k *model.Kind) string { return k.Name })
}

func (c *Catalog) RelationType(name string) (*model.RelationType, bool) {
	return c.relations.get(name)
}

func (c *Catalog) RelationTypeByID(id value.ID) (*model.RelationType, bool) {
	r, ok := c.relations.byID[id]
	return r, ok
}

// RelationTypes returns all relation types, defined or not, sorted by name.
func (c *Catalog) RelationTypes() []*model.RelationType {
	return sorted(c.relations, func(r *model.RelationType) string { return r.Name })
}

// FormatDef returns a derived format's definition.
func (c *Catalog) FormatDef(name string) (*model.Format, bool) { return c.formats.get(name) }

// Formats returns the derived formats sorted by name.
func (c *Catalog) Formats() []*model.Format {
	return sorted(c.formats, func(f *model.Format) string { return f.Name })
}

// Format returns the format with the given name: a derived format of this
// database or a base format registered in Go.
func (c *Catalog) Format(name string) (value.Format, bool) {
	if f, ok := c.derived[name]; ok {
		return f, true
	}
	return value.LookupFormat(name)
}

func (c *Catalog) Index(name string) (*model.Index, bool) { return c.indexes.get(name) }
func (c *Catalog) IndexByID(id value.ID) (*model.Index, bool) {
	ix, ok := c.indexes.byID[id]
	return ix, ok
}

// Indexes returns all indexes sorted by name.
func (c *Catalog) Indexes() []*model.Index {
	return sorted(c.indexes, func(ix *model.Index) string { return ix.Name })
}

// IndexesOn returns the indexes on one kind, sorted by name.
func (c *Catalog) IndexesOn(kind value.ID) []*model.Index {
	var out []*model.Index
	for _, ix := range c.Indexes() {
		if ix.Kind == kind {
			out = append(out, ix)
		}
	}
	return out
}

// --- types in storage ---

// EncodeType encodes t for storage, writing kinds and derived formats as
// "#" and their ID. It implements keys.TypeCodec.
func (c *Catalog) EncodeType(t *value.Type) ([]byte, error) {
	st, err := t.MapNames(func(code value.Code, name string) (string, error) {
		if code == value.CodeRef {
			if id, ok := c.kinds.byName[name]; ok {
				return "#" + id.Base32(), nil
			}
			return "", fmt.Errorf("unknown kind %q", name)
		}
		if id, ok := c.formats.byName[name]; ok {
			return "#" + id.Base32(), nil
		}
		if _, ok := value.LookupFormat(name); ok {
			return name, nil
		}
		return "", fmt.Errorf("unknown format %q", name)
	})
	if err != nil {
		return nil, err
	}
	return value.AppendType(nil, st), nil
}

// DecodeType decodes a stored type, turning IDs back into names. A ref to
// a kind that has since been dropped becomes an unconstrained ref.
func (c *Catalog) DecodeType(b []byte) (*value.Type, error) {
	t, err := value.DecodeType(b)
	if err != nil {
		return nil, err
	}
	return t.MapNames(func(code value.Code, name string) (string, error) {
		id, isID := strings.CutPrefix(name, "#")
		if !isID {
			return name, nil
		}
		pid, err := value.ParseID(id)
		if err != nil {
			return "", err
		}
		if code == value.CodeRef {
			if k, ok := c.kinds.byID[pid]; ok {
				return k.Name, nil
			}
			return "", nil
		}
		if f, ok := c.formats.byID[pid]; ok {
			return f.Name, nil
		}
		return "", fmt.Errorf("stored type uses format %s, which was dropped", name)
	})
}

var _ keys.TypeCodec = (*Catalog)(nil)
