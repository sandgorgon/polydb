package fulltext

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func words(text string) string {
	var ws []string
	for _, t := range Tokenize(text) {
		ws = append(ws, t.Word)
	}
	return strings.Join(ws, " ")
}

func TestTokenize(t *testing.T) {
	for text, want := range map[string]string{
		"Hello, World!":          "hello world",
		"Résumé of Ünïcode":      "resume of unicode",
		"ÀÉÎÕÜ àéîõü Ωμέγα Ёлка": "aeiou aeiou ωμεγα елка",
		"Straße":                  "strasse",
		"e-mail me@x.io, 2026-11": "e mail me x io 2026 11",
		"东京タワー 서울":                "东 京 タ ワ ー 서 울",
		"नमस्ते दुनिया":           "नमस्ते दुनिया", // marks in Devanagari stay
		"ภาษาไทย":                 "ภาษาไทย",
		"   ":                     "",
		"don't":                   "don t",
	} {
		if got := words(text); got != want {
			t.Errorf("Tokenize(%q) = %q, want %q", text, got, want)
		}
	}
	toks := Tokenize("a b, c")
	if len(toks) != 3 || toks[2].Pos != 2 || toks[2].Word != "c" {
		t.Errorf("positions: %v", toks)
	}
	long := strings.Repeat("é", 100)
	if w := Fold(long); len(w) > MaxWordLen || !utf8.ValidString(w) {
		t.Errorf("long word folded to %d bytes, valid %v", len(w), utf8.ValidString(w))
	}
	if w := Fold(strings.Repeat("語", 30)); len(w) > MaxWordLen || !utf8.ValidString(w) {
		t.Errorf("long CJK run folded to %q", w)
	}
}

func TestParse(t *testing.T) {
	for q, want := range map[string]string{
		"ann smith":          "ann smith",
		`"Ann Smith" -Jones`: `"ann smith" -jones`,
		"smi* ann OR anna":   "smi* ann OR anna",
		"e-mail":             `"e mail"`,
		"東京":                 `"東 京"`,
		"ann , !":            "ann",
		`"ann" OR "bo b" -x`: `ann OR "bo b" -x`,
		"a OR b OR c":        "a OR b OR c",
		"ORANGE or":          "orange or",
		"Café":               "cafe",
		"-x y":               "-x y",
	} {
		pq, err := Parse(q)
		if err != nil {
			t.Errorf("Parse(%q): %v", q, err)
			continue
		}
		if pq.String() != want {
			t.Errorf("Parse(%q) = %s, want %s", q, pq, want)
		}
	}
	for _, q := range []string{"", "-x", "-x -y", `"open`, "OR a", "a OR", "a OR OR b", "-a OR b", "a OR -b", "*", "e-mail*", "!!"} {
		if _, err := Parse(q); err == nil || !errors.Is(err, ErrQuery) {
			t.Errorf("Parse(%q) = %v, want a query error", q, err)
		}
	}
}

func TestMatch(t *testing.T) {
	text := "Ann Smith wrote the Résumé, then e-mailed it to 東京タワー."
	for q, want := range map[string]bool{
		"ann":               true,
		"ANN smith":         true,
		"ann jones":         false,
		`"ann smith"`:       true,
		`"smith ann"`:       false,
		`"the resume"`:      true,
		"resume -jones":     true,
		"resume -ann":       false,
		"wro*":              true,
		"wrx*":              false,
		"jones OR smith":    true,
		"jones OR bob":      false,
		"e-mailed":          true,
		"東京":                true,
		"京東":                false,
		"smith ann OR bob":  true,
		"smith bob OR carl": false,
		`"mailed it"`:       true,
	} {
		pq, err := Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		if got := pq.Match(text); got != want {
			t.Errorf("%q matches: %v, want %v", q, got, want)
		}
	}
}

// FuzzParse checks that parsing never panics, and that a parsed query
// formats to a query that parses to the same thing.
func FuzzParse(f *testing.F) {
	for _, s := range []string{`a "b c" -d e* f OR g`, `"`, "-", "OR", "東京 OR x*"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		pq, err := Parse(q)
		if err != nil {
			return
		}
		again, err := Parse(pq.String())
		if err != nil || again.String() != pq.String() {
			t.Fatalf("%q → %q → %v %v", q, pq.String(), again, err)
		}
		pq.Match(q)
	})
}

// FuzzTokenize checks that words are folded, valid and short.
func FuzzTokenize(f *testing.F) {
	f.Add("Résumé 東京 नमस्ते")
	f.Fuzz(func(t *testing.T, s string) {
		for i, tok := range Tokenize(s) {
			if tok.Pos != i || tok.Word == "" || len(tok.Word) > MaxWordLen || !utf8.ValidString(tok.Word) {
				t.Fatalf("token %d: %+v", i, tok)
			}
			if Fold(tok.Word) != tok.Word {
				t.Fatalf("%q isn't folded", tok.Word)
			}
		}
	})
}
