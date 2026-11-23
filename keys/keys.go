// Package keys maps PolyDB's graph model onto the ordered key-value store.
//
// Each database has its own store. Keys start with a one-byte prefix; IDs
// are 16 bytes, so most keys are fixed-width concatenations:
//
//	0x01 meta     0x01 name                    database settings (format version)
//	0x02 catalog  0x02 type id                 one catalog definition
//	0x10 entity   0x10 entity                  entity header: kind and metadata
//	0x11 field    0x11 entity name             one field: id, value, and its
//	                                           definition if added
//	0x12 node     0x12 id                      what an ID names (entity, field
//	                                           with its entity and name, or relation)
//	0x13 member   0x13 kind entity             entities of each kind
//	0x14 backref  0x14 target field            a restrict ref field pointing at target
//	0x20 relation 0x20 relation                type, endpoints, properties
//	0x21 out      0x21 from type to relation   adjacency, by start node
//	0x22 in       0x22 to type from relation   adjacency, by end node
//	0x23 bytype   0x23 type relation           relations of each type
//	0x30 index    (Phase 5)                    secondary index entries
//	0x31 unique   0x31 owner name 0x00 values  the entity holding a unique value;
//	                                           owner is the kind (unique field,
//	                                           name = field) or a unique index
//	                                           (name empty), values are key
//	                                           encodings (value.Key)
//	0x40 history  0x40 entity len name time    opt-in field history (history.go)
//	0x41 histkind 0x41 kind entity
//	0x42 life     0x42 entity
//
// An entity's fields are contiguous under its field prefix, so reading a
// whole entity is one short range scan. Kinds and relation types appear in
// keys by ID, so renaming them never rewrites data.
package keys

import (
	"bytes"
	"fmt"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/value"
)

// Key prefixes.
const (
	PrefixMeta     = 0x01
	PrefixCatalog  = 0x02
	PrefixEntity   = 0x10
	PrefixField    = 0x11
	PrefixNode     = 0x12
	PrefixMember   = 0x13
	PrefixBackRef  = 0x14
	PrefixRelation = 0x20
	PrefixOut      = 0x21
	PrefixIn       = 0x22
	PrefixByType   = 0x23
	PrefixIndex    = 0x30
	PrefixUnique   = 0x31
	PrefixHistory  = 0x40
)

const idLen = 16

func key(prefix byte, ids ...value.ID) []byte {
	k := make([]byte, 1, 1+idLen*len(ids))
	k[0] = prefix
	for _, id := range ids {
		k = append(k, id[:]...)
	}
	return k
}

// Prefix returns the one-byte prefix itself, for scanning a whole section.
func Prefix(p byte) []byte { return []byte{p} }

// Range returns the bounds for scanning every key that starts with prefix.
func Range(prefix []byte) storage.IterOptions {
	return storage.IterOptions{Lower: prefix, Upper: storage.PrefixEnd(prefix)}
}

// Meta returns the key of a database setting.
func Meta(name string) []byte { return append([]byte{PrefixMeta}, name...) }

// Catalog returns the key of a catalog definition of the given type.
func Catalog(typ byte, id value.ID) []byte {
	return append([]byte{PrefixCatalog, typ}, id[:]...)
}

// ParseCatalog splits a catalog key.
func ParseCatalog(k []byte) (typ byte, id value.ID, err error) {
	if len(k) != 2+idLen || k[0] != PrefixCatalog {
		return 0, id, badKey("catalog", k)
	}
	return k[1], value.ID(k[2:]), nil
}

// Entity returns the key of an entity header.
func Entity(id value.ID) []byte { return key(PrefixEntity, id) }

// Field returns the key of one field of an entity.
func Field(entity value.ID, name string) []byte {
	return append(key(PrefixField, entity), name...)
}

// Fields returns the prefix of all fields of an entity.
func Fields(entity value.ID) []byte { return key(PrefixField, entity) }

// ParseField splits a field key.
func ParseField(k []byte) (entity value.ID, name string, err error) {
	if len(k) <= 1+idLen || k[0] != PrefixField {
		return entity, "", badKey("field", k)
	}
	return value.ID(k[1 : 1+idLen]), string(k[1+idLen:]), nil
}

// Node returns the key of the record saying what an ID names.
func Node(id value.ID) []byte { return key(PrefixNode, id) }

// Member returns the key recording that entity is of kind.
func Member(kind, entity value.ID) []byte { return key(PrefixMember, kind, entity) }

// Members returns the prefix of all entities of kind.
func Members(kind value.ID) []byte { return key(PrefixMember, kind) }

// ParseMember returns the entity in a member key.
func ParseMember(k []byte) (kind, entity value.ID, err error) {
	if len(k) != 1+2*idLen || k[0] != PrefixMember {
		return kind, entity, badKey("member", k)
	}
	return value.ID(k[1 : 1+idLen]), value.ID(k[1+idLen:]), nil
}

// BackRef returns the key recording that the restrict ref field holds a
// ref to target.
func BackRef(target, field value.ID) []byte { return key(PrefixBackRef, target, field) }

// BackRefs returns the prefix of all back references to target.
func BackRefs(target value.ID) []byte { return key(PrefixBackRef, target) }

// ByType returns the key listing a relation under its type.
func ByType(typ, rel value.ID) []byte { return key(PrefixByType, typ, rel) }

// OfType returns the prefix of all relations of a type.
func OfType(typ value.ID) []byte { return key(PrefixByType, typ) }

// Unique returns the key claiming a unique value. name is the field name
// for a unique field and empty for a unique index; vals is the
// concatenated key encoding of the values.
func Unique(owner value.ID, name string, vals []byte) []byte {
	k := append(key(PrefixUnique, owner), name...)
	return append(append(k, 0), vals...)
}

// UniquePrefix returns the prefix of all unique keys of one owner and name.
func UniquePrefix(owner value.ID, name string) []byte {
	return append(append(key(PrefixUnique, owner), name...), 0)
}

// CatalogVersion is the key every catalog change writes and every data
// change guards, so the two can't interleave.
var CatalogVersion = Meta("catalog")

// Relation returns the key of a relation record.
func Relation(id value.ID) []byte { return key(PrefixRelation, id) }

// Out returns the outgoing adjacency key of a relation.
func Out(from, typ, to, rel value.ID) []byte { return key(PrefixOut, from, typ, to, rel) }

// In returns the incoming adjacency key of a relation.
func In(to, typ, from, rel value.ID) []byte { return key(PrefixIn, to, typ, from, rel) }

// Adjacent returns the prefix of a node's outgoing (PrefixOut) or incoming
// (PrefixIn) adjacency keys, optionally limited to one relation type.
func Adjacent(dir byte, node value.ID, typ ...value.ID) []byte {
	return key(dir, append([]value.ID{node}, typ...)...)
}

// Edge is a parsed adjacency key. For an out key Node is the start and
// Other the end; for an in key the other way round.
type Edge struct {
	Dir      byte // PrefixOut or PrefixIn
	Node     value.ID
	Type     value.ID
	Other    value.ID
	Relation value.ID
}

// ParseEdge splits an adjacency key.
func ParseEdge(k []byte) (Edge, error) {
	if len(k) != 1+4*idLen || (k[0] != PrefixOut && k[0] != PrefixIn) {
		return Edge{}, badKey("adjacency", k)
	}
	id := func(i int) value.ID { return value.ID(k[1+i*idLen : 1+(i+1)*idLen]) }
	return Edge{Dir: k[0], Node: id(0), Type: id(1), Other: id(2), Relation: id(3)}, nil
}

func badKey(what string, k []byte) error {
	return fmt.Errorf("keys: malformed %s key %x", what, k)
}

// HasPrefix reports whether k starts with p.
func HasPrefix(k, p []byte) bool { return bytes.HasPrefix(k, p) }
