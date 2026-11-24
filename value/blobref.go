package value

import (
	"encoding/hex"
	"fmt"
	"strconv"
)

// BlobRef stands for a large text, bytes or image value kept in a
// database's blob store (spec/types.md §13). Only stored records hold
// one: the engine writes it in place of the value and replaces it with
// the value again when reading, so queries never see it.
type BlobRef struct {
	Of    Code     // CodeText, CodeBytes or CodeImage
	Hash  [32]byte // SHA-256 of the content
	Len   int64    // content length in bytes
	Media string   // a bytes value's media type
}

func (BlobRef) Code() Code { return CodeBlobRef }

func (r BlobRef) String() string {
	return fmt.Sprintf("blob(%s, %s, %s)", r.Of, hex.EncodeToString(r.Hash[:8]), strconv.FormatInt(r.Len, 10))
}

// Large returns the content of a text, bytes or image value and true,
// or false for any other value.
func Large(v Value) (content []byte, mediaType string, ok bool) {
	switch x := v.(type) {
	case Text:
		return []byte(x.s), "", true
	case Bytes:
		return x.b, x.mediaType, true
	case Image:
		return x.data, "", true
	}
	return nil, "", false
}

// LargeLen returns the content length of a text, bytes or image value,
// or -1 for any other value.
func LargeLen(v Value) int {
	switch x := v.(type) {
	case Text:
		return len(x.s)
	case Bytes:
		return len(x.b)
	case Image:
		return len(x.data)
	}
	return -1
}

// FromBlob rebuilds the value a BlobRef stands for from its content,
// which the caller must not change afterwards.
func FromBlob(r BlobRef, content []byte) (Value, error) {
	if int64(len(content)) != r.Len {
		return nil, fmt.Errorf("blob %s: content is %d bytes", r, len(content))
	}
	switch r.Of {
	case CodeText:
		if len(content) > MaxTextBytes {
			return nil, fmt.Errorf("text longer than %d bytes", MaxTextBytes)
		}
		return Text{string(content)}, nil // checked when it was written
	case CodeBytes:
		return Bytes{content, r.Media}, nil
	case CodeImage:
		return newImage(content)
	}
	return nil, fmt.Errorf("blob %s: not a text, bytes or image", r)
}
