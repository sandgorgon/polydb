package value

import "testing"

func TestDecimalArith(t *testing.T) {
	d := func(s string) Decimal {
		t.Helper()
		v, err := ParseDecimal(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		op   string
		a, b string
		want string
	}{
		{"+", "1.5", "2.25", "3.75"},
		{"-", "1.5", "2.25", "-0.75"},
		{"*", "1.5", "-2.25", "-3.375"},
		{"/", "10.00", "4", "2.50"},
		{"/", "10", "4", "2.5"},
		{"/", "1", "3", "0.3333333333333333"},
		{"/", "2", "3", "0.6666666666666667"},
		{"/", "-2", "3", "-0.6666666666666667"},
		{"/", "1", "-8", "-0.125"},
		{"/", "1", "32", "0.03125"},
		{"/", "0.0000000000000001", "4", "0.0000000000000000"}, // 2.5e-17: half to even rounds to 0
		{"/", "0.0000000000000003", "2", "0.0000000000000002"}, // 1.5e-16 → 2e-16 (even)
		{"/", "0.0000000000000005", "2", "0.0000000000000002"}, // 2.5e-16 → 2e-16 (even)
		{"%", "7.5", "2", "1.5"},
		{"%", "-7.5", "2", "-1.5"},
	}
	for _, c := range cases {
		a, b := d(c.a), d(c.b)
		var r Decimal
		var err error
		switch c.op {
		case "+":
			r, err = a.Add(b)
		case "-":
			r, err = a.Sub(b)
		case "*":
			r, err = a.Mul(b)
		case "/":
			r, err = a.Quo(b)
		case "%":
			r, err = a.Rem(b)
		}
		if err != nil || r.Plain() != c.want {
			t.Errorf("%s %s %s = %s, %v; want %s", c.a, c.op, c.b, r.Plain(), err, c.want)
		}
	}
	if _, err := d("1").Quo(d("0")); err != ErrDivZero {
		t.Errorf("1/0: %v", err)
	}
	for in, want := range map[string][2]string{"1.5": {"1", "2"}, "-1.5": {"-2", "-1"}, "2": {"2", "2"}, "-0.1": {"-1", "0"}} {
		if f, c := d(in).Floor().Plain(), d(in).Ceil().Plain(); f != want[0] || c != want[1] {
			t.Errorf("floor/ceil(%s) = %s, %s; want %v", in, f, c, want)
		}
	}
}
