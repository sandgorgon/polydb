package keys

import (
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
