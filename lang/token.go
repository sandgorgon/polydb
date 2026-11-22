// Package lang implements the Rhizome language front end: the lexer,
// parser, syntax tree and checker (spec/rhizome.md).
package lang

import (
	"fmt"

	"github.com/sandgorgon/polydb/value"
)

// Pos is a position in source text. Line and Col count from 1; Col counts
// code points.
type Pos struct {
	Offset int
	Line   int
	Col    int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Error is a syntax or checking error at a position.
type Error struct {
	Pos  Pos
	Code string // "syntax", "type", "unknown_field", "not_found", …
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s: %s", e.Pos, e.Code, e.Msg) }

func syntaxErr(p Pos, format string, args ...any) *Error {
	return &Error{Pos: p, Code: "syntax", Msg: fmt.Sprintf(format, args...)}
}

// TokKind is the kind of a token.
type TokKind uint8

const (
	EOF     TokKind = iota
	Ident           // a name; Text is the name (backticks removed)
	Keyword         // a reserved word; Text is the word
	Handle          // @name; Text is the name
	Param           // $name; Text is the name
	Literal         // a literal value; Val holds it
	Punct           // an operator or punctuation; Text is the symbol
)

var tokNames = [...]string{"end of input", "name", "keyword", "handle", "parameter", "literal", "symbol"}

func (k TokKind) String() string { return tokNames[k] }

// Token is one token.
type Token struct {
	Kind   TokKind
	Text   string      // see TokKind
	Val    value.Value // Literal only
	Quoted bool        // Ident written in backticks
	Pos    Pos
	End    int // byte offset just past the token
}

func (t Token) String() string {
	switch t.Kind {
	case EOF:
		return "end of input"
	case Ident:
		if t.Quoted {
			return "`" + t.Text + "`"
		}
		return t.Text
	case Handle:
		return "@" + t.Text
	case Param:
		return "$" + t.Text
	case Literal:
		return t.Val.String()
	}
	return t.Text
}

// Is reports whether t is the keyword or symbol s.
func (t Token) Is(s string) bool { return (t.Kind == Keyword || t.Kind == Punct) && t.Text == s }

// IsWord reports whether t is the unquoted word w, which may be a keyword
// or a contextual keyword lexed as an identifier.
func (t Token) IsWord(w string) bool {
	return (t.Kind == Keyword || t.Kind == Ident && !t.Quoted) && t.Text == w
}

// Keywords are Rhizome's reserved words (spec/rhizome.md §1.5), including
// those reserved for future features.
var Keywords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{
		"alter", "and", "as", "asc", "begin", "between", "bind", "by", "cast", "commit", "create", "database", "default",
		"define", "delete", "desc", "describe", "distinct", "doc", "drop", "exists", "explain", "false", "find", "follow",
		"for", "format", "from", "grant", "group", "has", "having", "history", "if", "in", "index", "inf", "is", "keep", "kind",
		"let", "limit", "match", "matches", "nan", "not", "null", "of", "on", "or", "password", "pop", "priority", "push", "read",
		"relate", "relation", "restrict", "revoke", "role", "rollback", "set", "show", "skip", "sort", "to", "true", "union",
		"unique", "unrelate", "unset", "update", "use", "user", "where", "with", "write",
		// reserved for future features
		"rule", "infer", "derive", "when", "then", "trigger", "view", "extends", "trait",
	} {
		m[w] = true
	}
	return m
}()

// puncts are the symbols, longest first so the lexer matches greedily.
var puncts = []string{
	"++=", "--=",
	"::", "..", "!=", "<=", ">=", "++", "--", "+=", "-=", "*=", "/=", "=>", "->", "~>", "<~",
	";", ",", ".", ":", "(", ")", "[", "]", "{", "}", "|", "=", "<", ">", "+", "-", "*", "/", "%", "~", "?",
}
