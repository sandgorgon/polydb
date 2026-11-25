package value

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestTextChecker(t *testing.T) {
	cases := []struct {
		pieces []string
		ok     bool
	}{
		{[]string{"abc", "déf"}, true},
		{[]string{"ab\xc3", "\xa9cd"}, true},       // é split across pieces
		{[]string{"a\xe2", "\x82", "\xac!"}, true}, // € split three ways
		{[]string{"ab\xc3", "x"}, false},           // a broken character
		{[]string{"abc\xe2\x82"}, false},           // ends inside one
		{[]string{"\xff"}, false},
		{[]string{strings.Repeat("ü", 1000), "\xf0\x9f", "\x98\x80"}, true},
	}
	for _, c := range cases {
		var tc TextChecker
		for _, p := range c.pieces {
			tc.Write([]byte(p))
		}
		if err := tc.Err(); (err == nil) != c.ok {
			t.Errorf("%q: %v", c.pieces, err)
		}
	}
}

func TestLazy(t *testing.T) {
	text := strings.Repeat("héllo ", 100)
	l := Lazy{Ref: BlobRef{Of: CodeText, Hash: sha256.Sum256([]byte(text)), Len: int64(len(text))},
		Src: bytes.NewReader([]byte(text))}
	if l.Code() != CodeText {
		t.Fatal(l.Code())
	}
	if n, err := l.RuneCount(); err != nil || n != int64(len([]rune(text))) {
		t.Fatalf("rune count %d %v", n, err)
	}
	v, err := Load(l)
	if err != nil || !Equal(v, MustText(text)) {
		t.Fatalf("load: %v", err)
	}
	// Comparing and keying never read the content, and never panic.
	other := l
	other.Ref.Hash[0]++
	if Compare(l, l) != 0 || Compare(l, other) == 0 || Compare(l, MustText("x")) == 0 {
		t.Error("compare")
	}
	// A lazy value keys like its content, reading only its start.
	lk, err := Key(l)
	if err != nil {
		t.Fatal(err)
	}
	if ek, _ := Key(MustText(text)); !bytes.Equal(lk, ek) || len(lk) > KeyPrefixLen+40 {
		t.Errorf("lazy key %d bytes, eager %d", len(lk), len(ek))
	}
}

func TestLongKeys(t *testing.T) {
	p := strings.Repeat("p", KeyPrefixLen)
	vals := []string{"", "a", p[:KeyPrefixLen-1], p, p + "a", p + "b", p + "\x00", "q", "q" + p}
	for _, a := range vals {
		for _, b := range vals {
			ka, kb := must(Key(MustText(a))), must(Key(MustText(b)))
			want := Compare(MustText(a), MustText(b))
			got := bytes.Compare(ka, kb)
			sameGroup := len(a) > KeyPrefixLen && len(b) > KeyPrefixLen && a[:KeyPrefixLen] == b[:KeyPrefixLen]
			if (want == 0) != (got == 0) || !sameGroup && want != got {
				t.Errorf("%.3q… vs %.3q… (len %d, %d): key order %d, value order %d", a, b, len(a), len(b), got, want)
			}
			g, err := SameKeyGroup(MustText(a), MustText(b))
			if err != nil || g != (sameGroup || a == b) {
				t.Errorf("SameKeyGroup(len %d, len %d) = %v", len(a), len(b), g)
			}
		}
	}
	// Long keys stay short, and are self-delimiting in composites.
	long := MustText(strings.Repeat("x", 1<<20))
	if k := must(Key(long)); len(k) != 1+KeyPrefixLen+2+32 {
		t.Errorf("long key is %d bytes", len(k))
	}
	a1 := must(NewArray(long, Int(1)))
	a2 := must(NewArray(long, Int(2)))
	if bytes.Compare(must(Key(a1)), must(Key(a2))) >= 0 {
		t.Error("composite keys with a long element")
	}
	if IsKeyable(BlobRef{Of: CodeText}) {
		t.Error("a blob reference is keyable")
	}
}
