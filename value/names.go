package value

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxNameLen is the maximum length of a name, in code points
// (spec/types.md §14).
const MaxNameLen = 255

// IsIdentifier reports whether s is a plain identifier: a Unicode letter or
// "_" first, then letters, digits or "_", 1 to MaxNameLen code points, in
// NFC. Names that aren't plain identifiers can still be used in Rhizome by
// writing them in backticks; see ValidateName.
func IsIdentifier(s string) bool {
	if s == "" || !utf8.ValidString(s) || !norm.NFC.IsNormalString(s) {
		return false
	}
	n := 0
	for i, r := range s {
		n++
		if n > MaxNameLen {
			return false
		}
		if r == '_' || unicode.IsLetter(r) {
			continue
		}
		if i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

// ValidateName checks a name for a kind, field, relation type, format or
// other catalog object. Names must be plain identifiers (spec/types.md §14).
// what describes the object for the error message, for example "field".
func ValidateName(what, s string) error {
	if !IsIdentifier(s) {
		return fmt.Errorf("invalid %s name %q: must start with a letter or _, contain only letters, digits and _, be NFC-normalized, and be at most %d characters", what, s, MaxNameLen)
	}
	return nil
}
