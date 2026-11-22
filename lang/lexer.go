package lang

import (
	"encoding/base64"
	"encoding/hex"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/sandgorgon/polydb/value"
)

// Lex splits src into tokens, ending with an EOF token. The source is
// NFC-normalized first (spec/rhizome.md §1.1), and positions refer to the
// normalized text.
func Lex(src string) ([]Token, error) {
	if !utf8.ValidString(src) {
		return nil, syntaxErr(Pos{Line: 1, Col: 1}, "source is not valid UTF-8")
	}
	l := &lexer{src: norm.NFC.String(src), line: 1, col: 1}
	var toks []Token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.Kind == EOF {
			return toks, nil
		}
	}
}

type lexer struct {
	src       string
	off       int
	line, col int
}

func (l *lexer) pos() Pos { return Pos{Offset: l.off, Line: l.line, Col: l.col} }

func (l *lexer) peek() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.off:])
	return r
}

func (l *lexer) peekAt(n int) rune { // the rune n bytes ahead (ASCII lookahead only)
	if l.off+n >= len(l.src) {
		return -1
	}
	return rune(l.src[l.off+n])
}

func (l *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// skip advances past the next n bytes, which must end on a rune boundary.
func (l *lexer) skip(n int) {
	for end := l.off + n; l.off < end; {
		l.advance()
	}
}

func (l *lexer) rest() string { return l.src[l.off:] }

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return isIdentStart(r) || unicode.IsDigit(r) }

// skipSpace skips whitespace and comments.
func (l *lexer) skipSpace() error {
	for {
		switch r := l.peek(); {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			l.advance()
		case strings.HasPrefix(l.rest(), "//"):
			for l.peek() != '\n' && l.peek() != -1 {
				l.advance()
			}
		case strings.HasPrefix(l.rest(), "/*"):
			start := l.pos()
			l.skip(2)
			for !strings.HasPrefix(l.rest(), "*/") {
				if l.peek() == -1 {
					return syntaxErr(start, "unterminated /* comment")
				}
				l.advance()
			}
			l.skip(2)
		default:
			return nil
		}
	}
}

func (l *lexer) next() (Token, error) {
	if err := l.skipSpace(); err != nil {
		return Token{}, err
	}
	start := l.pos()
	tok := func(kind TokKind, text string) (Token, error) {
		return Token{Kind: kind, Text: text, Pos: start, End: l.off}, nil
	}
	lit := func(v value.Value, err error) (Token, error) {
		if err != nil {
			return Token{}, err
		}
		return Token{Kind: Literal, Val: v, Pos: start, End: l.off}, nil
	}
	r := l.peek()
	switch {
	case r == -1:
		return tok(EOF, "")
	case unicode.IsDigit(r):
		return lit(l.number(start))
	case r == '"':
		return lit(l.text(start))
	case r == '`':
		name, err := l.backtick(start)
		if err != nil {
			return Token{}, err
		}
		return Token{Kind: Ident, Text: name, Quoted: true, Pos: start, End: l.off}, nil
	case r == '@':
		l.advance()
		if l.peek() == '@' {
			return Token{}, syntaxErr(start, "@@ is reserved for persistent names")
		}
		name, err := l.name(start, "handle")
		if err != nil {
			return Token{}, err
		}
		return tok(Handle, name)
	case r == '$':
		l.advance()
		name, err := l.name(start, "parameter")
		if err != nil {
			return Token{}, err
		}
		return tok(Param, name)
	case r == '#':
		return lit(l.id(start))
	case isIdentStart(r):
		if v, ok, err := l.prefixed(start); ok || err != nil {
			return lit(v, err)
		}
		for isIdentPart(l.peek()) {
			l.advance()
		}
		word := l.src[start.Offset:l.off]
		if utf8.RuneCountInString(word) > value.MaxNameLen {
			return Token{}, syntaxErr(start, "name longer than %d characters", value.MaxNameLen)
		}
		if Keywords[word] {
			return tok(Keyword, word)
		}
		return tok(Ident, word)
	}
	for _, p := range puncts {
		if strings.HasPrefix(l.rest(), p) {
			l.skip(len(p))
			return tok(Punct, p)
		}
	}
	return Token{}, syntaxErr(start, "unexpected character %q", r)
}

// name reads an identifier after @ or $.
func (l *lexer) name(start Pos, what string) (string, error) {
	if l.peek() == '`' {
		return l.backtick(start)
	}
	if !isIdentStart(l.peek()) {
		return "", syntaxErr(start, "expected a name after the %s prefix", what)
	}
	s := l.off
	for isIdentPart(l.peek()) {
		l.advance()
	}
	return l.src[s:l.off], nil
}

func (l *lexer) backtick(start Pos) (string, error) {
	l.advance()
	s := l.off
	for l.peek() != '`' {
		if l.peek() == -1 {
			return "", syntaxErr(start, "unterminated `quoted name`")
		}
		l.advance()
	}
	name := l.src[s:l.off]
	l.advance()
	if name == "" {
		return "", syntaxErr(start, "empty `quoted name`")
	}
	if utf8.RuneCountInString(name) > value.MaxNameLen {
		return "", syntaxErr(start, "name longer than %d characters", value.MaxNameLen)
	}
	return name, nil
}

// prefixed lexes typed literals: r`raw`, x"hex", b64"…", t"time", d"date".
// The prefix only counts when a quote follows immediately.
func (l *lexer) prefixed(start Pos) (value.Value, bool, error) {
	rest := l.rest()
	switch {
	case strings.HasPrefix(rest, "r`"):
		l.skip(1)
		s, err := l.backtick(start)
		if err != nil {
			return nil, true, syntaxErr(start, "unterminated raw text")
		}
		t, err := value.NewText(s)
		return t, true, l.wrap(start, err)
	case strings.HasPrefix(rest, `x"`):
		l.skip(1)
		s, err := l.quoted(start)
		if err != nil {
			return nil, true, err
		}
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, true, syntaxErr(start, "invalid hex in bytes literal")
		}
		v, err := value.NewBytes(b, "")
		return v, true, l.wrap(start, err)
	case strings.HasPrefix(rest, `b64"`):
		l.skip(3)
		s, err := l.quoted(start)
		if err != nil {
			return nil, true, err
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, true, syntaxErr(start, "invalid base64 in bytes literal")
		}
		v, err := value.NewBytes(b, "")
		return v, true, l.wrap(start, err)
	case strings.HasPrefix(rest, `t"`):
		l.skip(1)
		s, err := l.quoted(start)
		if err != nil {
			return nil, true, err
		}
		v, err := value.ParseTime(s)
		return v, true, l.wrap(start, err)
	case strings.HasPrefix(rest, `d"`):
		l.skip(1)
		s, err := l.quoted(start)
		if err != nil {
			return nil, true, err
		}
		v, err := value.ParseDate(s)
		return v, true, l.wrap(start, err)
	}
	return nil, false, nil
}

func (l *lexer) wrap(p Pos, err error) error {
	if err == nil {
		return nil
	}
	return syntaxErr(p, "%v", err)
}

// quoted reads "…" without escapes (for typed literals).
func (l *lexer) quoted(start Pos) (string, error) {
	l.advance()
	s := l.off
	for l.peek() != '"' {
		if l.peek() == -1 || l.peek() == '\n' {
			return "", syntaxErr(start, "unterminated literal")
		}
		l.advance()
	}
	v := l.src[s:l.off]
	l.advance()
	return v, nil
}

// text reads a "…" text literal with escapes.
func (l *lexer) text(start Pos) (value.Value, error) {
	l.advance()
	var b strings.Builder
	for {
		r := l.peek()
		switch r {
		case -1:
			return nil, syntaxErr(start, "unterminated text literal")
		case '"':
			l.advance()
			t, err := value.NewText(b.String())
			return t, l.wrap(start, err)
		case '\\':
			escPos := l.pos()
			l.advance()
			switch e := l.peek(); e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '"':
				b.WriteRune(e)
			case 'u':
				l.advance()
				if l.peek() != '{' {
					return nil, syntaxErr(escPos, `expected \u{hex}`)
				}
				l.advance()
				s := l.off
				for l.peek() != '}' {
					if l.peek() == -1 || l.off-s > 6 {
						return nil, syntaxErr(escPos, `unterminated \u{…} escape`)
					}
					l.advance()
				}
				n, err := strconv.ParseUint(l.src[s:l.off], 16, 32)
				if err != nil || n > unicode.MaxRune || (n >= 0xD800 && n < 0xE000) {
					return nil, syntaxErr(escPos, `invalid code point in \u{…} escape`)
				}
				b.WriteRune(rune(n))
			default:
				return nil, syntaxErr(escPos, `unknown escape \%c; use \n \t \r \\ \" or \u{…}`, e)
			}
			l.advance()
		default:
			b.WriteRune(l.advance())
		}
	}
}

// id reads #ID.
func (l *lexer) id(start Pos) (value.Value, error) {
	l.advance()
	s := l.off
	for isIdentPart(l.peek()) {
		l.advance()
	}
	id, err := value.ParseID(l.src[s:l.off])
	if err != nil {
		return nil, syntaxErr(start, "%v", err)
	}
	return id, nil
}

// digits reads digits with single underscores between them, using valid
// to accept a digit, and returns them without underscores.
func (l *lexer) digits(start Pos, valid func(rune) bool) (string, error) {
	var b strings.Builder
	for {
		r := l.peek()
		if valid(r) {
			b.WriteRune(l.advance())
			continue
		}
		if r == '_' && b.Len() > 0 && l.off+1 < len(l.src) && valid(rune(l.src[l.off+1])) {
			l.advance()
			continue
		}
		break
	}
	if b.Len() == 0 {
		return "", syntaxErr(start, "expected digits")
	}
	return b.String(), nil
}

func isDec(r rune) bool { return r >= '0' && r <= '9' }
func isHex(r rune) bool { return isDec(r) || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' }
func isBin(r rune) bool { return r == '0' || r == '1' }

// durationUnits in matching order (longer first).
var durationUnits = []struct {
	name string
	d    time.Duration
}{{"ns", time.Nanosecond}, {"us", time.Microsecond}, {"µs", time.Microsecond}, {"ms", time.Millisecond},
	{"s", time.Second}, {"m", time.Minute}, {"h", time.Hour}}

func (l *lexer) unit() (time.Duration, bool) {
	for _, u := range durationUnits {
		if strings.HasPrefix(l.rest(), u.name) {
			l.skip(len(u.name))
			return u.d, true
		}
	}
	return 0, false
}

// number reads any numeric literal: int, bigint, float, decimal or
// duration.
func (l *lexer) number(start Pos) (value.Value, error) {
	if l.peek() == '0' && (l.peekAt(1) == 'x' || l.peekAt(1) == 'b') && l.peekAt(2) != -1 {
		base, valid := 16, isHex
		if l.peekAt(1) == 'b' {
			base, valid = 2, isBin
		}
		if valid(l.peekAt(2)) {
			l.skip(2)
			ds, err := l.digits(start, valid)
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseInt(ds, base, 64)
			if err != nil {
				return nil, syntaxErr(start, "integer literal out of range; use a bigint (n suffix)")
			}
			return value.Int(n), l.noSuffix(start)
		}
	}
	intPart, err := l.digits(start, isDec)
	if err != nil {
		return nil, err
	}
	frac := ""
	if l.peek() == '.' && isDec(l.peekAt(1)) {
		l.advance()
		if frac, err = l.digits(start, isDec); err != nil {
			return nil, err
		}
	}
	// Duration: one or more number+unit parts.
	if d, ok := l.unit(); ok {
		total, err := durationPart(start, intPart, frac, d)
		if err != nil {
			return nil, err
		}
		for isDec(l.peek()) {
			ip, err := l.digits(start, isDec)
			if err != nil {
				return nil, err
			}
			fp := ""
			if l.peek() == '.' && isDec(l.peekAt(1)) {
				l.advance()
				if fp, err = l.digits(start, isDec); err != nil {
					return nil, err
				}
			}
			u, ok := l.unit()
			if !ok {
				return nil, syntaxErr(start, "duration part without a unit (ns, us, ms, s, m, h)")
			}
			part, err := durationPart(start, ip, fp, u)
			if err != nil {
				return nil, err
			}
			if total > math.MaxInt64-part {
				return nil, syntaxErr(start, "duration out of range")
			}
			total += part
		}
		return value.Duration(total), l.noSuffix(start)
	}
	switch l.peek() {
	case 'n':
		if frac != "" {
			return nil, syntaxErr(start, "a bigint can't have a fraction")
		}
		l.advance()
		v, err := value.ParseBigInt(intPart)
		if err != nil {
			return nil, l.wrap(start, err)
		}
		return v, l.noSuffix(start)
	case 'd':
		l.advance()
		s := intPart
		if frac != "" {
			s += "." + frac
		}
		v, err := value.ParseDecimal(s)
		if err != nil {
			return nil, l.wrap(start, err)
		}
		return v, l.noSuffix(start)
	}
	exp := ""
	if r := l.peek(); r == 'e' || r == 'E' {
		save := *l
		l.advance()
		sign := ""
		if l.peek() == '+' || l.peek() == '-' {
			sign = string(l.advance())
		}
		if isDec(l.peek()) {
			ds, _ := l.digits(start, isDec)
			exp = "e" + sign + ds
		} else {
			*l = save // not an exponent; the e belongs to something else
		}
	}
	if frac == "" && exp == "" {
		n, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return nil, syntaxErr(start, "integer literal out of range; use a bigint (n suffix)")
		}
		return value.Int(n), l.noSuffix(start)
	}
	s := intPart
	if frac != "" {
		s += "." + frac
	}
	f, err := strconv.ParseFloat(s+exp, 64)
	if err != nil {
		return nil, syntaxErr(start, "float literal out of range")
	}
	return value.NewFloat(f), l.noSuffix(start)
}

// noSuffix rejects a number followed directly by letters, like 5min.
func (l *lexer) noSuffix(start Pos) error {
	if isIdentPart(l.peek()) {
		return syntaxErr(start, "invalid number suffix %q", l.peek())
	}
	return nil
}

// durationPart converts digits.frac × unit to nanoseconds exactly.
func durationPart(start Pos, intPart, frac string, unit time.Duration) (time.Duration, error) {
	n, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return 0, syntaxErr(start, "invalid duration")
	}
	n.Mul(n, big.NewInt(int64(unit)))
	n.Quo(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil))
	if !n.IsInt64() {
		return 0, syntaxErr(start, "duration out of range")
	}
	return time.Duration(n.Int64()), nil
}
