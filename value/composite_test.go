package value

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func list(vs ...Value) List { return must(NewList(vs...)) }
func set(vs ...Value) Set   { return must(NewSet(vs...)) }
func txt(s string) Text     { return MustText(s) }
func dec(s string) Decimal  { return must(ParseDecimal(s)) }

func TestSetSortsAndDedupes(t *testing.T) {
	s := set(Int(3), Int(1), NewFloat(1), Int(2), Int(3))
	if s.String() != "{1, 2, 3}" {
		t.Errorf("got %s", s)
	}
	// 1 and 1.0 are equal; the int (lower type code) is kept whatever the
	// input order, so encoding stays deterministic.
	if a, b := set(NewFloat(1), Int(1)), set(Int(1), NewFloat(1)); a.String() != "{1}" || b.String() != "{1}" {
		t.Errorf("got %s and %s", a, b)
	}
	if !s.Has(dec("2.0")) || s.Has(Int(4)) {
		t.Error("Has wrong")
	}
	if _, err := NewSet(list(Int(1))); err == nil {
		t.Error("list accepted as a set element")
	}
	if _, err := NewSet(must(NewPair(Int(1), txt("a")))); err != nil {
		t.Errorf("keyable pair rejected: %v", err)
	}
}

func TestMap(t *testing.T) {
	m := must(MapOfText(map[string]Value{"b": Int(2), "a": Int(1), "c d": Null}))
	if m.String() != `{"a": 1, "b": 2, "c d": null}` {
		t.Errorf("got %s", m)
	}
	if v, ok := m.Get(txt("b")); !ok || !Equal(v, Int(2)) {
		t.Errorf("Get = %v, %v", v, ok)
	}
	if _, err := NewMap([]Value{Int(1), NewFloat(1)}, []Value{Null, Null}); err == nil {
		t.Error("duplicate key accepted")
	}
	if _, err := NewMap([]Value{list()}, []Value{Null}); err == nil {
		t.Error("list key accepted")
	}
}

func TestCompositeLiterals(t *testing.T) {
	p := must(NewPair(txt("k"), Int(1)))
	nested := must(NewPair(p, p))
	cases := []struct {
		v    Value
		want string
	}{
		{list(Int(1), txt("a"), Null), `[1, "a", null]`},
		{list(p, Int(2)), `[("k" => 1), 2]`},
		{p, `"k" => 1`},
		{nested, `("k" => 1) => ("k" => 1)`},
		{must(NewArray(NewFloat(1), NewFloat(2))), `[1.0, 2.0]`},
		{must(NewRecord([]string{"x", "match", "名前"}, []Value{Int(1), Int(2), Int(3)})), `{x: 1, "match": 2, 名前: 3}`},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestValueNestingLimit(t *testing.T) {
	var v Value = Int(1)
	var err error
	for i := 1; i < MaxNesting; i++ {
		if v, err = NewList(v); err != nil {
			t.Fatalf("depth %d: %v", i+1, err)
		}
	}
	if _, err := NewList(v); err == nil {
		t.Error("33 levels accepted")
	}
	if _, err := NewMap([]Value{txt("k")}, []Value{v}); err == nil {
		t.Error("33 levels accepted in a map")
	}
}

func TestRecord(t *testing.T) {
	r := must(NewRecord([]string{"b", "a"}, []Value{Int(1), Int(2)}))
	if n, _ := r.Field(0); n != "b" {
		t.Error("declared order not kept")
	}
	for _, names := range [][]string{{"a", "a"}, {"1x"}, {}} {
		vals := make([]Value, len(names))
		if _, err := NewRecord(names, vals); err == nil {
			t.Errorf("NewRecord(%q) accepted", names)
		}
	}
}

func TestCompositeOrder(t *testing.T) {
	// In increasing order (spec/types.md §11.2).
	vals := []Value{
		Ref{ID{9}},
		must(NewDoc(jsonFormat(), `{}`)),
		must(NewImage(pngBytes(t, 1, 1))),
		must(NewPair(Int(1), Int(2))),
		must(NewArray(Int(1), Int(2))),
		list(Int(1), Int(2)),
		list(Int(1), Int(2), Int(0)),
		list(Int(1), Int(3)),
		must(NewRecord([]string{"a"}, []Value{Int(1)})),
		must(NewRecord([]string{"a"}, []Value{Int(2)})),
		must(NewRecord([]string{"b"}, []Value{Int(0)})),
		set(),
		set(Int(1)),
		set(Int(1), Int(2)),
		must(MapOfText(map[string]Value{"a": Int(1)})),
		must(MapOfText(map[string]Value{"a": Int(2)})),
		must(NewQueueOf(MustParseType("queue.fifo<int>"), []Value{Int(1)}, nil)),
		must(NewQueueOf(MustParseType("queue.lifo<int>"), []Value{Int(1)}, nil)),
		must(NewQueueOf(MustParseType("queue.priority<int>"), []Value{Int(1)}, []int64{1})),
		must(NewQueueOf(MustParseType("queue.priority<int>"), []Value{Int(1)}, []int64{2})),
	}
	for i := range vals {
		for j := range vals {
			if got, want := Compare(vals[i], vals[j]), cmpInt(i, j); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", vals[i], vals[j], got, want)
			}
		}
	}
	if !Equal(list(Int(1)), list(NewFloat(1))) {
		t.Error("[1] should equal [1.0]")
	}
}

func TestQueues(t *testing.T) {
	push := func(q Queue, v Value, p int64) Queue {
		t.Helper()
		return must(q.Push(v, p))
	}
	drain := func(q Queue) string {
		var out []string
		for q.Len() > 0 {
			var v Value
			q, v = q.Pop()
			out = append(out, v.String())
		}
		return strings.Join(out, " ")
	}

	fifo := must(NewQueue(MustParseType("queue.fifo<int, max 2>")))
	fifo = push(push(fifo, Int(1), 0), Int(2), 0)
	if _, err := fifo.Push(Int(3), 0); err != ErrQueueFull {
		t.Errorf("push to full fifo: %v", err)
	}
	if got := drain(fifo); got != "1 2" {
		t.Errorf("fifo: %s", got)
	}

	lifo := must(NewQueue(MustParseType("queue.lifo<int>")))
	lifo = push(push(push(lifo, Int(1), 0), Int(2), 0), Int(3), 0)
	if got := drain(lifo); got != "3 2 1" {
		t.Errorf("lifo: %s", got)
	}

	ring := must(NewQueue(MustParseType("queue.ring<int, 2>")))
	for i := range 5 {
		ring = push(ring, Int(i), 0)
	}
	if got := drain(ring); got != "3 4" {
		t.Errorf("ring: %s", got)
	}

	high := must(NewQueue(MustParseType("queue.priority<text>")))
	low := must(NewQueue(MustParseType("queue.priority<text, low>")))
	for _, x := range []struct {
		s string
		p int64
	}{{"a", 1}, {"b", 5}, {"c", 1}, {"d", 3}, {"e", 5}} {
		high = push(high, txt(x.s), x.p)
		low = push(low, txt(x.s), x.p)
	}
	if high.String() != `[5 => "b", 5 => "e", 3 => "d", 1 => "a", 1 => "c"]` {
		t.Errorf("high: %s", high)
	}
	if got := drain(low); got != `"a" "c" "d" "b" "e"` {
		t.Errorf("low: %s", got)
	}

	empty, v := must(NewQueue(MustParseType("queue.fifo<int>"))).Pop()
	if !IsNull(v) || !IsNull(empty.Peek()) {
		t.Error("empty pop should give null")
	}
}

func TestQueueImmutable(t *testing.T) {
	q1 := must(must(NewQueue(MustParseType("queue.fifo<int>"))).Push(Int(1), 0))
	q1, _ = must(q1.Push(Int(2), 0)).Pop() // q1 = [2] sharing a backing array
	q2 := must(q1.Push(Int(3), 0))
	q3 := must(q1.Push(Int(4), 0))
	if q1.String() != "[2]" || q2.String() != "[2, 3]" || q3.String() != "[2, 4]" {
		t.Errorf("q1 %s q2 %s q3 %s", q1, q2, q3)
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImage(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Black, color.White})
	encoders := map[string]func(*bytes.Buffer) error{
		"png":  func(b *bytes.Buffer) error { return png.Encode(b, img) },
		"jpeg": func(b *bytes.Buffer) error { return jpeg.Encode(b, img, nil) },
		"gif":  func(b *bytes.Buffer) error { return gif.Encode(b, img, nil) },
		"bmp":  func(b *bytes.Buffer) error { return bmp.Encode(b, img) },
		"tiff": func(b *bytes.Buffer) error { return tiff.Encode(b, img, nil) },
	}
	for format, enc := range encoders {
		var buf bytes.Buffer
		if err := enc(&buf); err != nil {
			t.Fatal(err)
		}
		v, err := NewImage(buf.Bytes())
		if err != nil {
			t.Errorf("%s: %v", format, err)
			continue
		}
		if v.Format() != format || v.Width() != 4 || v.Height() != 3 || !bytes.Equal(v.Data(), buf.Bytes()) {
			t.Errorf("%s: got %s %dx%d", format, v.Format(), v.Width(), v.Height())
		}
	}
	if _, err := NewImage([]byte("not an image")); err == nil {
		t.Error("garbage accepted")
	}
}

func jsonFormat() Format {
	f, _ := LookupFormat("json")
	return f
}

func parse(t *testing.T, format, text string) Value {
	t.Helper()
	f, ok := LookupFormat(format)
	if !ok {
		t.Fatalf("no format %s", format)
	}
	v, err := f.Parse(text)
	if err != nil {
		t.Fatalf("%s %q: %v", format, text, err)
	}
	return v
}

func TestJSON(t *testing.T) {
	v := parse(t, "json", `{"b": [1, 1.0, 1.5, 1e3, 1e30, 9007199254740993, true, null, "x"], "a": {}, "a": 2}`)
	want := `{"a": 2, "b": [1, 1, 1.5, 1000, 1e+30, 9007199254740993, true, null, "x"]}`
	if v.String() != want {
		t.Errorf("got  %s\nwant %s", v, want)
	}
	b, _ := v.(Map).Get(txt("b"))
	if c := b.(List).At(1).Code(); c != CodeInt {
		t.Errorf("1.0 parsed as %s", c)
	}
	f := jsonFormat()
	for _, bad := range []string{``, `{`, `[1] [2]`, `1e400`, `{"a" 1}`, `[1,]`} {
		if _, err := f.Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := NewDoc(f, `{"ok": true}`); err != nil {
		t.Error(err)
	}
	if _, err := NewDoc(f, `{bad}`); err == nil {
		t.Error("invalid JSON doc accepted")
	}
}

func TestXML(t *testing.T) {
	v := parse(t, "xml", `<?xml version="1.0"?>
<!-- c --><a x="1" xmlns:p="urn:p"><b>hi</b> tail <p:c/></a>`)
	want := `{name: "a", attrs: {"x": "1", "xmlns:p": "urn:p"}, children: [{name: "b", attrs: {}, children: ["hi"]}, " tail ", {name: "{urn:p}c", attrs: {}, children: []}]}`
	if v.String() != want {
		t.Errorf("got  %s\nwant %s", v, want)
	}
	if _, err := Conform(v, XMLElementType); err != nil {
		t.Errorf("parsed element doesn't conform: %v", err)
	}
	f, _ := LookupFormat("xml")
	for _, bad := range []string{``, `<a>`, `<a/><b/>`, `text<a/>`, `<a></b>`} {
		if _, err := f.Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDelimited(t *testing.T) {
	if v := parse(t, "csv", "a,\"b,c\"\n1,2\n"); v.String() != `[["a", "b,c"], ["1", "2"]]` {
		t.Errorf("csv: %s", v)
	}
	if v := parse(t, "tsv", "a\tb\n1\t2\n"); v.String() != `[["a", "b"], ["1", "2"]]` {
		t.Errorf("tsv: %s", v)
	}
	f, _ := LookupFormat("csv")
	if _, err := f.Parse("a,b\n1\n"); err == nil {
		t.Error("ragged csv accepted")
	}
}

func TestDerivedFormat(t *testing.T) {
	inv := must(NewDerivedFormat("invoice", jsonFormat(),
		MustParseType("record{number: text, total: decimal(12,2), lines: list<record{sku: text, qty: int}>}")))
	v, err := inv.Parse(`{"number": "A1", "total": 12, "lines": [{"sku": "x", "qty": 2}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != `{number: "A1", total: 12.00d, lines: [{sku: "x", qty: 2}]}` {
		t.Errorf("got %s", v)
	}
	d, err := NewDoc(inv, `{"number": "A2", "total": 1, "lines": []}`)
	if err != nil || d.Format() != "invoice" {
		t.Errorf("NewDoc: %v", err)
	}
	if _, err := Conform(d, DocOf("invoice")); err != nil {
		t.Error(err)
	}
	if _, err := Conform(d, DocOf("json")); err == nil {
		t.Error("invoice doc accepted as doc<json>")
	}
	for _, bad := range []string{`{"number": "A1"}`, `{"number": 1, "total": 1, "lines": []}`, `{"number": "A", "total": "1.5", "lines": []}`} {
		if _, err := NewDoc(inv, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// JSON numbers at decimal positions are read exactly; elsewhere they
	// stay floats.
	mixed := must(NewDerivedFormat("mixed", jsonFormat(),
		MustParseType("record{d: decimal, f: float, a: any, l: list<decimal>, u: union<decimal | text>}")))
	v, err = mixed.Parse(`{"d": 0.12345678901234567890123, "f": 0.5, "a": 2.5, "l": [1.5, 2e-3, 3], "u": 1.10}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{d: 0.12345678901234567890123d, f: 0.5, a: 2.5, l: [1.5d, 0.002d, 3d], u: 1.10d}`; v.String() != want {
		t.Errorf("got  %s\nwant %s", v, want)
	}
	v, err = inv.Parse(`{"number": "A", "total": 1.5, "lines": []}`)
	if err != nil || v.String() != `{number: "A", total: 1.50d, lines: []}` {
		t.Errorf("total 1.5: %v, %v", v, err)
	}
}

func TestConform(t *testing.T) {
	ok := []struct {
		v    Value
		typ  string
		want string
	}{
		{Int(1), "bigint", "1n"},
		{Int(5), "decimal(5,2)", "5.00d"},
		{dec("1.005"), "decimal(5,2)", "1.00d"},
		{Null, "int?", "null"},
		{txt("x"), "any", `"x"`},
		{list(Int(1), Int(2)), "list<bigint>", "[1n, 2n]"},
		{list(Int(1), Int(2)), "array<int, 2>", "[1, 2]"},
		{must(MapOfText(nil)), "set<int>", "{}"},
		{must(MapOfText(map[string]Value{"x": NewFloat(1)})), "record{x: float, y: float?}", "{x: 1.0, y: null}"},
		{Int(1), "union<decimal | int>", "1"},
		{Int(1), "union<text | decimal>", "1d"},
		{list(Int(1), Int(2)), "queue.lifo<int>", "[1, 2]"},
		{list(must(NewPair(Int(1), txt("a"))), must(NewPair(Int(9), txt("b")))), "queue.priority<text>", `[9 => "b", 1 => "a"]`},
		{must(MapOfText(map[string]Value{"k": Int(1)})), "map<text, decimal>", `{"k": 1d}`},
	}
	for _, c := range ok {
		got, err := Conform(c.v, MustParseType(c.typ))
		if err != nil {
			t.Errorf("Conform(%s, %s): %v", c.v, c.typ, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("Conform(%s, %s) = %s, want %s", c.v, c.typ, got, c.want)
		}
	}

	bad := []struct {
		v   Value
		typ string
	}{
		{Int(1), "float"},
		{NewFloat(1), "int"},
		{NewFloat(1), "decimal"},
		{Null, "int"},
		{dec("1000"), "decimal(5,2)"},
		{list(Int(1)), "array<int, 2>"},
		{list(txt("a")), "list<int>"},
		{must(MapOfText(map[string]Value{"x": NewFloat(1)})), "record{y: float}"},
		{must(MapOfText(map[string]Value{"z": Int(1)})), "record{x: int?}"},
		{txt("a"), "union<int | float>"},
		{list(Int(1), Int(2), Int(3)), "queue.fifo<int, max 2>"},
		{list(Int(1)), "queue.priority<int>"},
		{must(NewQueueOf(MustParseType("queue.fifo<int>"), nil, nil)), "queue.lifo<int>"},
	}
	for _, c := range bad {
		if got, err := Conform(c.v, MustParseType(c.typ)); err == nil {
			t.Errorf("Conform(%s, %s) = %s, want error", c.v, c.typ, got)
		}
	}
}

func TestOrderIsTotal(t *testing.T) {
	vals := []Value{
		Null, Bool(false), Int(1), NewFloat(1), dec("1.0"), NewFloat(-2), txt(""), txt("a"),
		list(), list(Int(1)), list(NewFloat(1)), list(Int(1), Null), must(NewArray(Int(1))),
		must(NewPair(Int(1), Null)), set(), set(Int(1)), must(MapOfText(nil)),
		must(NewRecord([]string{"a"}, []Value{Null})),
	}
	for _, a := range vals {
		for _, b := range vals {
			if Compare(a, b) != -Compare(b, a) {
				t.Errorf("Compare not antisymmetric for %s, %s", a, b)
			}
			for _, c := range vals {
				if Compare(a, b) <= 0 && Compare(b, c) <= 0 && Compare(a, c) > 0 {
					t.Errorf("not transitive: %s <= %s <= %s", a, b, c)
				}
			}
		}
	}
}

func TestKeyableComposites(t *testing.T) {
	rec := must(NewRecord([]string{"a"}, []Value{Int(1)}))
	arr := must(NewArray(Int(1), txt("x")))
	if s := set(rec, arr, rec); s.Len() != 2 {
		t.Errorf("set = %s", s)
	}
	badRec := must(NewRecord([]string{"a"}, []Value{list()}))
	badArr := must(NewArray(set()))
	for _, v := range []Value{badRec, badArr, must(NewPair(Int(1), list()))} {
		if IsKeyable(v) {
			t.Errorf("%s should not be keyable", v)
		}
	}
}

func TestConformKeepsUnchangedValues(t *testing.T) {
	// Equal but not identical: 1.5d must become 1.50d, and the int inside a
	// set<decimal> must become a decimal.
	got := must(Conform(set(dec("1.5"), Int(2)), MustParseType("set<decimal(4,2)>")))
	if got.String() != "{1.50d, 2.00d}" {
		t.Errorf("got %s", got)
	}
	l := list(Int(1), list(txt("a")))
	same := must(Conform(l, MustParseType("list<any>")))
	if !identical(l, same) {
		t.Error("identical value changed")
	}
	if identical(list(Int(1)), list(NewFloat(1))) || identical(dec("1.5"), dec("1.50")) {
		t.Error("identical too loose")
	}
}

func TestDocAndImageLiterals(t *testing.T) {
	d := must(NewDoc(jsonFormat(), "{\"a\":\n1}"))
	if d.String() != `cast("{\"a\":\n1}" as doc<json>)` || d.Text() != "{\"a\":\n1}" {
		t.Errorf("got %s", d)
	}
	img := must(NewImage(pngBytes(t, 1, 1)))
	if !strings.HasPrefix(img.String(), `cast(x"89504e47`) || !strings.HasSuffix(img.String(), `" as image)`) {
		t.Errorf("got %s", img)
	}
}
