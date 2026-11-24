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
	if _, err := Key(l); err == nil {
		t.Error("a lazy value keyed")
	}
}
