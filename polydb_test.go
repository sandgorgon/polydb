package polydb

import (
	"context"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/value"
)

func TestToValue(t *testing.T) {
	tm := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	big1, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	var nilPtr *int
	five := 5
	for _, c := range []struct {
		in   any
		want string
	}{
		{nil, "null"}, {true, "true"}, {42, "42"}, {int8(-3), "-3"}, {uint64(math.MaxUint64), "18446744073709551615n"},
		{1.5, "1.5"}, {"hi", `"hi"`}, {[]byte{1, 2}, `x"0102"`}, {tm, `t"2026-11-22T12:00:00Z"`},
		{time.Second, "1s"}, {big1, "123456789012345678901234567890n"}, {value.Int(7), "7"},
		{[]int{1, 2}, "[1, 2]"}, {map[string]any{"a": 1}, `{"a": 1}`}, {nilPtr, "null"}, {&five, "5"},
	} {
		v, err := ToValue(c.in)
		if err != nil || v.String() != c.want {
			t.Errorf("ToValue(%#v) = %v, %v; want %s", c.in, v, err, c.want)
		}
	}
	if _, err := ToValue(struct{}{}); err == nil {
		t.Error("struct accepted")
	}
	if _, err := ParamValues(Params{"x": make(chan int)}); err == nil || !strings.Contains(err.Error(), "$x") {
		t.Errorf("bad param: %v", err)
	}
}

func TestScan(t *testing.T) {
	ctx := context.Background()
	d, _ := value.ParseDate("2026-01-02")
	id := value.ID{1}
	cells := []any{value.MustText("a"), value.Int(3), value.NewFloat(2.5), value.Bool(true), mustBytes("xy"),
		d, value.Duration(time.Minute), value.Ref{Target: id}, value.Null, value.Int(4)}
	var (
		s   string
		i   int
		f   float64
		b   bool
		bs  []byte
		tt  time.Time
		dur time.Duration
		ref value.ID
		np  *string
		ip  *int64
	)
	if err := Scan(ctx, cells, &s, &i, &f, &b, &bs, &tt, &dur, &ref, &np, &ip); err != nil {
		t.Fatal(err)
	}
	if s != "a" || i != 3 || f != 2.5 || !b || string(bs) != "xy" || tt.Day() != 2 || dur != time.Minute ||
		ref != id || np != nil || ip == nil || *ip != 4 {
		t.Errorf("scanned %v %v %v %v %v %v %v %v %v %v", s, i, f, b, bs, tt, dur, ref, np, ip)
	}
	if err := Scan(ctx, cells[:1], &i); err == nil {
		t.Error("text scanned into int")
	}
	if err := Scan(ctx, cells[:1]); err == nil {
		t.Error("wrong count accepted")
	}
	// Small values scan into **Blob too.
	var blob *Blob
	if err := Scan(ctx, []any{value.MustText("hello")}, &blob); err != nil || blob.Length != 5 {
		t.Fatalf("inline blob: %v %v", blob, err)
	}
	p := make([]byte, 3)
	if n, _ := blob.ReadAt(p, 2); n != 3 || string(p) != "llo" {
		t.Errorf("ReadAt %q", p)
	}
	if v, err := blob.Value(ctx); err != nil || v.String() != `"hello"` {
		t.Errorf("blob value %v %v", v, err)
	}
}

func mustBytes(s string) value.Bytes {
	b, _ := value.NewBytes([]byte(s), "")
	return b
}
