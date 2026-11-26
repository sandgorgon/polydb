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
	t := NewTokenizer(func(tok Token) { out = append(out, tok) })
	t.WriteString(text)
	t.Close()
	return out
}

// maxWordRunes bounds the runes of a word kept before folding: Fold
// keeps at most MaxWordLen bytes, so a longer run (text with no spaces)
// gives the same word.
const maxWordRunes = 4 * MaxWordLen

// Tokenizer splits text arriving in pieces into folded words, as
// Tokenize does, holding only the current word: a character may span
// two pieces.
type Tokenizer struct {
	emit  func(Token)
	n     int
	cur   []rune
	carry []byte
}

// NewTokenizer returns a tokenizer that calls emit for each word.
func NewTokenizer(emit func(Token)) *Tokenizer { return &Tokenizer{emit: emit} }

func (t *Tokenizer) flush() {
	if len(t.cur) > 0 {
		if w := Fold(string(t.cur)); w != "" {
			t.emit(Token{w, t.n})
			t.n++
		}
		t.cur = t.cur[:0]
	}
}

func (t *Tokenizer) rune(r rune) {
	switch {
	case single(r):
		t.flush()
		t.cur = append(t.cur, r)
		t.flush()
	case wordRune(r):
		if len(t.cur) < maxWordRunes {
			t.cur = append(t.cur, r)
		}
	default:
		t.flush()
	}
}

// WriteString adds a piece of text.
func (t *Tokenizer) WriteString(s string) {
	if old := len(t.carry); old > 0 {
		need := min(utf8.UTFMax-old, len(s))
		buf := append(t.carry, s[:need]...)
		if !utf8.FullRune(buf) {
			t.carry = buf // s was too short to finish the character
			return
		}
		r, size := utf8.DecodeRune(buf)
		t.rune(r)
		if size >= old {
			s = s[size-old:]
		} else {
			s = string(buf[size:old]) + s // an invalid byte: go on after it
		}
		t.carry = buf[:0]
	}
	for i, r := range s {
		if r == utf8.RuneError && !utf8.FullRuneInString(s[i:]) {
			t.carry = append(t.carry[:0], s[i:]...)
			return
		}
		t.rune(r)
	}
}

// Write adds a piece of text.
func (t *Tokenizer) Write(p []byte) (int, error) {
	t.WriteString(string(p))
	return len(p), nil
}

// Close ends the text.
func (t *Tokenizer) Close() {
	for len(t.carry) > 0 {
		r, size := utf8.DecodeRune(t.carry)
		t.rune(r)
		t.carry = t.carry[size:]
	}
	t.flush()
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
