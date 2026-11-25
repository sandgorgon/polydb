package value

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/big"
	"slices"
	"testing"
	"time"
)

// sampleValues returns at least one value of every type.
func sampleValues(t testing.TB) []Value {
	tm := must(ParseTime("0001-01-01T00:00:00Z"))
	rec := must(NewRecord([]string{"a", "b"}, []Value{Int(1), Null}))
	fifo := must(NewQueueOf(MustParseType("queue.fifo<int, max 5>"), []Value{Int(1), Int(2)}, nil))
	ring := must(NewQueueOf(MustParseType("queue.ring<text, 3>"), []Value{txt("a")}, nil))
	prio := must(NewQueueOf(MustParseType("queue.priority<text, low>"), []Value{txt("a"), txt("b")}, []int64{-3, 7}))
	return []Value{
		Null, Bool(false), Bool(true), Int(0), Int(-1), Int(math.MaxInt64), Int(math.MinInt64),
		must(ParseBigInt("-123456789012345678901234567890")), must(NewBigInt(new(big.Int))),
		NewFloat(0), NewFloat(-1.5), NewFloat(math.NaN()), NewFloat(math.Inf(-1)), NewFloat(5e-324),
		dec("0"), dec("-12.50"), dec("0.001"),
		txt(""), txt("héllo\x00"), must(NewBytes([]byte{0, 1, 2}, "application/octet-stream")),
		must(NewBytes(nil, "")), tm, must(ParseTime("2026-11-22T10:00:00.123456789Z")),
		must(ParseDate("0001-01-01")), must(ParseDate("9999-12-31")), Duration(-time.Hour),
		NewIDGenerator(1).New(), Ref{ID{1, 2, 3}},
		must(NewImage(pngBytes(t.(*testing.T), 2, 3))),
		must(NewDoc(jsonFormat(), `{"a": [1, 2]}`)),
		list(), list(Int(1), txt("x"), list(Null)), must(NewArray(Int(1), Int(2))),
		set(), set(Int(3), txt("a"), Int(1)), must(MapOfText(map[string]Value{"k": Int(1), "j": list()})),
		must(NewPair(Int(1), rec)), rec, fifo, ring, prio,
		must(NewQueueOf(MustParseType("queue.lifo<int>"), nil, nil)),
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	for _, v := range sampleValues(t) {
		b := Encode(v)
		got, err := Decode(b)
		if err != nil {
			t.Errorf("%s: %v", v, err)
			continue
		}
		if !identical(got, v) {
			t.Errorf("round trip: got %s, want %s", got, v)
		}
		if !bytes.Equal(Encode(got), b) {
			t.Errorf("%s: re-encoding differs", v)
		}
	}
}

func TestEncodeQueueParams(t *testing.T) {
	q := must(NewQueueOf(MustParseType("queue.priority<int, low, max 4>"), []Value{Int(1)}, []int64{2}))
	got := must(Decode(Encode(q))).(Queue)
	if !got.LowFirst() || got.Limit() != 4 || got.Code() != CodePriority {
		t.Errorf("got low=%v limit=%d %s", got.LowFirst(), got.Limit(), got.Code())
	}
}

func TestDecodeRejectsNonCanonical(t *testing.T) {
	negZero := binary.BigEndian.AppendUint64([]byte{tagFloat}, 1<<63)
	otherNaN := binary.BigEndian.AppendUint64([]byte{tagFloat}, 0x7FF0000000000002)
	cases := map[string][]byte{
		"empty":             {},
		"unknown tag":       {0xF0},
		"long varint":       {tagInt, 0x80, 0x00},
		"truncated":         {tagText, 5, 'a'},
		"-0":                negZero,
		"other NaN":         otherNaN,
		"bigint lead zero":  {tagBigInt, 0, 2, 0, 1},
		"bigint -0":         {tagBigInt, 1, 0},
		"not NFC":           append([]byte{tagText, 3}, "é"...),
		"bad UTF-8":         {tagText, 1, 0xFF},
		"set out of order":  {tagSet, 2, tagInt, 4, tagInt, 2},
		"set duplicate":     {tagSet, 2, tagInt, 2, tagInt, 2},
		"map out of order":  {tagMap, 2, tagInt, 4, tagNull, tagInt, 2, tagNull},
		"set of lists":      {tagSet, 1, tagList, 0},
		"empty array":       {tagArray, 0},
		"huge count":        {tagList, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F},
		"ring capacity 0":   {tagRing, 0, 0},
		"fifo over max":     {tagFifo, 1, 2, tagNull, tagNull},
		"prio not in order": {tagPriority, 0, 0, 2, 2, tagNull, 4, tagNull},
		"bad record name":   {tagRecord, 1, 2, '1', 'x', tagNull},
		"date out of range": binary.BigEndian.AppendUint32([]byte{tagDate}, 1<<31),
		"time nanos":        binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint64([]byte{tagTime}, 0), 1e9),
		"fake image":        {tagImage, 3, 'p', 'n', 'g', 1, 1, 1, 0},
		"trailing bytes":    {tagNull, tagNull},
	}
	for name, b := range cases {
		if v, err := Decode(b); err == nil {
			t.Errorf("%s: decoded %s", name, v)
		}
	}
	deep := slices.Repeat([]byte{tagList, 1}, MaxNesting)
	if _, err := Decode(append(deep, tagNull)); err == nil {
		t.Error("33 levels decoded")
	}
	if _, err := Decode(append(deep[2:], tagNull)); err != nil {
		t.Errorf("32 levels: %v", err)
	}
}

func TestNumberKeyOrder(t *testing.T) {
	two53 := new(big.Int).Lsh(big.NewInt(1), 53)
	// Each row is greater than the one before; values in one row are equal.
	rows := [][]Value{
		{NewFloat(math.Inf(-1))},
		{NewFloat(-math.MaxFloat64)},
		{must(NewBigInt(new(big.Int).Neg(new(big.Int).Add(two53, big.NewInt(1)))))},
		{NewFloat(-(1 << 53)), Int(-(1 << 53))},
		{Int(-100), dec("-100.0"), NewFloat(-100)},
		{dec("-1.5"), NewFloat(-1.5)},
		{Int(-1)},
		{NewFloat(-0.1)}, // a little less than -0.1
		{dec("-0.10")},
		{NewFloat(-5e-324)},
		{Int(0), NewFloat(0), dec("0.000"), must(NewBigInt(new(big.Int)))},
		{NewFloat(5e-324)},
		{dec("0.000001")},
		{dec("0.1")},
		{NewFloat(0.1)},
		{dec("0.12")},
		{dec("0.123")},
		{dec("0.2")},
		{Int(1), NewFloat(1), dec("1.00"), must(ParseBigInt("1"))},
		{dec("1.01")},
		{Int(10)},
		{Int(99)},
		{Int(100), NewFloat(100)},
		{Int(1 << 53), NewFloat(1 << 53)},
		{Int(1<<53 + 1)},
		{Int(math.MaxInt64)},
		{NewFloat(math.MaxFloat64)},
		{NewFloat(math.Inf(1))},
		{NewFloat(math.NaN())},
	}
	var prev []byte
	for i, row := range rows {
		k0 := must(Key(row[0]))
		for _, v := range row[1:] {
			if k := must(Key(v)); !bytes.Equal(k, k0) {
				t.Errorf("row %d: key(%s) != key(%s)", i, v, row[0])
			}
		}
		if i > 0 && bytes.Compare(prev, k0) >= 0 {
			t.Errorf("row %d: key(%s) not greater than previous", i, row[0])
		}
		if i > 0 && Compare(rows[i-1][0], row[0]) >= 0 {
			t.Errorf("row %d: Compare disagrees with the table", i)
		}
		prev = k0
	}
}

func TestKeyMatchesCompare(t *testing.T) {
	var keyable []Value
	for _, v := range sampleValues(t) {
		if IsKeyable(v) {
			keyable = append(keyable, v)
		}
	}
	keyable = append(keyable,
		txt("a"), txt("a\x00"), txt("a\x00b"), txt("ab"), txt("b"),
		must(NewPair(Int(1), Int(2))), must(NewPair(Int(1), Int(2.0))), must(NewArray(Int(1), Int(2))),
		must(NewArray(Int(1), Int(2), Int(3))), must(NewArray(Int(1))),
		must(NewRecord([]string{"a"}, []Value{Int(1)})), must(NewRecord([]string{"ab"}, []Value{Int(0)})),
	)
	for _, a := range keyable {
		for _, b := range keyable {
			checkKeyOrder(t, a, b)
		}
	}
	if _, err := Key(list()); err == nil {
		t.Error("list key accepted")
	}
}

func checkKeyOrder(t testing.TB, a, b Value) {
	t.Helper()
	ka, kb := must(Key(a)), must(Key(b))
	if got, want := bytes.Compare(ka, kb), Compare(a, b); got != want {
		t.Fatalf("key order %d, Compare %d for %s vs %s\n%x\n%x", got, want, a, b, ka, kb)
	}
}

// FuzzDecode checks that the decoder never panics and only accepts
// canonical encodings: re-encoding a decoded value gives the same bytes.
func FuzzDecode(f *testing.F) {
	for _, v := range sampleValues(&testing.T{}) {
		f.Add(Encode(v))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, n, err := DecodeValue(b)
		if err != nil {
			return
		}
		if enc := Encode(v); !bytes.Equal(enc, b[:n]) {
			t.Fatalf("decoded %s from %x, which re-encodes as %x", v, b[:n], enc)
		}
	})
}

// FuzzKeyOrder builds pairs of keyable values from fuzz input and checks
// that byte order of their keys matches Compare.
func FuzzKeyOrder(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	f.Add([]byte{3, 0x3F, 0xF0, 0, 0, 0, 0, 0, 0, 2, 1, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{5, 1, 0x80, 4, 3, 1, 0, 1, 5, 0, 0, 0, 0, 0, 0, 0, 1, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		g := &gen{b: b}
		a, c := g.value(1), g.value(1)
		checkKeyOrder(t, a, c)
		if ka, kc := must(Key(a)), must(Key(c)); bytes.HasPrefix(ka, kc) && len(ka) != len(kc) ||
			bytes.HasPrefix(kc, ka) && len(ka) != len(kc) {
			t.Fatalf("key of %s is a prefix of key of %s", a, c)
		}
	})
}

// gen deterministically builds keyable values from fuzz bytes.
type gen struct {
	b []byte
	i int
}

func (g *gen) byte() byte {
	if g.i >= len(g.b) {
		return 0
	}
	g.i++
	return g.b[g.i-1]
}

func (g *gen) u64() uint64 {
	var x uint64
	for range 8 {
		x = x<<8 | uint64(g.byte())
	}
	return x
}

func (g *gen) value(depth int) Value {
	switch k := g.byte() % 14; {
	case k == 0:
		return Null
	case k == 1:
		return Bool(g.byte()&1 == 1)
	case k == 2:
		return Int(int64(g.u64()) >> (g.byte() % 64))
	case k == 3:
		return NewFloat(math.Float64frombits(g.u64()))
	case k == 4:
		return NewFloat(float64(int64(g.u64())>>(g.byte()%64)) / float64(int(g.byte()%16)+1))
	case k == 5:
		n := new(big.Int).SetUint64(g.u64())
		n.Lsh(n, uint(g.byte()))
		if g.byte()&1 == 1 {
			n.Neg(n)
		}
		if g.byte()&1 == 1 {
			return must(NewBigInt(n))
		}
		return must(NewDecimal(n, int(g.byte()%40)))
	case k == 6:
		s := make([]byte, g.byte()%4)
		for i := range s {
			s[i] = "a\x00b\xc3"[g.byte()%3]
		}
		return MustText(string(s))
	case k == 7:
		s := make([]byte, g.byte()%4)
		for i := range s {
			s[i] = g.byte() % 3
		}
		return must(NewBytes(s, ""))
	case k == 8:
		return Date{int32(g.u64()%uint64(maxDays-minDays)) + minDays}
	case k == 9:
		return must(NewTime(time.Unix(int64(g.u64()%(1<<35))-(1<<34), int64(g.byte())*3)))
	case k == 10:
		return Duration(int64(g.u64()))
	case k == 11:
		var id ID
		id[15] = g.byte() % 3
		if g.byte()&1 == 1 {
			return Ref{id}
		}
		return id
	case depth >= 4:
		return Int(int64(g.byte() % 3))
	case k == 12:
		elems := make([]Value, g.byte()%3+1)
		for i := range elems {
			elems[i] = g.value(depth + 1)
		}
		if g.byte()&1 == 1 {
			return must(NewArray(elems...))
		}
		return must(NewPair(elems[0], g.value(depth+1)))
	default:
		names := []string{"a", "ab", "b"}[:g.byte()%3+1]
		vals := make([]Value, len(names))
		for i := range vals {
			vals[i] = g.value(depth + 1)
		}
		return must(NewRecord(names, vals))
	}
}

func TestDecodeListHead(t *testing.T) {
	b := Encode(must2(NewList(Int(1), MustText("two"), Bool(true))))
	for n, want := range map[int]int{0: 0, 1: 1, 3: 3, 9: 3} {
		vs, err := DecodeListHead(b, n)
		if err != nil || len(vs) != want {
			t.Errorf("head %d: %v %v", n, vs, err)
		}
	}
	if vs, _ := DecodeListHead(b, 2); !Equal(vs[1], MustText("two")) {
		t.Errorf("second element: %v", vs[1])
	}
	for _, bad := range [][]byte{nil, Encode(Int(1)), b[:3]} {
		if _, err := DecodeListHead(bad, 3); err == nil {
			t.Errorf("%x: no error", bad)
		}
	}
}

func TestBlobRefRoundTrip(t *testing.T) {
	r := BlobRef{Of: CodeBytes, Len: 1 << 20, Media: "application/pdf"}
	r.Hash[0], r.Hash[31] = 0xab, 0xcd
	l, err := NewList(Int(1), r, Null)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(Encode(l)); err == nil {
		t.Fatal("Decode accepted a blob reference")
	}
	if _, _, err := DecodeValue(Encode(r)); err == nil {
		t.Fatal("DecodeValue accepted a blob reference")
	}
	got, err := DecodeStored(Encode(l))
	if err != nil {
		t.Fatal(err)
	}
	if g := got.(List).At(1); g != r {
		t.Fatalf("got %v, want %v", g, r)
	}
	bad := Encode(r)
	bad[1] = byte(CodeInt)
	if _, err := DecodeStored(bad); err == nil {
		t.Fatal("decoded a blob reference to an int")
	}
	img := Encode(BlobRef{Of: CodeImage, Len: 3})
	if v, err := DecodeStored(img); err != nil || v.(BlobRef).Of != CodeImage {
		t.Fatalf("image ref: %v %v", v, err)
	}
	if _, err := FromBlob(BlobRef{Of: CodeText, Len: 2}, []byte("abc")); err == nil {
		t.Fatal("FromBlob accepted the wrong length")
	}
	v, err := FromBlob(BlobRef{Of: CodeBytes, Len: 3, Media: "x/y"}, []byte("abc"))
	if err != nil || v.(Bytes).MediaType() != "x/y" || string(v.(Bytes).Go()) != "abc" {
		t.Fatalf("FromBlob bytes: %v %v", v, err)
	}
}
