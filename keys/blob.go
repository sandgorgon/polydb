package keys

import (
	"fmt"

	"github.com/sandgorgon/polydb/value"
)

// Large values in the blob store (spec/types.md §13, engine/blob.go):
//
//	0x50 blob      0x50 hash                   a blob: its length and chunks
//	0x51 chunk     0x51 hash                   where a chunk is in the segments
//	0x52 bloburef  0x52 hash entity            entity may reference the blob
//	0x53 blobent   0x53 entity hash            the same, by entity
//	0x54 blobdirty 0x54 entity                 entity may have dropped references
//	0x55 blobcheck 0x55 hash                   blob may be unreferenced
//
// Hashes are SHA-256: a blob's of its whole content, a chunk's of its
// data. A field record or version holding a value.BlobRef has a use
// record (bloburef and blobent) for its blob.
const (
	PrefixBlob      = 0x50
	PrefixChunk     = 0x51
	PrefixBlobUse   = 0x52
	PrefixBlobEnt   = 0x53
	PrefixBlobDirty = 0x54
	PrefixBlobCheck = 0x55
)

const hashLen = 32

func hashKey(prefix byte, h [32]byte) []byte { return append([]byte{prefix}, h[:]...) }

// Blob returns the key of a blob record.
func Blob(h [32]byte) []byte { return hashKey(PrefixBlob, h) }

// Chunk returns the key of a chunk's location.
func Chunk(h [32]byte) []byte { return hashKey(PrefixChunk, h) }

// BlobUse returns the key recording that entity may reference blob h.
func BlobUse(h [32]byte, entity value.ID) []byte {
	return append(hashKey(PrefixBlobUse, h), entity[:]...)
}

// BlobUses returns the prefix of blob h's use records.
func BlobUses(h [32]byte) []byte { return hashKey(PrefixBlobUse, h) }

// BlobEnt returns the by-entity key of a use record.
func BlobEnt(entity value.ID, h [32]byte) []byte {
	return append(key(PrefixBlobEnt, entity), h[:]...)
}

// BlobEnts returns the prefix of an entity's use records.
func BlobEnts(entity value.ID) []byte { return key(PrefixBlobEnt, entity) }

// BlobDirty returns the key marking that entity may have dropped blob
// references.
func BlobDirty(entity value.ID) []byte { return key(PrefixBlobDirty, entity) }

// BlobCheck returns the key marking that blob h may be unreferenced.
func BlobCheck(h [32]byte) []byte { return hashKey(PrefixBlobCheck, h) }

// KeyHash returns the hash right after a key's prefix byte (blob, chunk,
// bloburef and blobcheck keys) or after the prefix and an entity
// (blobent keys).
func KeyHash(k []byte) ([32]byte, error) {
	var h [32]byte
	switch {
	case len(k) == 1+hashLen && (k[0] == PrefixBlob || k[0] == PrefixChunk || k[0] == PrefixBlobCheck):
		copy(h[:], k[1:])
	case len(k) == 1+hashLen+idLen && k[0] == PrefixBlobUse:
		copy(h[:], k[1:])
	case len(k) == 1+idLen+hashLen && k[0] == PrefixBlobEnt:
		copy(h[:], k[1+idLen:])
	default:
		return h, badKey("blob", k)
	}
	return h, nil
}

// KeyEntity returns the entity in a blobdirty, blobent or bloburef key.
func KeyEntity(k []byte) (value.ID, error) {
	switch {
	case len(k) == 0:
	case (k[0] == PrefixBlobDirty || k[0] == PrefixBlobEnt) && len(k) >= 1+idLen:
		return value.ID(k[1 : 1+idLen]), nil
	case k[0] == PrefixBlobUse && len(k) == 1+hashLen+idLen:
		return value.ID(k[1+hashLen:]), nil
	}
	return value.ID{}, badKey("blob", k)
}

// BlobRecord is a blob's length and the hashes of its chunks, in order.
type BlobRecord struct {
	Len    int64
	Chunks [][32]byte
}

// EncodeBlob encodes a blob record.
func EncodeBlob(r BlobRecord) []byte {
	hs := make([]byte, 0, len(r.Chunks)*hashLen)
	for _, h := range r.Chunks {
		hs = append(hs, h[:]...)
	}
	return list(value.Int(r.Len), must(value.NewBytes(hs, "")))
}

// DecodeBlob decodes a blob record.
func DecodeBlob(b []byte) (BlobRecord, error) {
	vs, err := fields(b, "blob", 2)
	if err != nil {
		return BlobRecord{}, err
	}
	r := &reader{what: "blob", vs: vs}
	n := get[value.Int](r)
	hs := get[value.Bytes](r).Go()
	if r.err != nil {
		return BlobRecord{}, r.err
	}
	if len(hs)%hashLen != 0 || n < 0 {
		return BlobRecord{}, fmt.Errorf("keys: blob record has %d hash bytes", len(hs))
	}
	out := BlobRecord{Len: int64(n), Chunks: make([][32]byte, len(hs)/hashLen)}
	for i := range out.Chunks {
		copy(out.Chunks[i][:], hs[i*hashLen:])
	}
	return out, nil
}

// ChunkLoc is where a chunk is in the blob segments.
type ChunkLoc struct {
	Seg uint32
	Off int64
	Len uint32
}

// EncodeChunk encodes a chunk's location.
func EncodeChunk(l ChunkLoc) []byte {
	return list(value.Int(l.Seg), value.Int(l.Off), value.Int(l.Len))
}

// DecodeChunk decodes a chunk's location.
func DecodeChunk(b []byte) (ChunkLoc, error) {
	vs, err := fields(b, "chunk", 3)
	if err != nil {
		return ChunkLoc{}, err
	}
	r := &reader{what: "chunk", vs: vs}
	seg, off, n := get[value.Int](r), get[value.Int](r), get[value.Int](r)
	if r.err != nil {
		return ChunkLoc{}, r.err
	}
	if seg <= 0 || seg > 1<<32-1 || off < 0 || n < 0 || n > 1<<32-1 {
		return ChunkLoc{}, fmt.Errorf("keys: bad chunk location %d/%d/%d", seg, off, n)
	}
	return ChunkLoc{uint32(seg), int64(off), uint32(n)}, nil
}
