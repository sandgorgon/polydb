package value

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// Lazy is a large text, bytes or image value whose content is still in a
// blob store (spec/types.md §13.1). Statements pass it along without
// reading it: showing it, storing it in another field, its length and
// equality need only the reference. Anything that needs the content
// loads it first with Load.
//
// Code returns the code of the value it stands for, so type checks see
// a text, bytes or image value. Compare, Equal and Key don't read the
// content: callers that compare or key large values load them first.
type Lazy struct {
	Ref BlobRef
	Src io.ReaderAt // the content, Ref.Len bytes
}

func (l Lazy) Code() Code { return l.Ref.Of }

func (l Lazy) String() string {
	return fmt.Sprintf("<%s of %s bytes>", l.Ref.Of, strconv.FormatInt(l.Ref.Len, 10))
}

// Len returns the content's length in bytes.
func (l Lazy) Len() int64 { return l.Ref.Len }

// MediaType returns a bytes value's media type.
func (l Lazy) MediaType() string { return l.Ref.Media }

// Load reads the content and returns the value it stands for.
func (l Lazy) Load() (Value, error) {
	if l.Ref.Len > MaxBytesLen {
		return nil, fmt.Errorf("%s is too long to load", l)
	}
	content := make([]byte, l.Ref.Len)
	if _, err := l.Src.ReadAt(content, 0); err != nil && !(errors.Is(err, io.EOF) && l.Ref.Len == 0) {
		return nil, fmt.Errorf("reading %s: %w", l, err)
	}
	return FromBlob(l.Ref, content)
}

// RuneCount counts the characters of a lazy text, reading its content a
// piece at a time.
func (l Lazy) RuneCount() (int64, error) {
	var n int64
	buf := make([]byte, 1<<20)
	for off := int64(0); off < l.Ref.Len; {
		k := min(int64(len(buf)), l.Ref.Len-off)
		if _, err := l.Src.ReadAt(buf[:k], off); err != nil {
			return 0, fmt.Errorf("reading %s: %w", l, err)
		}
		// The text is valid UTF-8, so every byte that isn't a
		// continuation byte starts a character.
		for _, b := range buf[:k] {
			if b&0xc0 != 0x80 {
				n++
			}
		}
		off += k
	}
	return n, nil
}

// Load returns v with a lazy value loaded, or v itself.
func Load(v Value) (Value, error) {
	if l, ok := v.(Lazy); ok {
		return l.Load()
	}
	return v, nil
}

// TextChecker validates text arriving in pieces: valid UTF-8 overall,
// with characters allowed to span pieces.
type TextChecker struct {
	carry []byte
	bad   bool
}

// Write checks the next piece.
func (c *TextChecker) Write(p []byte) (int, error) {
	n := len(p)
	if c.bad {
		return n, nil
	}
	if len(c.carry) > 0 {
		p = append(c.carry, p...)
		c.carry = nil
	}
	// Keep an incomplete character at the end for the next piece.
	cut := len(p)
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				cut = i
			}
			break
		}
	}
	if !utf8.Valid(p[:cut]) {
		c.bad = true
		return n, nil
	}
	c.carry = append([]byte(nil), p[cut:]...)
	return n, nil
}

// Err reports whether all the pieces made valid UTF-8.
func (c *TextChecker) Err() error {
	if c.bad || len(c.carry) > 0 {
		return errors.New("text is not valid UTF-8")
	}
	return nil
}

// ImageFormat checks that r starts with the header of a supported image
// and returns its format, reading only the header.
func ImageFormat(r io.Reader) (string, error) {
	c, err := imageConfig(bufio.NewReaderSize(r, 64<<10))
	return c.format, err
}
