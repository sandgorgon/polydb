package keys

import (
	"encoding/binary"
	"fmt"

	"github.com/sandgorgon/polydb/value"
)

// Secondary indexes (spec/rhizome.md §5.4):
//
//	0x30 index    0x30 index values entity     an entity's entry in a
//	                                           non-unique index; values are
//	                                           the key encodings (value.Key)
//	                                           of its keys, in order
//	0x01 meta     0x01 "ixbuild/" index        an index still being built:
//	                                           how far the builder got
//
// Entries sort by value, then entity, so an equality or range condition
// is one range scan.

// IndexEntry returns the key of an entity's entry in an index.
func IndexEntry(ix value.ID, vals []byte, entity value.ID) []byte {
	k := append(key(PrefixIndex, ix), vals...)
	return append(k, entity[:]...)
}

// IndexPrefix returns the prefix of all of an index's entries.
func IndexPrefix(ix value.ID) []byte { return key(PrefixIndex, ix) }

// ParseIndexEntry splits an entry key into its values and entity.
func ParseIndexEntry(k []byte) (vals []byte, entity value.ID, err error) {
	if len(k) < 1+2*idLen || k[0] != PrefixIndex {
		return nil, entity, badKey("index", k)
	}
	return k[1+idLen : len(k)-idLen], value.ID(k[len(k)-idLen:]), nil
}

// IndexBuild returns the key of an index's build state.
func IndexBuild(ix value.ID) []byte { return append(Meta("ixbuild/"), ix[:]...) }

// IndexBuilds returns the prefix of every build state.
func IndexBuilds() []byte { return Meta("ixbuild/") }

// BuildState is how far a background index build got: every entity of
// the kind with an ID up to After is indexed. Done counts them.
type BuildState struct {
	After value.ID // zero: nothing yet
	Done  int64
}

// EncodeBuildState encodes a build state.
func EncodeBuildState(b BuildState) []byte {
	return list(optID(b.After), value.Int(b.Done))
}

// DecodeBuildState decodes a build state.
func DecodeBuildState(b []byte) (BuildState, error) {
	vs, err := fields(b, "index build", 2)
	if err != nil {
		return BuildState{}, err
	}
	r := &reader{what: "index build", vs: vs}
	s := BuildState{After: r.optID(), Done: int64(get[value.Int](r))}
	return s, r.err
}

// Full-text indexes share the 0x30 prefix, with one entry per word, per
// entity and per indexed field:
//
//	0x30 index word 0x00 entity field          the word's positions in that
//	                                           field (uvarint deltas)
//
// Words never contain 0x00, so one word's entries are one range scan
// (TextWord) and so are all the words with a prefix (TextPrefix).

// TextEntry returns the key of a word's entry.
func TextEntry(ix value.ID, word string, entity value.ID, field int) []byte {
	k := append(key(PrefixIndex, ix), word...)
	k = append(append(k, 0), entity[:]...)
	return append(k, byte(field))
}

// TextWord returns the prefix of one word's entries.
func TextWord(ix value.ID, word string) []byte {
	return append(append(key(PrefixIndex, ix), word...), 0)
}

// TextPrefix returns the prefix of the entries of every word that starts
// with prefix.
func TextPrefix(ix value.ID, prefix string) []byte {
	return append(key(PrefixIndex, ix), prefix...)
}

// ParseTextEntry splits a text entry's key.
func ParseTextEntry(k []byte) (word string, entity value.ID, field int, err error) {
	n := len(k)
	if n < 1+idLen+1+idLen+1 || k[0] != PrefixIndex || k[n-idLen-2] != 0 {
		return "", entity, 0, badKey("text index", k)
	}
	return string(k[1+idLen : n-idLen-2]), value.ID(k[n-idLen-1 : n-1]), int(k[n-1]), nil
}

// EncodePositions encodes ascending word positions.
func EncodePositions(ps []int) []byte {
	var b []byte
	prev := 0
	for _, p := range ps {
		b = binary.AppendUvarint(b, uint64(p-prev))
		prev = p
	}
	return b
}

// DecodePositions decodes word positions.
func DecodePositions(b []byte) ([]int, error) {
	var out []int
	prev := 0
	for len(b) > 0 {
		d, n := binary.Uvarint(b)
		if n <= 0 || d > 1<<31 {
			return nil, fmt.Errorf("keys: malformed word positions")
		}
		prev += int(d)
		out = append(out, prev)
		b = b[n:]
	}
	return out, nil
}
