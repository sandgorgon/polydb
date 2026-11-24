package value

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Value is an immutable PolyDB value.
type Value interface {
	// Code returns the value's type family.
	Code() Code
	// String returns the value in Rhizome literal syntax, for example
	// `"hello"`, `19.99d` or `t"2026-11-21T14:30:00Z"`.
	String() string
}

// Size limits (spec/types.md §14).
const (
	MaxTextBytes   = 64 << 20
	MaxBytesLen    = 4 << 30
	MaxBigIntBytes = 4096
)

// --- null ---

type nullValue struct{}

// Null is the null value.
var Null Value = nullValue{}

func (nullValue) Code() Code     { return CodeNull }
func (nullValue) String() string { return "null" }

// IsNull reports whether v is null (or a nil interface).
func IsNull(v Value) bool {
	if v == nil {
		return true
	}
	_, ok := v.(nullValue)
	return ok
}

// --- bool ---

// Bool is a boolean value.
type Bool bool

func (Bool) Code() Code { return CodeBool }
func (b Bool) String() string {
	if b {
		return "true"
	}
	return "false"
}

// --- int ---

// Int is a signed 64-bit integer value.
type Int int64

func (Int) Code() Code       { return CodeInt }
func (i Int) String() string { return strconv.FormatInt(int64(i), 10) }

// --- bigint ---

// BigInt is an arbitrary-precision integer value.
type BigInt struct{ v *big.Int }

// NewBigInt returns a BigInt holding a copy of v. It fails if the magnitude
// is larger than MaxBigIntBytes.
func NewBigInt(v *big.Int) (BigInt, error) {
	if (v.BitLen()+7)/8 > MaxBigIntBytes {
		return BigInt{}, fmt.Errorf("bigint larger than %d bytes", MaxBigIntBytes)
	}
	return BigInt{new(big.Int).Set(v)}, nil
}

// ParseBigInt parses a decimal integer (without the "n" suffix).
func ParseBigInt(s string) (BigInt, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return BigInt{}, fmt.Errorf("invalid bigint %q", s)
	}
	return NewBigInt(v)
}

func (BigInt) Code() Code { return CodeBigInt }

// Go returns a copy of the value as a *big.Int.
func (b BigInt) Go() *big.Int {
	if b.v == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(b.v)
}

func (b BigInt) big() *big.Int {
	if b.v == nil {
		return new(big.Int)
	}
	return b.v
}

func (b BigInt) String() string { return b.big().String() + "n" }

// --- float ---

// Float is an IEEE 754 binary64 value. -0 is stored as +0 and every NaN as
// one canonical NaN (spec/types.md §4.4).
type Float struct{ f float64 }

// NewFloat returns f as a Float, normalized.
func NewFloat(f float64) Float {
	switch {
	case f == 0:
		f = 0 // turns -0 into +0
	case math.IsNaN(f):
		f = math.NaN()
	}
	return Float{f}
}

func (Float) Code() Code { return CodeFloat }

// Go returns the value as a float64.
func (f Float) Go() float64 { return f.f }

func (f Float) String() string {
	switch {
	case math.IsNaN(f.f):
		return "nan"
	case math.IsInf(f.f, 1):
		return "inf"
	case math.IsInf(f.f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f.f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0" // keep it a float literal, not an int
	}
	return s
}

// --- text ---

// Text is a text value: valid UTF-8, normalized to NFC.
type Text struct{ s string }

// NewText validates s and normalizes it to NFC. It fails on invalid UTF-8 or
// text longer than MaxTextBytes.
func NewText(s string) (Text, error) {
	if !utf8.ValidString(s) {
		return Text{}, errors.New("text is not valid UTF-8")
	}
	s = norm.NFC.String(s)
	if len(s) > MaxTextBytes {
		return Text{}, fmt.Errorf("text longer than %d bytes", MaxTextBytes)
	}
	return Text{s}, nil
}

// MustText is NewText for strings known to be valid. It panics on error.
func MustText(s string) Text {
	t, err := NewText(s)
	if err != nil {
		panic(err)
	}
	return t
}

func (Text) Code() Code { return CodeText }

// Go returns the text as a Go string.
func (t Text) Go() string { return t.s }

// Len returns the length in code points.
func (t Text) Len() int { return utf8.RuneCountInString(t.s) }

func (t Text) String() string { return QuoteText(t.s) }

// QuoteText returns s as a Rhizome text literal.
func QuoteText(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if unicode.IsPrint(r) || r == ' ' {
				b.WriteRune(r)
			} else {
				fmt.Fprintf(&b, `\u{%X}`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// --- bytes ---

// Bytes is an arbitrary byte string with an optional media type. The media
// type is metadata only: it isn't validated and doesn't affect equality.
type Bytes struct {
	b         []byte
	mediaType string
}

// NewBytes returns a Bytes holding a copy of b.
func NewBytes(b []byte, mediaType string) (Bytes, error) {
	if len(b) > MaxBytesLen {
		return Bytes{}, fmt.Errorf("bytes longer than %d", MaxBytesLen)
	}
	return Bytes{append([]byte(nil), b...), mediaType}, nil
}

// BytesOf is NewBytes without the copy, for callers that hand over b:
// it must not change afterwards. It panics past MaxBytesLen.
func BytesOf(b []byte, mediaType string) Bytes {
	if len(b) > MaxBytesLen {
		panic(fmt.Sprintf("value: bytes longer than %d", MaxBytesLen))
	}
	return Bytes{b, mediaType}
}

func (Bytes) Code() Code { return CodeBytes }

// Go returns a copy of the bytes.
func (b Bytes) Go() []byte { return append([]byte(nil), b.b...) }

// Len returns the length in bytes.
func (b Bytes) Len() int { return len(b.b) }

// MediaType returns the media type, or "" if none was given.
func (b Bytes) MediaType() string { return b.mediaType }

func (b Bytes) String() string { return `x"` + hex.EncodeToString(b.b) + `"` }

// --- time ---

// Time range limits (years 0001–9999, spec/types.md §4.8).
var (
	minTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
)

// Time is an instant, stored in UTC with nanosecond precision.
type Time struct{ t time.Time }

// NewTime returns t as a Time, converted to UTC. It fails outside years
// 0001–9999.
func NewTime(t time.Time) (Time, error) {
	t = t.Round(0).UTC() // drop any monotonic clock reading
	if t.Before(minTime) || t.After(maxTime) {
		return Time{}, fmt.Errorf("time %s outside years 0001-9999", t.Format(time.RFC3339Nano))
	}
	return Time{t}, nil
}

// ParseTime parses an RFC 3339 timestamp.
func ParseTime(s string) (Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return Time{}, fmt.Errorf("invalid time %q: want RFC 3339, for example 2026-11-21T14:30:00Z", s)
	}
	return NewTime(t)
}

func (Time) Code() Code { return CodeTime }

// Go returns the instant as a time.Time in UTC.
func (t Time) Go() time.Time { return t.t }

func (t Time) String() string { return `t"` + t.t.Format(time.RFC3339Nano) + `"` }

// --- date ---

// Date is a calendar date with no time or time zone, years 0001–9999.
type Date struct{ days int32 } // days since 1970-01-01

// NewDate returns the date year-month-day. It fails for dates that don't
// exist or fall outside years 0001–9999.
func NewDate(year int, month time.Month, day int) (Date, error) {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || t.Month() != month || t.Day() != day {
		return Date{}, fmt.Errorf("invalid date %04d-%02d-%02d", year, int(month), day)
	}
	if year < 1 || year > 9999 {
		return Date{}, fmt.Errorf("date year %d outside 0001-9999", year)
	}
	return Date{int32(floorDiv(t.Unix(), 86400))}, nil
}

// ParseDate parses a date written YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q: want YYYY-MM-DD", s)
	}
	return NewDate(t.Year(), t.Month(), t.Day())
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func (Date) Code() Code { return CodeDate }

// Go returns the date as a time.Time at midnight UTC.
func (d Date) Go() time.Time { return time.Unix(int64(d.days)*86400, 0).UTC() }

// Parts returns the year, month and day.
func (d Date) Parts() (int, time.Month, int) {
	t := d.Go()
	return t.Year(), t.Month(), t.Day()
}

func (d Date) String() string { return `d"` + d.Go().Format("2006-01-02") + `"` }

// --- duration ---

// Duration is a signed length of time in nanoseconds.
type Duration time.Duration

func (Duration) Code() Code { return CodeDuration }

// Go returns the value as a time.Duration.
func (d Duration) Go() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// --- ref ---

// Ref points to an entity or field by its ID. A ref is a value, not a
// relation (spec/types.md §4.12).
type Ref struct{ Target ID }

func (Ref) Code() Code       { return CodeRef }
func (r Ref) String() string { return r.Target.String() }
