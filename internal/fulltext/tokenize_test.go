package fulltext

import (
	"slices"
	"strings"
	"testing"
)

// Text split anywhere, even inside a character, tokenizes the same.
func TestTokenizerPieces(t *testing.T) {
	text := "Résumé of 東京 trip — naïve café, Ελληνικά and ру́сский; e\u0301t\u00e9 " +
		strings.Repeat("x", 300) + " end \xe2\x82 bad"
	want := Tokenize(text)
	for cut := 0; cut <= len(text); cut++ {
		for _, step := range []int{1, 2, 3, 5} {
			var got []Token
			tk := NewTokenizer(func(tok Token) { got = append(got, tok) })
			tk.WriteString(text[:cut])
			for i := cut; i < len(text); i += step {
				tk.Write([]byte(text[i:min(i+step, len(text))]))
			}
			tk.Close()
			if !slices.Equal(got, want) {
				t.Fatalf("cut %d step %d:\n got %v\nwant %v", cut, step, got, want)
			}
		}
	}
}
