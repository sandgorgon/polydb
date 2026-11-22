package value

import (
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTextNormalizesToNFC(t *testing.T) {
	composed := MustText("café")
	decomposed := MustText("café")
	if !Equal(composed, decomposed) {
		t.Fatal("NFC forms should be equal")
	}
	if decomposed.Go() != "café" {
		t.Errorf("not normalized: %q", decomposed.Go())
	}
	if decomposed.Len() != 4 {
		t.Errorf("Len = %d, want 4", decomposed.Len())
	}
	if _, err := NewText("bad \xff"); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
}

func TestTextLiteral(t *testing.T) {
	got := MustText("a\"b\\c\n\x01é").String()
	want := `"a\"b\\c\n\u{1}é"`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestFloatNormalization(t *testing.T) {
	if s := NewFloat(math.Copysign(0, -1)).String(); s != "0.0" {
		t.Errorf("-0 = %s", s)
	}
	if !Equal(NewFloat(math.NaN()), NewFloat(math.NaN())) {
		t.Error("NaN should equal NaN")
	}
	for f, want := range map[float64]string{2: "2.0", 2.5: "2.5", 1e21: "1e+21", math.Inf(1): "inf", math.Inf(-1): "-inf"} {
		if s := NewFloat(f).String(); s != want {
			t.Errorf("%v -> %s, want %s", f, s, want)
		}
	}
}

func TestDecimal(t *testing.T) {
	d, err := ParseDecimal("-12.50")
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != "-12.50d" || d.Scale() != 2 {
		t.Errorf("got %s scale %d", d, d.Scale())
	}
	if !Equal(must(ParseDecimal("1.50")), must(ParseDecimal("1.5"))) {
		t.Error("1.50 should equal 1.5")
	}
	if s := must(ParseDecimal("0.05")).String(); s != "0.05d" {
		t.Errorf("0.05 -> %s", s)
	}
	for _, bad := range []string{"", "-", "1.", ".5", "1e5", "1.2.3", "abc"} {
		if _, err := ParseDecimal(bad); err == nil {
			t.Errorf("ParseDecimal(%q) accepted", bad)
		}
	}
}

func TestDecimalRoundHalfEven(t *testing.T) {
	cases := map[string]string{
		"2.345": "2.34", "2.355": "2.36", "2.3451": "2.35",
		"-2.345": "-2.34", "-2.355": "-2.36", "0.005": "0.00", "0.015": "0.02",
		"1.5": "1.50", "-0.004": "0.00",
	}
	for in, want := range cases {
		if got := must(ParseDecimal(in)).Round(2).Plain(); got != want {
			t.Errorf("Round(%s, 2) = %s, want %s", in, got, want)
		}
	}
	if got := must(ParseDecimal("2.5")).Round(0).Plain(); got != "2" {
		t.Errorf("2.5 -> %s", got)
	}
	if got := must(ParseDecimal("3.5")).Round(0).Plain(); got != "4" {
		t.Errorf("3.5 -> %s", got)
	}
}

func TestDecimalFit(t *testing.T) {
	if r, err := must(ParseDecimal("999.994")).Fit(5, 2); err != nil || r.Plain() != "999.99" {
		t.Errorf("Fit = %v, %v", r, err)
	}
	if _, err := must(ParseDecimal("999.995")).Fit(5, 2); err == nil {
		t.Error("999.995 rounds to 1000.00, which shouldn't fit decimal(5,2)")
	}
	if r, err := must(ParseDecimal("0.05")).Fit(2, 2); err != nil || r.Plain() != "0.05" {
		t.Errorf("0.05 in decimal(2,2): %v, %v", r, err)
	}
}

func TestIDBase32RoundTrip(t *testing.T) {
	g := NewIDGenerator(7)
	for i := 0; i < 1000; i++ {
		id := g.New()
		s := id.Base32()
		if len(s) != 26 {
			t.Fatalf("len %d", len(s))
		}
		back, err := ParseID("#" + strings.ToLower(s))
		if err != nil || back != id {
			t.Fatalf("round trip %s: %v", s, err)
		}
		if id.Node() != 7 {
			t.Fatalf("node %d", id.Node())
		}
	}
	var max ID
	for i := range max {
		max[i] = 0xFF
	}
	if back, err := ParseID(max.Base32()); err != nil || back != max {
		t.Errorf("max ID round trip: %v", err)
	}
	if _, err := ParseID("8ZZZZZZZZZZZZZZZZZZZZZZZZZ"); err == nil {
		t.Error("overflowing ID accepted")
	}
	if _, err := ParseID("01JD8X3K7M0000Q2W5ZB9N4T6U"); err == nil {
		t.Error("U is not a Crockford character")
	}
}

func TestIDsIncrease(t *testing.T) {
	g := NewIDGenerator(0)
	fixed := time.UnixMilli(1_700_000_000_000)
	g.now = func() time.Time { return fixed }
	var ids []ID
	for i := 0; i < 70_000; i++ { // more than the per-millisecond counter holds
		ids = append(ids, g.New())
	}
	g.now = func() time.Time { return fixed.Add(-time.Second) } // clock goes back
	ids = append(ids, g.New())
	for i := 1; i < len(ids); i++ {
		if Compare(ids[i-1], ids[i]) >= 0 {
			t.Fatalf("ids[%d] >= ids[%d]", i-1, i)
		}
	}
	if !ids[0].Time().Equal(fixed) {
		t.Errorf("Time() = %v", ids[0].Time())
	}
}

func TestDateAndTime(t *testing.T) {
	d := must(ParseDate("2026-11-21"))
	if y, m, day := d.Parts(); y != 2026 || m != time.November || day != 21 {
		t.Errorf("parts %d %d %d", y, m, day)
	}
	if d.String() != `d"2026-11-21"` {
		t.Errorf("String = %s", d)
	}
	early := must(NewDate(1, 1, 1))
	if Compare(early, d) >= 0 {
		t.Error("year 1 should sort first")
	}
	for _, bad := range []string{"2026-02-30", "10000-01-01", "2026-1-1"} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("ParseDate(%q) accepted", bad)
		}
	}

	tm := must(ParseTime("2026-11-21T14:30:00.5+02:00"))
	if tm.String() != `t"2026-11-21T12:30:00.5Z"` {
		t.Errorf("String = %s", tm)
	}
	if _, err := NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Error("year 10000 accepted")
	}
}

func TestNumericComparison(t *testing.T) {
	big1 := must(NewBigInt(big.NewInt(1)))
	one := []Value{Int(1), big1, NewFloat(1), must(ParseDecimal("1.00"))}
	for _, a := range one {
		for _, b := range one {
			if !Equal(a, b) {
				t.Errorf("%s != %s", a, b)
			}
		}
	}
	// 2^53+1 can't be represented as a float64; it must still compare
	// greater than the float 2^53.
	if Compare(Int(1<<53+1), NewFloat(1<<53)) != 1 {
		t.Error("2^53+1 should be greater than float 2^53")
	}
	if Compare(NewFloat(1<<53), Int(1<<53+1)) != -1 {
		t.Error("float 2^53 should be less than 2^53+1")
	}
	huge := must(ParseBigInt("100000000000000000000000000000"))
	if Compare(huge, NewFloat(math.Inf(1))) != -1 || Compare(NewFloat(math.NaN()), huge) != 1 {
		t.Error("infinity and NaN ordering wrong")
	}
	if Compare(must(ParseDecimal("0.1")), NewFloat(0.1)) != -1 {
		// float 0.1 is slightly more than one tenth
		t.Error("decimal 0.1 should be less than float 0.1")
	}
	if Compare(Int(math.MinInt64), NewFloat(math.Inf(-1))) != 1 {
		t.Error("-inf should be smallest")
	}
}

func TestCrossTypeOrder(t *testing.T) {
	vals := []Value{
		Ref{Target: ID{1}}, ID{1}, Duration(5), must(ParseTime("2026-01-01T00:00:00Z")),
		must(ParseDate("2026-01-01")), must(NewBytes([]byte{1}, "")), MustText("a"),
		Int(-5), Bool(true), Bool(false), Null,
	}
	sorted := slices.Clone(vals)
	slices.SortFunc(sorted, Compare)
	slices.Reverse(vals)
	for i := range vals {
		if !Equal(vals[i], sorted[i]) || vals[i].Code() != sorted[i].Code() {
			t.Fatalf("position %d: got %s, want %s", i, sorted[i], vals[i])
		}
	}
	if !SameGroup(Int(1), NewFloat(2)) || SameGroup(Int(1), MustText("1")) {
		t.Error("SameGroup wrong")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
