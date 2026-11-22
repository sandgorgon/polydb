package value

import (
	"errors"
	"math/big"
)

// Decimal arithmetic. Results are exact except for Quo, and fail when they
// would exceed MaxDecimalDigits.

// ErrDivZero is returned for division by zero.
var ErrDivZero = errors.New("division by zero")

// QuoScale is the minimum number of digits after the point that Quo
// keeps for results that don't terminate, such as 1/3.
const QuoScale = 16

// align returns the coefficients of d and e at their common scale.
func align(d, e Decimal) (a, b *big.Int, scale int) {
	a, b = d.c(), e.c()
	switch {
	case d.scale < e.scale:
		a = new(big.Int).Mul(a, pow10(int(e.scale-d.scale)))
		return a, b, int(e.scale)
	case d.scale > e.scale:
		b = new(big.Int).Mul(b, pow10(int(d.scale-e.scale)))
	}
	return a, b, int(d.scale)
}

// Add returns d + e.
func (d Decimal) Add(e Decimal) (Decimal, error) {
	a, b, s := align(d, e)
	return NewDecimal(new(big.Int).Add(a, b), s)
}

// Sub returns d - e.
func (d Decimal) Sub(e Decimal) (Decimal, error) {
	a, b, s := align(d, e)
	return NewDecimal(new(big.Int).Sub(a, b), s)
}

// Mul returns d × e.
func (d Decimal) Mul(e Decimal) (Decimal, error) {
	return NewDecimal(new(big.Int).Mul(d.c(), e.c()), int(d.scale+e.scale))
}

// Quo returns d ÷ e. Results that terminate are exact; others are rounded
// half to even at max(QuoScale, the operands' scales) digits. Trailing
// zeros beyond the larger operand scale are dropped, so 10.00/4 is 2.50.
func (d Decimal) Quo(e Decimal) (Decimal, error) {
	den := e.c()
	if den.Sign() == 0 {
		return Decimal{}, ErrDivZero
	}
	keep := max(int(d.scale), int(e.scale))
	scale := max(QuoScale, keep)
	// d/e at scale: d.coef × 10^(scale - d.scale + e.scale) / e.coef.
	num := new(big.Int).Mul(d.c(), pow10(scale-int(d.scale)+int(e.scale)))
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half to even, away from zero in the result's direction.
	twice := new(big.Int).Abs(r)
	twice.Lsh(twice, 1)
	switch c := twice.Cmp(new(big.Int).Abs(den)); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		if num.Sign()*den.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	res, err := NewDecimal(q, scale)
	if err != nil {
		return Decimal{}, err
	}
	return res.trim(keep), nil
}

// Rem returns the remainder of d ÷ e truncated toward zero; it has the
// sign of d.
func (d Decimal) Rem(e Decimal) (Decimal, error) {
	if e.c().Sign() == 0 {
		return Decimal{}, ErrDivZero
	}
	a, b, s := align(d, e)
	return NewDecimal(new(big.Int).Rem(a, b), s)
}

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{new(big.Int).Neg(d.c()), d.scale} }

// Abs returns |d|.
func (d Decimal) Abs() Decimal { return Decimal{new(big.Int).Abs(d.c()), d.scale} }

// Floor returns the largest integer not above d, with scale 0.
func (d Decimal) Floor() Decimal {
	// Div is Euclidean division, which is the floor for a positive
	// divisor.
	return Decimal{new(big.Int).Div(d.c(), pow10(int(d.scale))), 0}
}

// Ceil returns the smallest integer not below d, with scale 0.
func (d Decimal) Ceil() Decimal { return d.Neg().Floor().Neg() }

// trim drops trailing zeros after the point, keeping at least keep
// digits.
func (d Decimal) trim(keep int) Decimal {
	c := new(big.Int).Set(d.c())
	s := int(d.scale)
	r := new(big.Int)
	for s > keep {
		q, rem := new(big.Int).QuoRem(c, bigTen, r)
		if rem.Sign() != 0 {
			break
		}
		c, s = q, s-1
	}
	return Decimal{c, int32(s)}
}
