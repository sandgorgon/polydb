package value

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on types (spec/types.md §4.5, §7, §14).
const (
	MaxNesting         = 32
	MaxDecimalDigits   = 1000
	MaxCollectionCount = 1<<32 - 1
)

// ParseType parses a type expression (spec/types.md §2), for example
// "map<text, list<int>>?" or "record{ x: float, y: float }". The result is
// validated (see Type.Validate).
func ParseType(s string) (*Type, error) {
	p := &typeParser{src: s}
	p.next()
	t, err := p.parseType()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tkEOF {
		return nil, p.errorf("unexpected %s after type", p.tok)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// MustParseType is ParseType for types known to be valid, such as those in
// tests and built-in definitions. It panics on error.
func MustParseType(s string) *Type {
	t, err := ParseType(s)
	if err != nil {
		panic(err)
	}
	return t
}

// Validate checks the rules a type expression must satisfy beyond its
// syntax: positive array and ring sizes, valid decimal precision, keyable
// set elements and map keys, unique record field names, valid names and the
// nesting limit.
func (t *Type) Validate() error { return t.validate(1) }

func (t *Type) validate(depth int) error {
	if depth > MaxNesting {
		return fmt.Errorf("type nested more than %d levels", MaxNesting)
	}
	switch t.Code {
	case CodeNull:
		return errors.New("null is a value, not a type; use T? for an optional type")
	case CodeDecimal:
		if t.Precision == 0 && t.Scale == 0 {
			return nil
		}
		if t.Precision < 1 || t.Precision > MaxDecimalDigits {
			return fmt.Errorf("decimal precision %d out of range 1..%d", t.Precision, MaxDecimalDigits)
		}
		if t.Scale < 0 || t.Scale > t.Precision {
			return fmt.Errorf("decimal scale %d out of range 0..%d", t.Scale, t.Precision)
		}
	case CodeRef:
		if t.Name != "" {
			return ValidateName("kind", t.Name)
		}
	case CodeDoc:
		return ValidateName("format", t.Name)
	case CodeList, CodeSet, CodeArray, CodeFifo, CodeLifo, CodeRing, CodePriority:
		if t.Elem == nil {
			return fmt.Errorf("%s needs an element type", t.Code)
		}
		if err := t.Elem.validate(depth + 1); err != nil {
			return err
		}
		switch t.Code {
		case CodeSet:
			if !t.Elem.Keyable() {
				return fmt.Errorf("set element type %s is not keyable", t.Elem)
			}
		case CodeArray, CodeRing:
			if t.N < 1 || t.N > MaxCollectionCount {
				return fmt.Errorf("%s size %d out of range 1..%d", t.Code, t.N, MaxCollectionCount)
			}
		default:
			if t.N < 0 || t.N > MaxCollectionCount {
				return fmt.Errorf("%s max %d out of range", t.Code, t.N)
			}
		}
	case CodeMap, CodePair:
		if t.Key == nil || t.Elem == nil {
			return fmt.Errorf("%s needs key and value types", t.Code)
		}
		if err := t.Key.validate(depth + 1); err != nil {
			return err
		}
		if err := t.Elem.validate(depth + 1); err != nil {
			return err
		}
		if t.Code == CodeMap && !t.Key.Keyable() {
			return fmt.Errorf("map key type %s is not keyable", t.Key)
		}
	case CodeRecord:
		if len(t.Fields) == 0 {
			return errors.New("record needs at least one field")
		}
		seen := make(map[string]bool, len(t.Fields))
		for _, f := range t.Fields {
			if err := ValidateName("record field", f.Name); err != nil {
				return err
			}
			if seen[f.Name] {
				return fmt.Errorf("duplicate record field %q", f.Name)
			}
			seen[f.Name] = true
			if err := f.Type.validate(depth + 1); err != nil {
				return err
			}
		}
	case CodeUnion:
		if len(t.Alts) < 2 {
			return errors.New("union needs at least two alternatives")
		}
		for i, a := range t.Alts {
			if a.Code == CodeAny || a.Code == CodeUnion {
				return fmt.Errorf("union alternative %s not allowed", a)
			}
			if a.Optional {
				return fmt.Errorf("union alternative %s may not be optional; make the union optional instead", a)
			}
			if err := a.validate(depth + 1); err != nil {
				return err
			}
			for _, b := range t.Alts[:i] {
				if a.Equal(b) {
					return fmt.Errorf("duplicate union alternative %s", a)
				}
			}
		}
	}
	return nil
}

// --- lexer ---

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkInt
	tkString
	tkPunct
	tkError
)

type token struct {
	kind tokKind
	text string // identifier, punctuation or decoded string
	pos  int
}

func (t token) String() string {
	switch t.kind {
	case tkEOF:
		return "end of input"
	case tkString:
		return strconv.Quote(t.text)
	}
	return "\"" + t.text + "\""
}

type typeParser struct {
	src string
	pos int
	tok token
}

func (p *typeParser) errorf(format string, args ...any) error {
	return fmt.Errorf("type %q, position %d: %s", p.src, p.tok.pos+1, fmt.Sprintf(format, args...))
}

func (p *typeParser) next() {
	for p.pos < len(p.src) {
		r, n := utf8.DecodeRuneInString(p.src[p.pos:])
		if !unicode.IsSpace(r) {
			break
		}
		p.pos += n
	}
	start := p.pos
	if p.pos >= len(p.src) {
		p.tok = token{kind: tkEOF, pos: start}
		return
	}
	r, n := utf8.DecodeRuneInString(p.src[p.pos:])
	switch {
	case r == '_' || unicode.IsLetter(r):
		p.pos += n
		for p.pos < len(p.src) {
			r, n = utf8.DecodeRuneInString(p.src[p.pos:])
			if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			p.pos += n
		}
		p.tok = token{kind: tkIdent, text: p.src[start:p.pos], pos: start}
	case r >= '0' && r <= '9':
		for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '_') {
			p.pos++
		}
		p.tok = token{kind: tkInt, text: strings.ReplaceAll(p.src[start:p.pos], "_", ""), pos: start}
	case r == '`':
		end := strings.IndexByte(p.src[p.pos+1:], '`')
		if end < 0 {
			p.tok = token{kind: tkError, text: "unterminated backtick name", pos: start}
			return
		}
		p.tok = token{kind: tkIdent, text: p.src[p.pos+1 : p.pos+1+end], pos: start}
		p.pos += end + 2
	case r == '"':
		s, n, err := scanQuoted(p.src[p.pos:])
		if err != nil {
			p.tok = token{kind: tkError, text: err.Error(), pos: start}
			return
		}
		p.pos += n
		p.tok = token{kind: tkString, text: s, pos: start}
	default:
		p.pos += n
		p.tok = token{kind: tkPunct, text: string(r), pos: start}
	}
}

// scanQuoted decodes a double-quoted string with Rhizome escapes at the
// start of s, returning the decoded text and the number of bytes consumed.
func scanQuoted(s string) (string, int, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(s) {
				return "", 0, errors.New("unterminated string")
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'u':
				if i+1 >= len(s) || s[i+1] != '{' {
					return "", 0, errors.New(`\u must be followed by {hex}`)
				}
				end := strings.IndexByte(s[i:], '}')
				if end < 0 {
					return "", 0, errors.New(`unterminated \u{...} escape`)
				}
				v, err := strconv.ParseUint(s[i+2:i+end], 16, 32)
				if err != nil || v > unicode.MaxRune || (v >= 0xD800 && v <= 0xDFFF) {
					return "", 0, fmt.Errorf(`invalid \u{%s} escape`, s[i+2:i+end])
				}
				b.WriteRune(rune(v))
				i += end
			default:
				return "", 0, fmt.Errorf(`unknown escape \%c`, s[i])
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, errors.New("unterminated string")
}

// --- parser ---

func (p *typeParser) expect(punct string) error {
	if p.tok.kind != tkPunct || p.tok.text != punct {
		return p.errorf("expected %q, found %s", punct, p.tok)
	}
	p.next()
	return nil
}

func (p *typeParser) isPunct(punct string) bool {
	return p.tok.kind == tkPunct && p.tok.text == punct
}

func (p *typeParser) isWord(w string) bool {
	return p.tok.kind == tkIdent && p.tok.text == w
}

func (p *typeParser) parseInt() (int, error) {
	if p.tok.kind != tkInt {
		return 0, p.errorf("expected a number, found %s", p.tok)
	}
	n, err := strconv.Atoi(p.tok.text)
	if err != nil || n > MaxCollectionCount {
		return 0, p.errorf("number %s too large", p.tok.text)
	}
	p.next()
	return n, nil
}

func (p *typeParser) parseName() (string, error) {
	if p.tok.kind != tkIdent {
		return "", p.errorf("expected a name, found %s", p.tok)
	}
	s := p.tok.text
	p.next()
	return s, nil
}

func (p *typeParser) parseType() (*Type, error) {
	t, err := p.parseBase()
	if err != nil {
		return nil, err
	}
	if p.isPunct("?") {
		p.next()
		if t.Code == CodeAny {
			return nil, p.errorf("any already accepts null; any? is not allowed")
		}
		t.Optional = true
	}
	return t, nil
}

var simpleTypes = map[string]Code{
	"bool": CodeBool, "int": CodeInt, "bigint": CodeBigInt, "float": CodeFloat,
	"text": CodeText, "bytes": CodeBytes, "time": CodeTime, "date": CodeDate,
	"duration": CodeDuration, "id": CodeID, "image": CodeImage, "any": CodeAny,
}

func (p *typeParser) parseBase() (*Type, error) {
	if p.tok.kind == tkError {
		return nil, p.errorf("%s", p.tok.text)
	}
	if p.tok.kind != tkIdent {
		return nil, p.errorf("expected a type, found %s", p.tok)
	}
	word := p.tok.text
	p.next()
	if c, ok := simpleTypes[word]; ok {
		return &Type{Code: c}, nil
	}
	switch word {
	case "decimal":
		t := &Type{Code: CodeDecimal}
		if p.isPunct("(") {
			p.next()
			var err error
			if t.Precision, err = p.parseInt(); err != nil {
				return nil, err
			}
			if err := p.expect(","); err != nil {
				return nil, err
			}
			if t.Scale, err = p.parseInt(); err != nil {
				return nil, err
			}
			if t.Precision == 0 {
				return nil, p.errorf("decimal precision must be at least 1")
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		return t, nil
	case "ref":
		t := &Type{Code: CodeRef}
		if p.isPunct("<") {
			p.next()
			var err error
			if t.Name, err = p.parseName(); err != nil {
				return nil, err
			}
			if err := p.expect(">"); err != nil {
				return nil, err
			}
		}
		return t, nil
	case "doc":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		return DocOf(name), p.expect(">")
	case "list", "set":
		elem, err := p.parseOneArg()
		if err != nil {
			return nil, err
		}
		if word == "list" {
			return ListOf(elem), nil
		}
		return SetOf(elem), nil
	case "array":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		elem, err := p.parseType()
		if err != nil {
			return nil, err
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
		n, err := p.parseInt()
		if err != nil {
			return nil, err
		}
		return ArrayOf(elem, n), p.expect(">")
	case "map", "pair":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		k, err := p.parseType()
		if err != nil {
			return nil, err
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
		v, err := p.parseType()
		if err != nil {
			return nil, err
		}
		if err := p.expect(">"); err != nil {
			return nil, err
		}
		if word == "map" {
			return MapOf(k, v), nil
		}
		return PairOf(k, v), nil
	case "queue":
		return p.parseQueue()
	case "record":
		return p.parseRecord()
	case "union":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		t := &Type{Code: CodeUnion}
		for {
			a, err := p.parseType()
			if err != nil {
				return nil, err
			}
			t.Alts = append(t.Alts, a)
			if !p.isPunct("|") {
				break
			}
			p.next()
		}
		return t, p.expect(">")
	}
	return nil, p.errorf("unknown type %q", word)
}

func (p *typeParser) parseOneArg() (*Type, error) {
	if err := p.expect("<"); err != nil {
		return nil, err
	}
	t, err := p.parseType()
	if err != nil {
		return nil, err
	}
	return t, p.expect(">")
}

func (p *typeParser) parseQueue() (*Type, error) {
	if err := p.expect("."); err != nil {
		return nil, err
	}
	kind, err := p.parseName()
	if err != nil {
		return nil, err
	}
	var code Code
	switch kind {
	case "fifo":
		code = CodeFifo
	case "lifo":
		code = CodeLifo
	case "ring":
		code = CodeRing
	case "priority":
		code = CodePriority
	default:
		return nil, p.errorf("unknown queue kind %q (want fifo, lifo, ring or priority)", kind)
	}
	if err := p.expect("<"); err != nil {
		return nil, err
	}
	elem, err := p.parseType()
	if err != nil {
		return nil, err
	}
	t := &Type{Code: code, Elem: elem}
	if code == CodeRing {
		if err := p.expect(","); err != nil {
			return nil, err
		}
		if t.N, err = p.parseInt(); err != nil {
			return nil, err
		}
		return t, p.expect(">")
	}
	if code == CodePriority && p.isPunct(",") {
		save := *p
		p.next()
		switch {
		case p.isWord("high"):
			p.next()
		case p.isWord("low"):
			t.LowFirst = true
			p.next()
		default:
			*p = save // not a direction; maybe ", max N"
		}
	}
	if p.isPunct(",") {
		p.next()
		if !p.isWord("max") {
			return nil, p.errorf("expected max, found %s", p.tok)
		}
		p.next()
		if t.N, err = p.parseInt(); err != nil {
			return nil, err
		}
		if t.N == 0 {
			return nil, p.errorf("queue max must be at least 1")
		}
	}
	return t, p.expect(">")
}

func (p *typeParser) parseRecord() (*Type, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	t := &Type{Code: CodeRecord}
	for !p.isPunct("}") {
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		ft, err := p.parseType()
		if err != nil {
			return nil, err
		}
		f := RecordField{Name: name, Type: ft}
		if p.isWord("doc") {
			p.next()
		}
		if p.tok.kind == tkString {
			f.Doc = p.tok.text
			p.next()
		}
		t.Fields = append(t.Fields, f)
		if p.isPunct(",") {
			p.next()
		}
	}
	p.next()
	return t, nil
}
