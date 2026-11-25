package value

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestJSONRoundTrip(t *testing.T) {
	jsonFmt, _ := LookupFormat("json")
	doc := must(NewDoc(jsonFmt, `{"a": [1, 2]}`))
	id := must(ParseID("01M86ZCATD000000BCAYRXKMMD"))
	fifo := must(NewQueueOf(&Type{Code: CodeFifo, N: 10}, []Value{Int(1), MustText("x")}, nil))
	ring := must(NewQueueOf(&Type{Code: CodeRing, N: 3}, []Value{Int(1)}, nil))
	prio := must(NewQueueOf(&Type{Code: CodePriority, LowFirst: true}, []Value{MustText("a"), MustText("b")}, []int64{5, -2}))
	vals := []Value{
		Null, Bool(true), Int(0), Int(-1 << 63), Int(1<<63 - 1), MustText(""), MustText("héllo \"\n <"),
		NewFloat(1.5), NewFloat(1), NewFloat(-0.1), NewFloat(math.NaN()), NewFloat(math.Inf(-1)), NewFloat(1e300),
		must(NewBigInt(new(big.Int).Lsh(big.NewInt(1), 100))), must(ParseDecimal("-19.990")),
		must(NewBytes([]byte{0, 1, 255}, "")), must(NewBytes([]byte("<svg/>"), "image/svg+xml")),
		must(NewTime(time.Date(2026, 11, 24, 10, 30, 0, 123456789, time.UTC))), must(ParseDate("0001-01-01")),
		Duration(90*time.Minute + 5), id, Ref{Target: id}, doc,
		must(NewList(Int(1), MustText("a"), Null)), must(NewArray(Int(1), Int(2))), must(NewSet(Int(3), Int(1))),
		must(NewPair(MustText("k"), Int(1))),
		must(NewMap([]Value{MustText("b"), MustText("a")}, []Value{Int(1), must(NewList())})),
		must(NewMap([]Value{MustText("$id"), Int(7)}, []Value{Int(1), Int(2)})),
		must(NewMap(nil, nil)),
		must(NewRecord([]string{"z", "a"}, []Value{Int(1), Int(2)})),
		fifo, ring, prio,
		BlobRef{Of: CodeBytes, Hash: [32]byte{1, 2}, Len: 99, Media: "video/mp4"},
	}
	for _, v := range vals {
		js, err := AppendJSON(nil, v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		got, err := ParseJSON(js, LookupFormat)
		if err != nil {
			t.Fatalf("%s: parse %s: %v", v, js, err)
		}
		if Compare(got, v) != 0 || got.Code() != v.Code() || got.String() != v.String() {
			t.Errorf("%s: %s gave back %s", v, js, got)
		}
		if b, ok := v.(Bytes); ok && got.(Bytes).MediaType() != b.MediaType() {
			t.Errorf("%s: media type lost", js)
		}
		if r, ok := got.(Record); ok && strings.Join(r.Names(), ",") != "z,a" {
			t.Errorf("record field order: %v", r.Names())
		}
	}
	// Plain JSON from elsewhere.
	for src, want := range map[string]string{
		`{"n": 1.0, "f": 2.5, "s": "x", "l": [true, null]}`: `{"f": 2.5, "l": [true, null], "n": 1, "s": "x"}`,
		`3e2`: `300`,
	} {
		v, err := ParseJSON([]byte(src), nil)
		if err != nil || v.String() != want {
			t.Errorf("%s: %v %v", src, v, err)
		}
	}
	for _, bad := range []string{`{"$nope": 1}`, `{"$id": "x"}`, `{"$bytes": "AA", "extra": 1}`, `{"$a": 1, "$b": 2}`,
		`99999999999999999999`, `{"$doc": "{}", "format": "nope"}`, `[1] 2`} {
		if _, err := ParseJSON([]byte(bad), LookupFormat); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
