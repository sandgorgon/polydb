package lang

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/sandgorgon/polydb/value"
)

// Large values (types.md §13.1) that a statement compares, searches or
// cuts are read a piece at a time here, so they never have to fit in
// memory: ordering comparisons (and sort, min, max, between), =,
// starts with, ends with, contains, matches and substr. Other uses load
// them (Evaluator.Load).

// pieceSize is how much of a large value is read at a time.
const pieceSize = 64 << 10

// content returns a text or bytes value's content as a reader, lazy or
// not; ok is false for other values.
func content(v value.Value) (r io.ReaderAt, n int64, ok bool) {
	switch x := v.(type) {
	case value.Lazy:
		if x.Code() == value.CodeText || x.Code() == value.CodeBytes {
			return x.Src, x.Len(), true
		}
	case value.Text:
		return strings.NewReader(x.Go()), int64(len(x.Go())), true
	case value.Bytes:
		b, _, _ := value.Large(x)
		return bytes.NewReader(b), int64(len(b)), true
	}
	return nil, 0, false
}

// standIn returns an empty value of a lazy text or bytes value's type,
// which sorts in the same group; other values stay.
func standIn(v value.Value) value.Value {
	if l, ok := v.(value.Lazy); ok {
		switch l.Code() {
		case value.CodeText:
			return value.Text{}
		case value.CodeBytes:
			return value.Bytes{}
		}
	}
	return v
}

// sameGroup is value.SameGroup with lazy values standing for their type.
func sameGroup(a, b value.Value) bool { return value.SameGroup(standIn(a), standIn(b)) }

// isLazy reports whether any of vs is a lazy value.
func isLazy(vs ...value.Value) bool {
	for _, v := range vs {
		if _, ok := v.(value.Lazy); ok {
			return true
		}
	}
	return false
}

// readAt reads len(p) bytes at off, reporting a failure at n.
func readAt(n Node, r io.ReaderAt, p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	if _, err := r.ReadAt(p, off); err != nil {
		return runErr(n, "internal", "reading a large value: %v", err)
	}
	return nil
}

// prefix returns the first value.KeyPrefixLen bytes of a text or bytes
// value, kept for the statement for lazy values, which sorting compares
// over and over.
func (ev *Evaluator) prefix(n Node, v value.Value) ([]byte, error) {
	l, isLazy := v.(value.Lazy)
	if isLazy {
		if p, ok := ev.prefixes[l.Ref]; ok {
			return p, nil
		}
	}
	r, size, _ := content(v)
	p := make([]byte, min(size, value.KeyPrefixLen))
	if err := readAt(n, r, p, 0); err != nil {
		return nil, err
	}
	if isLazy {
		if ev.prefixes == nil {
			ev.prefixes = map[value.BlobRef][]byte{}
		}
		ev.prefixes[l.Ref] = p
	}
	return p, nil
}

// Order compares a and b as value.Compare does, reading large values a
// piece at a time rather than loading them. Values of different groups
// compare by group, like value.Compare.
func (ev *Evaluator) Order(n Node, a, b value.Value) (int, error) {
	if !isLazy(a, b) {
		return value.Compare(a, b), nil
	}
	ra, na, okA := content(a)
	rb, nb, okB := content(b)
	if !okA || !okB || a.Code() != b.Code() {
		if !sameGroup(a, b) {
			// Different groups order by group, whatever the content.
			return value.Compare(standIn(a), standIn(b)), nil
		}
		var err error
		if a, err = ev.Load(n, a); err != nil {
			return 0, err
		}
		if b, err = ev.Load(n, b); err != nil {
			return 0, err
		}
		return value.Compare(a, b), nil
	}
	la, lazyA := a.(value.Lazy)
	lb, lazyB := b.(value.Lazy)
	if lazyA && lazyB && la.Ref.Hash == lb.Ref.Hash {
		return 0, nil
	}
	pa, err := ev.prefix(n, a)
	if err != nil {
		return 0, err
	}
	pb, err := ev.prefix(n, b)
	if err != nil {
		return 0, err
	}
	m := min(len(pa), len(pb))
	if c := bytes.Compare(pa[:m], pb[:m]); c != 0 {
		return c, nil
	}
	bufA, bufB := make([]byte, pieceSize), make([]byte, pieceSize)
	for off := int64(m); ; {
		k := min(int64(pieceSize), na-off, nb-off)
		if k <= 0 {
			return cmp64(na, nb), nil // one is a prefix of the other
		}
		if err := readAt(n, ra, bufA[:k], off); err != nil {
			return 0, err
		}
		if err := readAt(n, rb, bufB[:k], off); err != nil {
			return 0, err
		}
		if c := bytes.Compare(bufA[:k], bufB[:k]); c != 0 {
			return c, nil
		}
		off += k
	}
}

func cmp64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareLarge evaluates a comparison operator with a lazy operand.
func (ev *Evaluator) compareLarge(e Node, op string, x, y value.Value) (bool, error) {
	if value.IsNull(x) || value.IsNull(y) {
		if op == "=" || op == "!=" {
			return compare(e, op, x, y)
		}
		return false, nil
	}
	if x.Code() != y.Code() {
		// Text and bytes compare only with their own type.
		if op == "=" || op == "!=" {
			return op == "!=", nil
		}
		return false, runErr(e, "type", "can't compare %s with %s using %s", typeName(x), typeName(y), op)
	}
	c, err := ev.Order(e, x, y)
	if err != nil {
		return false, err
	}
	switch op {
	case "=":
		return c == 0, nil
	case "!=":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	}
	return c >= 0, nil
}

// textOpLarge evaluates starts with, ends with, contains or matches on a
// lazy text. ok is false when the operands aren't a lazy text and text.
func (ev *Evaluator) textOpLarge(e *Binary, x, y value.Value) (v value.Value, ok bool, err error) {
	l, isLazy := x.(value.Lazy)
	if !isLazy || l.Code() != value.CodeText {
		return nil, false, nil
	}
	if y, err = ev.Load(e.Y, y); err != nil {
		return nil, true, err
	}
	yt, isText := y.(value.Text)
	if !isText {
		return nil, false, nil
	}
	pat := yt.Go()
	switch e.Op {
	case "starts with", "ends with":
		if int64(len(pat)) > l.Len() {
			return value.Bool(false), true, nil
		}
		off := int64(0)
		if e.Op == "ends with" {
			off = l.Len() - int64(len(pat))
		}
		p := make([]byte, len(pat))
		if err := readAt(e, l.Src, p, off); err != nil {
			return nil, true, err
		}
		return value.Bool(string(p) == pat), true, nil
	case "contains":
		found, err := streamContains(e, l, pat)
		return value.Bool(found), true, err
	case "matches":
		re, err := ev.regexp(pat)
		if err != nil {
			return nil, true, runErr(e.Y, "type", "invalid regular expression: %v", err)
		}
		r := &errReader{r: io.NewSectionReader(l.Src, 0, l.Len())}
		m := re.MatchReader(bufio.NewReaderSize(r, pieceSize))
		if r.err != nil {
			return nil, true, runErr(e, "internal", "reading a large value: %v", r.err)
		}
		return value.Bool(m), true, nil
	}
	return nil, false, nil
}

// errReader keeps the first read error, which a RuneReader user like
// regexp can't report.
type errReader struct {
	r   io.Reader
	err error
}

func (r *errReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// streamContains reports whether a lazy text contains pat, reading it in
// pieces that overlap by len(pat)-1 bytes.
func streamContains(n Node, l value.Lazy, pat string) (bool, error) {
	if pat == "" {
		return true, nil
	}
	over := int64(len(pat) - 1)
	buf := make([]byte, max(pieceSize, 2*len(pat)))
	for off := int64(0); off < l.Len(); {
		k := min(int64(len(buf)), l.Len()-off)
		if err := readAt(n, l.Src, buf[:k], off); err != nil {
			return false, err
		}
		if strings.Contains(string(buf[:k]), pat) {
			return true, nil
		}
		if off+k == l.Len() {
			break
		}
		off += max(k-over, 1)
	}
	return false, nil
}

// substrLarge cuts n characters (all when n < 0) from a lazy text from
// character start, reading only up to the end of the cut. The result is
// counted against the budget.
func (ev *Evaluator) substrLarge(e Node, l value.Lazy, start, n int64) (value.Value, error) {
	var out []byte
	buf := make([]byte, pieceSize)
	chars := int64(0) // characters before the read position
	var carry []byte
	for off := int64(0); off < l.Len() && (n < 0 || chars < start+n); {
		k := min(int64(len(buf)), l.Len()-off)
		if err := readAt(e, l.Src, buf[:k], off); err != nil {
			return nil, err
		}
		off += k
		p := append(carry, buf[:k]...)
		carry = nil
		for i := 0; i < len(p); {
			if !utf8.FullRune(p[i:]) && off < l.Len() {
				carry = append([]byte(nil), p[i:]...)
				break
			}
			_, size := utf8.DecodeRune(p[i:])
			if chars >= start && (n < 0 || chars < start+n) {
				out = append(out, p[i:i+size]...)
				if err := ev.charge(e, int64(size)); err != nil {
					return nil, err
				}
			}
			chars++
			i += size
		}
	}
	return built(value.NewText(string(out)))
}

// charge counts n bytes against the budget.
func (ev *Evaluator) charge(e Node, n int64) error {
	if err := ev.Budget.Load(n); err != nil {
		return runErr(e, "limit", "%v", err)
	}
	return nil
}
