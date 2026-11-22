package value

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Key encoding (spec/types.md §12.2). For keyable values a and b,
//
//	bytes.Compare(Key(a), Key(b)) == Compare(a, b)
//
// so the storage engine can sort and range-scan without decoding. Equal
// values encode identically, even across numeric types (1, 1.0, 1n and
// 1.00d share one key), so a key can't be decoded back to its exact type;
// indexes point to the stored value for that.
//
// Every key starts with its sort group number (1-17, see group) and is
// self-delimiting: no key is a prefix of a different key. That lets
// composite keys be built by plain concatenation.
//
// Layouts after the group byte:
//
//	null                 nothing
//	bool                 0 or 1
//	number               class byte: 1 -inf, 2 negative, 3 zero, 4 positive,
//	                     5 +inf, 6 NaN. A finite nonzero number is written
//	                     exactly as 0.d1d2…dn × 10^E (d1 ≠ 0, no trailing
//	                     zeros): E as 4 bytes (sign bit flipped), then the
//	                     digits in pairs, each pair p as 2p+1, or 2p for the
//	                     last pair (an odd count is padded with 0). For
//	                     negative numbers E and the digit bytes are inverted.
//	text, bytes          the bytes with 0x00 escaped as 0x00 0xFF, then 0x00 0x01
//	date                 4 bytes, sign bit flipped
//	time                 8 bytes seconds (sign bit flipped), 4 bytes nanoseconds
//	duration             8 bytes, sign bit flipped
//	id, ref              16 bytes
//	pair, array          the elements' keys, 0x00, then 0 (pair) or 1 (array)
//	record               for each field: 0x01, the name (escaped as text), the
//	                     value's key; then 0x00
//
// Floats have finite decimal expansions, so this is exact: the float 0.1
// sorts just above the decimal 0.1d. Keys for floats with large or tiny
// exponents can be long (up to about 770 digits for subnormals).

// Key returns the key encoding of v, or an error if v isn't keyable.
func Key(v Value) ([]byte, error) { return AppendKey(nil, v) }

// AppendKey appends the key encoding of v to dst.
func AppendKey(dst []byte, v Value) ([]byte, error) {
	if !IsKeyable(v) {
		return dst, fmt.Errorf("%s value can't be used as a key", code(v))
	}
	return appendKey(dst, v), nil
}

func appendKey(dst []byte, v Value) []byte {
	c := code(v)
	dst = append(dst, byte(group(c)))
	switch x := v.(type) {
	case nil, nullValue:
		return dst
	case Bool:
		if x {
			return append(dst, 1)
		}
		return append(dst, 0)
	case Int, BigInt, Float, Decimal:
		return appendNumberKey(dst, v)
	case Text:
		return appendEscaped(dst, x.s)
	case Bytes:
		return appendEscaped(dst, string(x.b))
	case Date:
		return binary.BigEndian.AppendUint32(dst, uint32(x.days)^1<<31)
	case Time:
		dst = binary.BigEndian.AppendUint64(dst, uint64(x.t.Unix())^1<<63)
		return binary.BigEndian.AppendUint32(dst, uint32(x.t.Nanosecond()))
	case Duration:
		return binary.BigEndian.AppendUint64(dst, uint64(x)^1<<63)
	case ID:
		return append(dst, x[:]...)
	case Ref:
		return append(dst, x.Target[:]...)
	case Pair:
		dst = appendKey(appendKey(dst, x.Key()), x.Value())
		return append(dst, 0, byte(familyRank(CodePair)))
	case Array:
		for _, e := range x.elems {
			dst = appendKey(dst, e)
		}
		return append(dst, 0, byte(familyRank(CodeArray)))
	case Record:
		for i, n := range x.names {
			dst = appendKey(appendEscaped(append(dst, 1), n), x.vals[i])
		}
		return append(dst, 0)
	}
	panic(fmt.Sprintf("value: no key encoding for %s", c))
}

func appendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			dst = append(dst, 0, 0xFF)
		} else {
			dst = append(dst, s[i])
		}
	}
	return append(dst, 0, 1)
}

// Number classes, in sort order.
const (
	numNegInf = 1 + iota
	numNeg
	numZero
	numPos
	numPosInf
	numNaN
)

func appendNumberKey(dst []byte, v Value) []byte {
	if f, ok := v.(Float); ok {
		switch {
		case math.IsNaN(f.f):
			return append(dst, numNaN)
		case math.IsInf(f.f, 1):
			return append(dst, numPosInf)
		case math.IsInf(f.f, -1):
			return append(dst, numNegInf)
		}
	}
	neg, digits, exp := decimalDigits(v)
	if digits == "" {
		return append(dst, numZero)
	}
	var flip byte
	cls := byte(numPos)
	if neg {
		flip, cls = 0xFF, numNeg
	}
	dst = append(dst, cls)
	var e [4]byte
	binary.BigEndian.PutUint32(e[:], uint32(exp)^1<<31)
	for _, b := range e {
		dst = append(dst, b^flip)
	}
	for i := 0; i < len(digits); i += 2 {
		p := int(digits[i]-'0') * 10
		if i+1 < len(digits) {
			p += int(digits[i+1] - '0')
		}
		b := byte(2*p + 1)
		if i+2 >= len(digits) {
			b-- // last pair
		}
		dst = append(dst, b^flip)
	}
	return dst
}

// decimalDigits returns a finite number's exact value as sign, significant
// digits (no leading or trailing zeros; "" for zero) and exponent E, so the
// magnitude is 0.digits × 10^E.
func decimalDigits(v Value) (neg bool, digits string, exp int) {
	var mag string // magnitude as an integer digit string
	scale := 0     // value = mag × 10^-scale
	switch x := v.(type) {
	case Int:
		neg = x < 0
		mag = strconv.FormatUint(absInt64(int64(x)), 10)
	case BigInt:
		neg = x.big().Sign() < 0
		mag = new(big.Int).Abs(x.big()).String()
	case Decimal:
		neg = x.Sign() < 0
		mag = new(big.Int).Abs(x.c()).String()
		scale = int(x.scale)
	case Float:
		neg = x.f < 0
		mag, scale = floatDigits(math.Abs(x.f))
	}
	if mag == "0" {
		return false, "", 0
	}
	exp = len(mag) - scale
	return neg, strings.TrimRight(mag, "0"), exp
}

func absInt64(i int64) uint64 {
	if i < 0 {
		return uint64(-(i + 1)) + 1 // works for math.MinInt64
	}
	return uint64(i)
}

// floatDigits returns the exact value of a finite, non-negative float as
// mag × 10^-scale with mag an integer digit string.
func floatDigits(f float64) (mag string, scale int) {
	if f == 0 {
		return "0", 0
	}
	bits := math.Float64bits(f)
	m := bits & (1<<52 - 1)
	e := int(bits>>52) & 0x7FF
	if e == 0 {
		e = 1 // subnormal
	} else {
		m |= 1 << 52
	}
	e -= 1075 // f = m × 2^e
	for m&1 == 0 && e < 0 {
		m >>= 1
		e++
	}
	b := new(big.Int).SetUint64(m)
	if e >= 0 {
		return b.Lsh(b, uint(e)).String(), 0
	}
	// m × 2^e = m × 5^-e / 10^-e
	five := new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(-e)), nil)
	return b.Mul(b, five).String(), -e
}
