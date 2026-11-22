package value

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// group returns the sort group of a type family (spec/types.md §11.2).
// Values in a lower group sort before values in a higher group.
func group(c Code) int {
	switch c {
	case CodeNull:
		return 1
	case CodeBool:
		return 2
	case CodeInt, CodeBigInt, CodeFloat, CodeDecimal:
		return 3
	case CodeText:
		return 4
	case CodeBytes:
		return 5
	case CodeDate:
		return 6
	case CodeTime:
		return 7
	case CodeDuration:
		return 8
	case CodeID:
		return 9
	case CodeRef:
		return 10
	case CodeDoc:
		return 11
	case CodeImage:
		return 12
	case CodePair, CodeArray, CodeList:
		return 13
	case CodeRecord:
		return 14
	case CodeSet:
		return 15
	case CodeMap:
		return 16
	case CodeFifo, CodeLifo, CodeRing, CodePriority:
		return 17
	}
	panic(fmt.Sprintf("value: no sort group for %s", c))
}

// SameGroup reports whether a and b are in the same comparison group, which
// is what Rhizome requires for <, >, <= and >= (spec/types.md §11.2).
func SameGroup(a, b Value) bool { return group(code(a)) == group(code(b)) }

func code(v Value) Code {
	if v == nil {
		return CodeNull
	}
	return v.Code()
}

// Equal reports whether a and b are equal. Numbers are equal when their
// values are equal regardless of type (1 = 1.0 = 1.00d), NaN equals NaN,
// and null equals null.
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// Compare returns -1, 0 or 1 as a sorts before, equal to or after b in the
// total order over all values (spec/types.md §11). A nil Value is treated
// as null.
func Compare(a, b Value) int {
	ca, cb := code(a), code(b)
	if ga, gb := group(ca), group(cb); ga != gb {
		return cmpInt(ga, gb)
	}
	switch group(ca) {
	case 1: // null
		return 0
	case 2:
		return cmpBool(bool(a.(Bool)), bool(b.(Bool)))
	case 3:
		return compareNumbers(a, b)
	case 4:
		return strings.Compare(a.(Text).s, b.(Text).s)
	case 5:
		return bytes.Compare(a.(Bytes).b, b.(Bytes).b)
	case 6:
		return cmpInt(int(a.(Date).days), int(b.(Date).days))
	case 7:
		return a.(Time).t.Compare(b.(Time).t)
	case 8:
		return cmpInt64(int64(a.(Duration)), int64(b.(Duration)))
	case 9:
		x, y := a.(ID), b.(ID)
		return bytes.Compare(x[:], y[:])
	case 10:
		x, y := a.(Ref).Target, b.(Ref).Target
		return bytes.Compare(x[:], y[:])
	}
	return compareComposite(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// compareNumbers compares two numeric values of any numeric types by their
// exact mathematical value. Integers are never converted to floats, so
// 2^53+1 is correctly greater than the float 2^53. NaN is the largest
// number and equals itself.
func compareNumbers(a, b Value) int {
	// Fast paths for the common same-type cases.
	switch x := a.(type) {
	case Int:
		switch y := b.(type) {
		case Int:
			return cmpInt64(int64(x), int64(y))
		case Float:
			return -compareFloatInt(y.f, int64(x))
		}
	case Float:
		switch y := b.(type) {
		case Float:
			return compareFloats(x.f, y.f)
		case Int:
			return compareFloatInt(x.f, int64(y))
		}
	}

	// Handle NaN and infinities before converting to exact rationals.
	fa, fb := floatClass(a), floatClass(b)
	if fa != 0 || fb != 0 {
		return cmpInt(fa, fb)
	}
	return exactRat(a).Cmp(exactRat(b))
}

// floatClass returns 0 for finite numbers (including all non-floats), -1
// for -Inf, 1 for +Inf and 2 for NaN, which is enough to order any value
// with a non-finite float.
func floatClass(v Value) int {
	f, ok := v.(Float)
	if !ok {
		return 0
	}
	switch {
	case math.IsNaN(f.f):
		return 2
	case math.IsInf(f.f, 1):
		return 1
	case math.IsInf(f.f, -1):
		return -1
	}
	return 0
}

func compareFloats(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareFloatInt compares a float with an int exactly.
func compareFloatInt(f float64, i int64) int {
	if math.IsNaN(f) {
		return 1
	}
	// Integers of magnitude up to 2^53 convert to float64 exactly.
	if i >= -(1<<53) && i <= 1<<53 {
		return compareFloats(f, float64(i))
	}
	if math.IsInf(f, 0) {
		return int(math.Copysign(1, f))
	}
	return new(big.Rat).SetFloat64(f).Cmp(new(big.Rat).SetInt64(i))
}

// exactRat returns the exact value of a finite numeric value.
func exactRat(v Value) *big.Rat {
	switch x := v.(type) {
	case Int:
		return new(big.Rat).SetInt64(int64(x))
	case BigInt:
		return new(big.Rat).SetInt(x.big())
	case Float:
		return new(big.Rat).SetFloat64(x.f)
	case Decimal:
		return x.Rat()
	}
	panic(fmt.Sprintf("value: %s is not a number", v.Code()))
}

// compareComposite orders values in the composite groups (docs, images,
// collections, records and queues). It is defined with those types.
var compareComposite = func(a, b Value) int {
	panic(fmt.Sprintf("value: comparing %s values is not supported yet", a.Code()))
}
