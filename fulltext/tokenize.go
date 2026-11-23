// Package fulltext is PolyDB's full-text search: the tokenizer that text
// indexes and search() share, the search query syntax, and matching a
// query against a text (spec/rhizome.md §5.4, §8.4).
//
// Words are runs of letters, digits and combining marks (spacing marks
// too, which scripts like Thai and Devanagari need). They are
// compared with case and accents ignored, so "Résumé" matches "resume".
// Chinese, Japanese and Korean characters are each a word of their own,
// so a search for several of them is a phrase. There is no stemming:
// "run" doesn't match "running" (use run*).
package fulltext

import (
	"strings"
	"unicode"

	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// MaxWordLen is the longest word indexed, in bytes after folding; longer
// runs are cut. It keeps index keys small.
const MaxWordLen = 64

// Token is a word and its position in the text, counting words from 0.
type Token struct {
	Word string
	Pos  int
}

// single reports whether r is a character that is a word by itself:
// scripts written without spaces between words.
func single(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// Tokenize splits text into folded words.
func Tokenize(text string) []Token {
	var out []Token
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			if w := Fold(string(cur)); w != "" {
				out = append(out, Token{w, len(out)})
			}
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case single(r):
			flush()
			cur = append(cur, r)
			flush()
		case wordRune(r):
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// Fold normalizes a word for comparison: case folded, accents removed,
// NFC, and at most MaxWordLen bytes. Accents are the combining marks on
// Latin, Greek and Cyrillic letters; in other scripts marks are vowels
// and other parts of the word, so they stay.
func Fold(word string) string {
	// Case folding changes scripts' letters (µ, the micro sign, folds
	// to Greek μ, whose accents go) and isn't idempotent for every
	// script (Cherokee folds small letters to capitals and back), so
	// fold, lower-case and strip accents until nothing changes.
	s := word
	for range 4 {
		next := stripAccents(strings.Map(unicode.ToLower, cases.Fold().String(s)))
		if next == s {
			break
		}
		s = next
	}
	if len(s) > MaxWordLen {
		s = s[:MaxWordLen]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// stripAccents removes the combining marks on Latin, Greek and Cyrillic
// letters, and returns NFC.
func stripAccents(s string) string {
	var b strings.Builder
	var base rune
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			if unicode.In(base, unicode.Latin, unicode.Greek, unicode.Cyrillic) {
				continue
			}
		} else {
			base = r
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}
