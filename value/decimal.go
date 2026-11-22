package value

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal is an exact base-10 number: an integer coefficient and a scale
// (the number of digits after the decimal point), so its value is
// coef × 10^-scale. The scale is kept for display; equality and ordering
// ignore trailing zeros, so 1.50 equals 1.5 (spec/types.md §4.5).
type Decimal struct {
	coef  *big.Int
	scale int32
}

var bigTen = big.NewInt(10)

func pow10(n int) *big.Int {
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// NewDecimal returns coef × 10^-scale. scale must be between 0 and
// MaxDecimalDigits, and the coefficient at most MaxDecimalDigits digits.
func NewDecimal(coef *big.Int, scale int) (Decimal, error) {
	if scale < 0 || scale > MaxDecimalDigits {
		return Decimal{}, fmt.Errorf("decimal scale %d out of range 0..%d", scale, MaxDecimalDigits)
	}
	d := Decimal{new(big.Int).Set(coef), int32(scale)}
	if n := d.coefDigits(); n > MaxDecimalDigits {
		return Decimal{}, fmt.Errorf("decimal with %d digits exceeds %d", n, MaxDecimalDigits)
	}
	return d, nil
}

// DecimalFromInt returns i as a Decimal with scale 0.
func DecimalFromInt(i int64) Decimal { return Decimal{big.NewInt(i), 0} }

// ParseDecimal parses a decimal number such as "-12.50" (without the "d"
// suffix). Exponents aren't accepted.
func ParseDecimal(s string) (Decimal, error) {
	orig := s
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, frac, hasPoint := strings.Cut(s, ".")
	if intPart == "" || (hasPoint && frac == "") || !allDigits(intPart) || !allDigits(frac) {
		return Decimal{}, fmt.Errorf("invalid decimal %q", orig)
	}
	coef, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal %q", orig)
	}
	if neg {
		coef.Neg(coef)
	}
	return NewDecimal(coef, len(frac))
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (Decimal) Code() Code { return CodeDecimal }

func (d Decimal) c() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

// Scale returns the number of digits after the decimal point.
func (d Decimal) Scale() int { return int(d.scale) }

// Coefficient returns a copy of the integer coefficient.
func (d Decimal) Coefficient() *big.Int { return new(big.Int).Set(d.c()) }

// Sign returns -1, 0 or 1.
func (d Decimal) Sign() int { return d.c().Sign() }

// Rat returns the exact value as a *big.Rat.
func (d Decimal) Rat() *big.Rat {
	return new(big.Rat).SetFrac(d.c(), pow10(int(d.scale)))
}

func (d Decimal) coefDigits() int {
	c := d.c()
	if c.Sign() == 0 {
		return 1
	}
	return len(new(big.Int).Abs(c).String())
}

// IntDigits returns the number of digits before the decimal point, ignoring
// leading zeros (0 for values below 1 in magnitude).
func (d Decimal) IntDigits() int {
	q := new(big.Int).Quo(new(big.Int).Abs(d.c()), pow10(int(d.scale)))
	if q.Sign() == 0 {
		return 0
	}
	return len(q.String())
}

// Round returns d rounded to scale digits after the point, using round
// half to even. Rounding to a larger scale just adds trailing zeros.
func (d Decimal) Round(scale int) Decimal {
	if scale >= int(d.scale) {
		coef := new(big.Int).Mul(d.c(), pow10(scale-int(d.scale)))
		return Decimal{coef, int32(scale)}
	}
	div := pow10(int(d.scale) - scale)
	q, r := new(big.Int).QuoRem(d.c(), div, new(big.Int))
	// Compare 2|r| with div to decide the direction.
	twice := new(big.Int).Abs(r)
	twice.Lsh(twice, 1)
	switch cmp := twice.Cmp(div); {
	case cmp > 0, cmp == 0 && q.Bit(0) == 1:
		if d.c().Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return Decimal{q, int32(scale)}
}

// Fit rounds d to the given scale and checks it fits decimal(precision,
// scale): at most precision-scale digits before the point.
func (d Decimal) Fit(precision, scale int) (Decimal, error) {
	r := d.Round(scale)
	if r.IntDigits() > precision-scale {
		return Decimal{}, fmt.Errorf("%s does not fit decimal(%d,%d)", d, precision, scale)
	}
	return r, nil
}

// Cmp compares d and e by value: -1, 0 or 1.
func (d Decimal) Cmp(e Decimal) int {
	a, b := d.c(), e.c()
	switch {
	case d.scale < e.scale:
		a = new(big.Int).Mul(a, pow10(int(e.scale-d.scale)))
	case d.scale > e.scale:
		b = new(big.Int).Mul(b, pow10(int(d.scale-e.scale)))
	}
	return a.Cmp(b)
}

// String returns the Rhizome literal, for example "12.50d".
func (d Decimal) String() string {
	return d.Plain() + "d"
}

// Plain returns the number without the "d" suffix, for example "12.50".
func (d Decimal) Plain() string {
	c := d.c()
	s := new(big.Int).Abs(c).String()
	if d.scale > 0 {
		if len(s) <= int(d.scale) {
			s = strings.Repeat("0", int(d.scale)-len(s)+1) + s
		}
		s = s[:len(s)-int(d.scale)] + "." + s[len(s)-int(d.scale):]
	}
	if c.Sign() < 0 {
		s = "-" + s
	}
	return s
}
