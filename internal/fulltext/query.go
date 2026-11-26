package fulltext

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// A search query (the second argument of search()):
//
//	ann smith          both words
//	"ann smith"        the phrase: the words next to each other, in order
//	-smith             not containing the word
//	smi*               a word starting with smi
//	ann OR anna        either (OR binds tighter than the implicit AND, so
//	                   `smith ann OR anna` is smith AND (ann OR anna))
//
// Words in a query are tokenized like the text, so a query word that
// splits into several words ("e-mail", or 東京) is a phrase. A query
// needs at least one word to find: one made only of exclusions can't be
// answered from an index.

// Query is a parsed search query: every clause must match.
type Query struct {
	Clauses []Clause
}

// Clause matches when any of its items does, or, for Not, when none does.
type Clause struct {
	Not bool
	Any []Item
}

// Item is a word, a word prefix, or a phrase (several words).
type Item struct {
	Words  []string // folded
	Prefix bool     // the single word is a prefix
}

// ErrQuery reports a malformed query.
var ErrQuery = errors.New("search query")

func qerr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrQuery, fmt.Sprintf(format, args...))
}

// Parse parses a search query.
func Parse(q string) (*Query, error) {
	type lexeme struct {
		text   string
		quoted bool
		not    bool
		or     bool
	}
	var lx []lexeme
	rs := []rune(q)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
			continue
		}
		not := false
		if r == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) {
			not = true
			i++
		}
		if i < len(rs) && rs[i] == '"' {
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if j == len(rs) {
				return nil, qerr(`a phrase is missing its closing "`)
			}
			lx = append(lx, lexeme{text: string(rs[i+1 : j]), quoted: true, not: not})
			i = j + 1
			continue
		}
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
			j++
		}
		w := string(rs[i:j])
		i = j
		if w == "OR" && !not {
			lx = append(lx, lexeme{or: true})
			continue
		}
		lx = append(lx, lexeme{text: w, not: not})
	}

	item := func(l lexeme) (Item, bool, error) {
		text := l.text
		prefix := false
		if !l.quoted && strings.HasSuffix(text, "*") {
			text = strings.TrimRight(text, "*")
			prefix = true
		}
		toks := Tokenize(text)
		if len(toks) == 0 {
			if prefix {
				return Item{}, false, qerr("* needs letters before it")
			}
			return Item{}, false, nil // punctuation only: nothing to find
		}
		it := Item{Prefix: prefix}
		for _, t := range toks {
			it.Words = append(it.Words, t.Word)
		}
		if prefix && len(it.Words) > 1 {
			return Item{}, false, qerr("* works on a single word, not %q", l.text)
		}
		return it, true, nil
	}

	query := &Query{}
	for i := 0; i < len(lx); i++ {
		if lx[i].or {
			return nil, qerr("OR needs a word on each side")
		}
		c := Clause{Not: lx[i].not}
		it, ok, err := item(lx[i])
		if err != nil {
			return nil, err
		}
		if ok {
			c.Any = append(c.Any, it)
		}
		for i+1 < len(lx) && lx[i+1].or {
			if i+2 >= len(lx) || lx[i+2].or {
				return nil, qerr("OR needs a word on each side")
			}
			if c.Not || lx[i+2].not {
				return nil, qerr("- can't be used with OR")
			}
			it, ok, err := item(lx[i+2])
			if err != nil {
				return nil, err
			}
			if ok {
				c.Any = append(c.Any, it)
			}
			i += 2
		}
		if len(c.Any) > 0 {
			query.Clauses = append(query.Clauses, c)
		}
	}
	for _, c := range query.Clauses {
		if !c.Not {
			return query, nil
		}
	}
	return nil, qerr("the query has no words to find")
}

// String formats the query in the syntax Parse reads, with folded words.
func (q *Query) String() string {
	var cs []string
	for _, c := range q.Clauses {
		var is []string
		for _, it := range c.Any {
			s := strings.Join(it.Words, " ")
			switch {
			case len(it.Words) > 1:
				s = `"` + s + `"`
			case it.Prefix:
				s += "*"
			}
			is = append(is, s)
		}
		s := strings.Join(is, " OR ")
		if c.Not {
			s = "-" + s
		}
		cs = append(cs, s)
	}
	return strings.Join(cs, " ")
}

// Doc is a text prepared for matching: each word's positions.
type Doc map[string][]int

// NewDoc tokenizes a text for matching.
func NewDoc(text string) Doc {
	d := Doc{}
	for _, t := range Tokenize(text) {
		d[t.Word] = append(d[t.Word], t.Pos)
	}
	return d
}

// Match reports whether a text matches the query.
func (q *Query) Match(text string) bool { return q.MatchDoc(NewDoc(text)) }

// MatchDoc reports whether a prepared text matches the query.
func (q *Query) MatchDoc(d Doc) bool {
	for _, c := range q.Clauses {
		any := false
		for _, it := range c.Any {
			if it.match(d) {
				any = true
				break
			}
		}
		if any == c.Not {
			return false
		}
	}
	return true
}

func (it Item) match(d Doc) bool {
	if it.Prefix {
		for w := range d {
			if strings.HasPrefix(w, it.Words[0]) {
				return true
			}
		}
		return false
	}
	first := d[it.Words[0]]
	for _, p := range first {
		ok := true
		for i, w := range it.Words[1:] {
			if !hasPos(d[w], p+i+1) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func hasPos(ps []int, p int) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}
