package lang

import (
	"slices"
	"strings"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// ParseScript parses statements separated by ";". The final ";" may be
// left out.
func ParseScript(src string) ([]Stmt, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	var out []Stmt
	for {
		for p.accept(";") {
		}
		if p.tok().Kind == EOF {
			return out, nil
		}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if p.tok().Kind != EOF {
			if err := p.expect(";"); err != nil {
				return nil, err
			}
		}
	}
}

// ParseStatement parses exactly one statement, with or without a final
// ";".
func ParseStatement(src string) (Stmt, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	s, err := p.statement()
	if err != nil {
		return nil, err
	}
	p.accept(";")
	if t := p.tok(); t.Kind != EOF {
		return nil, syntaxErr(t.Pos, "expected the end of the statement, found %s", t)
	}
	return s, nil
}

// ParseExpr parses a single expression, as used in check and default
// clauses.
func ParseExpr(src string) (Expr, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	if t := p.tok(); t.Kind != EOF {
		return nil, syntaxErr(t.Pos, "expected the end of the expression, found %s", t)
	}
	return e, nil
}

type parser struct {
	toks []Token
	i    int
}

func newParser(src string) (*parser, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	return &parser{toks: toks}, nil
}

func (p *parser) tok() Token          { return p.toks[p.i] }
func (p *parser) peekTok(n int) Token { return p.toks[min(p.i+n, len(p.toks)-1)] }
func (p *parser) pos() Pos            { return p.tok().Pos }
func (p *parser) at() At              { return At{p.pos()} }

func (p *parser) advance() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

// accept consumes the keyword or symbol s if it is next.
func (p *parser) accept(s string) bool {
	if p.tok().Is(s) {
		p.advance()
		return true
	}
	return false
}

// acceptWord consumes the unquoted word w (keyword or contextual) if next.
func (p *parser) acceptWord(w string) bool {
	if p.tok().IsWord(w) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) errExpected(what string) error {
	return syntaxErr(p.pos(), "expected %s, found %s", what, p.tok())
}

func (p *parser) expect(s string) error {
	if !p.accept(s) {
		return p.errExpected(quoteSym(s))
	}
	return nil
}

func (p *parser) expectWord(w string) error {
	if !p.acceptWord(w) {
		return p.errExpected(quoteSym(w))
	}
	return nil
}

func quoteSym(s string) string { return "\"" + s + "\"" }

// name reads an identifier. A keyword in its place gets a hint about
// backticks.
func (p *parser) name(what string) (string, error) {
	t := p.tok()
	switch t.Kind {
	case Ident:
		p.advance()
		return t.Text, nil
	case Keyword:
		return "", syntaxErr(t.Pos, "expected %s, found the keyword %s; write `%s` to use it as a name", what, t.Text, t.Text)
	}
	return "", p.errExpected(what)
}

// metaName reads a metadata name after "::". Metadata names are a fixed
// set, some of which are keywords (::kind, ::doc, ::from, ::to), so
// keywords are accepted here.
func (p *parser) metaName() (string, error) {
	if t := p.tok(); t.Kind == Keyword {
		p.advance()
		return t.Text, nil
	}
	return p.name("a metadata name")
}

func (p *parser) handle() (string, error) {
	t := p.tok()
	if t.Kind != Handle {
		return "", p.errExpected("a handle (@name)")
	}
	p.advance()
	return t.Text, nil
}

func (p *parser) textLit(what string) (string, error) {
	t := p.tok()
	if t.Kind == Literal {
		if s, ok := t.Val.(value.Text); ok {
			p.advance()
			return s.Go(), nil
		}
	}
	return "", p.errExpected(what)
}

func (p *parser) intLit(what string) (int, error) {
	t := p.tok()
	if t.Kind == Literal {
		if n, ok := t.Val.(value.Int); ok && n >= 0 && n <= 1<<31-1 {
			p.advance()
			return int(n), nil
		}
	}
	return 0, p.errExpected(what)
}

// optDoc reads an optional doc string: ["doc"] text.
func (p *parser) optDoc(bare bool) (*string, error) {
	if p.acceptWord("doc") {
		s, err := p.textLit("a doc string")
		return &s, err
	}
	if bare && p.tok().Kind == Literal {
		if s, ok := p.tok().Val.(value.Text); ok {
			p.advance()
			str := s.Go()
			return &str, nil
		}
	}
	return nil, nil
}

// statementWords are the words a statement can start with, for "did you
// mean" suggestions.
var statementWords = []string{"create", "drop", "use", "set", "define", "alter", "update", "delete", "relate",
	"unrelate", "push", "pop", "find", "begin", "commit", "rollback", "grant", "revoke", "describe", "show",
	"explain", "history"}

// ===== statements =====

func (p *parser) statement() (Stmt, error) {
	t := p.tok()
	if t.Kind != Keyword {
		if t.Kind == Ident && !t.Quoted {
			if Keywords[strings.ToLower(t.Text)] {
				return nil, syntaxErr(t.Pos, "keywords are lowercase: did you mean %s?", strings.ToLower(t.Text))
			}
			if s := model.Suggest(t.Text, statementWords); s != "" {
				return nil, syntaxErr(t.Pos, "unknown statement %s (did you mean %s?)", t.Text, s)
			}
		}
		return nil, syntaxErr(t.Pos, "expected a statement, found %s", t)
	}
	at := p.at()
	switch t.Text {
	case "create":
		p.advance()
		switch {
		case p.accept("database"):
			n, err := p.name("a database name")
			return &CreateDatabase{at, n}, err
		case p.accept("user"):
			n, err := p.name("a user name")
			if err != nil {
				return nil, err
			}
			if err := p.expect("password"); err != nil {
				return nil, err
			}
			pw, err := p.expr()
			return &CreateUser{at, n, pw}, err
		case p.accept("role"):
			n, err := p.name("a role name")
			return &CreateRole{at, n}, err
		}
		return p.create(at)
	case "drop":
		p.advance()
		if p.accept("database") {
			n, err := p.name("a database name")
			return &DropDatabase{at, n}, err
		}
		for _, w := range []string{"kind", "relation", "format", "index", "user", "role"} {
			if p.accept(w) {
				n, err := p.name("a " + w + " name")
				return &Drop{at, w, n}, err
			}
		}
		return nil, p.errExpected("database, kind, relation, format, index, user or role")
	case "use":
		p.advance()
		n, err := p.name("a database name")
		return &Use{at, n}, err
	case "set":
		p.advance()
		n, err := p.name("a setting name")
		if err != nil {
			return nil, err
		}
		if err := p.expect("="); err != nil {
			return nil, err
		}
		v, err := p.expr()
		return &SetSetting{at, n, v}, err
	case "define":
		p.advance()
		return p.define(at)
	case "alter":
		p.advance()
		return p.alter(at)
	case "update":
		return p.update()
	case "delete":
		p.advance()
		tg, err := p.target()
		return &Delete{at, tg}, err
	case "relate":
		return p.relate()
	case "unrelate":
		return p.unrelate()
	case "push":
		return p.push()
	case "pop":
		return p.pop()
	case "find":
		return p.pipeline()
	case "begin":
		p.advance()
		ro := false
		if p.accept("read") {
			if err := p.expectWord("only"); err != nil {
				return nil, err
			}
			ro = true
		}
		return &Begin{at, ro}, nil
	case "commit":
		p.advance()
		return &Commit{at}, nil
	case "rollback":
		p.advance()
		return &Rollback{at}, nil
	case "grant", "revoke":
		return p.grant()
	case "describe":
		p.advance()
		for _, w := range []string{"kind", "relation", "format", "index"} {
			if p.accept(w) {
				n, err := p.name("a " + w + " name")
				return &Describe{At: at, What: w, Name: n}, err
			}
		}
		tg, err := p.target()
		return &Describe{At: at, Target: tg}, err
	case "show":
		p.advance()
		return p.show(at)
	case "explain":
		p.advance()
		an := p.acceptWord("analyze")
		s, err := p.statement()
		return &Explain{at, an, s}, err
	case "history":
		p.advance()
		fr, err := p.fieldRef()
		return &History{at, fr}, err
	}
	if s := model.Suggest(t.Text, statementWords); s != "" && s != t.Text {
		return nil, syntaxErr(t.Pos, "a statement can't start with %s (did you mean %s?)", t.Text, s)
	}
	return nil, syntaxErr(t.Pos, "a statement can't start with %s", t.Text)
}

func (p *parser) show(at At) (Stmt, error) {
	t := p.tok()
	if t.Kind != Ident || t.Quoted {
		return nil, p.errExpected("what to show (databases, kinds, relations, formats, indexes, users, roles, sessions, grants, keys or stats)")
	}
	s := &ShowStmt{At: at, What: t.Text}
	switch t.Text {
	case "databases", "kinds", "relations", "formats", "indexes", "users", "roles", "sessions":
		p.advance()
	case "grants", "keys":
		p.advance()
		if p.accept("for") {
			n, err := p.name("a user or role name")
			s.For = n
			return s, err
		}
	case "stats":
		p.advance()
		if p.accept("kind") {
			n, err := p.name("a kind name")
			s.For = n
			return s, err
		}
	default:
		return nil, syntaxErr(t.Pos, "can't show %s", t.Text)
	}
	return s, nil
}

// --- definitions ---

func (p *parser) define(at At) (Stmt, error) {
	switch {
	case p.accept("kind"):
		k := &DefineKind{At: at}
		var err error
		if k.Name, err = p.name("a kind name"); err != nil {
			return nil, err
		}
		once := options{}
		for {
			at := p.pos()
			if d, err := p.optDoc(true); err != nil {
				return nil, err
			} else if d != nil {
				k.Doc = d
				if err := once.use(at, "doc"); err != nil {
					return nil, err
				}
				continue
			}
			if p.acceptWord("closed") {
				k.Closed = true
				if err := once.use(at, "closed"); err != nil {
					return nil, err
				}
				continue
			}
			if p.tok().Is("history") {
				if err := once.use(at, "history"); err != nil {
					return nil, err
				}
				if k.History, err = p.historyOpt(); err != nil {
					return nil, err
				}
				continue
			}
			break
		}
		k.Fields, err = p.fieldBlock(true)
		return k, err
	case p.accept("relation"):
		r := &DefineRelation{At: at}
		var err error
		if r.Name, err = p.name("a relation name"); err != nil {
			return nil, err
		}
		if err := p.expect("from"); err != nil {
			return nil, err
		}
		if r.From, err = p.name("a kind, any or field"); err != nil {
			return nil, err
		}
		if err := p.expect("to"); err != nil {
			return nil, err
		}
		if r.To, err = p.name("a kind, any or field"); err != nil {
			return nil, err
		}
		once := options{}
		for {
			at := p.pos()
			o, ok, err := p.relOption()
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			if err := once.use(at, o.name()); err != nil {
				return nil, err
			}
			mergeRelOption(r, o)
		}
		if p.tok().Is("{") {
			r.Props, err = p.fieldBlock(true)
		}
		return r, err
	case p.accept("format"):
		f := &DefineFormat{At: at}
		var err error
		if f.Name, err = p.name("a format name"); err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		if f.Base, err = p.name("a base format"); err != nil {
			return nil, err
		}
		if f.Doc, err = p.optDoc(true); err != nil {
			return nil, err
		}
		f.Fields, err = p.fieldBlock(false)
		return f, err
	}
	ix := &DefineIndex{At: at}
	if p.accept("unique") {
		ix.Unique = true
	} else if p.tok().IsWord("text") && p.peekTok(1).Is("index") {
		p.advance()
		ix.Text = true
	}
	if !p.accept("index") {
		return nil, p.errExpected("kind, relation, format or index")
	}
	var err error
	if ix.Name, err = p.name("an index name"); err != nil {
		return nil, err
	}
	if err := p.expect("on"); err != nil {
		return nil, err
	}
	if ix.Kind, err = p.name("a kind name"); err != nil {
		return nil, err
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	for {
		var k IndexKey
		if k.Field, err = p.name("a field name"); err != nil {
			return nil, err
		}
		for p.accept("->") {
			st, err := p.docStep()
			if err != nil {
				return nil, err
			}
			k.Path = append(k.Path, st)
		}
		ix.Keys = append(ix.Keys, k)
		if !p.accept(",") {
			break
		}
	}
	return ix, p.expect(")")
}

func mergeRelOption(r *DefineRelation, o *RelOption) {
	if o.Doc != nil {
		r.Doc = o.Doc
	}
	if o.MaxOut != nil {
		r.MaxOut = o.MaxOut
	}
	if o.MaxIn != nil {
		r.MaxIn = o.MaxIn
	}
	if o.OnDelete != "" {
		r.OnDelete = o.OnDelete
	}
}

// name names the option that o sets, for duplicate checks.
func (o *RelOption) name() string {
	switch {
	case o.Doc != nil:
		return "doc"
	case o.MaxOut != nil:
		return "max out"
	case o.MaxIn != nil:
		return "max in"
	}
	return "on delete"
}

// options tracks the options a definition has used, so that each appears
// at most once (spec/rhizome.md §15.5).
type options map[string]bool

func (o options) use(at Pos, name string) error {
	if o[name] {
		return syntaxErr(at, "%s is given more than once", name)
	}
	o[name] = true
	return nil
}

// relOption reads one relation option, if present.
func (p *parser) relOption() (*RelOption, bool, error) {
	if d, err := p.optDoc(true); err != nil || d != nil {
		return &RelOption{Doc: d}, d != nil, err
	}
	if p.tok().IsWord("max") {
		p.advance()
		out := p.acceptWord("out")
		if !out && !p.acceptWord("in") && !p.accept("in") {
			return nil, false, p.errExpected("out or in")
		}
		n, err := p.intLit("a maximum")
		if err != nil {
			return nil, false, err
		}
		if out {
			return &RelOption{MaxOut: &n}, true, nil
		}
		return &RelOption{MaxIn: &n}, true, nil
	}
	if p.tok().Is("on") {
		p.advance()
		if err := p.expect("delete"); err != nil {
			return nil, false, err
		}
		switch {
		case p.acceptWord("cascade"):
			return &RelOption{OnDelete: "cascade"}, true, nil
		case p.accept("restrict"):
			return &RelOption{OnDelete: "restrict"}, true, nil
		}
		return nil, false, p.errExpected("cascade or restrict")
	}
	return nil, false, nil
}

func (p *parser) historyOpt() (*HistoryOpt, error) {
	if err := p.expect("history"); err != nil {
		return nil, err
	}
	h := &HistoryOpt{}
	if p.accept("keep") {
		var err error
		h.Keep, err = p.expr()
		return h, err
	}
	return h, nil
}

// fieldBlock reads { field_def [","] … }. full allows the kind-field
// options; otherwise only doc strings.
func (p *parser) fieldBlock(full bool) ([]*FieldDef, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []*FieldDef
	for !p.accept("}") {
		f, err := p.fieldDef(full)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
		p.accept(",")
	}
	return out, nil
}

func (p *parser) fieldDef(full bool) (*FieldDef, error) {
	f := &FieldDef{At: p.at()}
	var err error
	if f.Name, err = p.name("a field name"); err != nil {
		return nil, err
	}
	if err := p.expect(":"); err != nil {
		return nil, err
	}
	if f.Type, err = p.typ(); err != nil {
		return nil, err
	}
	once := options{}
	for {
		at := p.pos()
		if d, err := p.optDoc(true); err != nil {
			return nil, err
		} else if d != nil {
			f.Doc = d
			if err := once.use(at, "doc"); err != nil {
				return nil, err
			}
			continue
		}
		if !full {
			return f, nil
		}
		var opt string
		switch t := p.tok(); {
		case t.Is("unique"), t.Is("default"), t.Is("history"):
			opt = t.Text
		case t.IsWord("check") && !p.peekTok(1).Is(":"):
			opt = "check"
		case t.Is("on"):
			opt = "on delete"
		}
		if opt != "" {
			if err := once.use(at, opt); err != nil {
				return nil, err
			}
		}
		switch {
		case p.accept("unique"):
			f.Unique = true
		case p.accept("default"):
			if f.Default, err = p.expr(); err != nil {
				return nil, err
			}
		case p.tok().IsWord("check") && !p.peekTok(1).Is(":"):
			p.advance()
			if f.Check, err = p.expr(); err != nil {
				return nil, err
			}
		case p.tok().Is("on"):
			if f.OnDelete, err = p.onDeleteRef(); err != nil {
				return nil, err
			}
		case p.tok().Is("history"):
			if f.History, err = p.historyOpt(); err != nil {
				return nil, err
			}
		default:
			return f, nil
		}
	}
}

func (p *parser) onDeleteRef() (string, error) {
	if err := p.expect("on"); err != nil {
		return "", err
	}
	if err := p.expect("delete"); err != nil {
		return "", err
	}
	switch {
	case p.acceptWord("dangle"):
		return "dangle", nil
	case p.accept("restrict"):
		return "restrict", nil
	}
	return "", p.errExpected("dangle or restrict")
}

func (p *parser) alter(at At) (Stmt, error) {
	switch {
	case p.accept("kind"):
		a := &AlterKind{At: at}
		var err error
		if a.Name, err = p.name("a kind name"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptWord("add"):
			a.AddField, err = p.fieldDef(true)
		case p.accept("drop"):
			a.DropField, err = p.name("a field name")
		case p.accept("alter"):
			a.Alter, err = p.fieldDef(true)
		case p.acceptWord("rename"):
			if err = p.expect("to"); err == nil {
				a.RenameTo, err = p.name("a kind name")
			}
		case p.accept("set"):
			b := true
			switch {
			case p.acceptWord("closed"):
			case p.acceptWord("extensible"):
				b = false
			default:
				return nil, p.errExpected("closed or extensible")
			}
			a.SetClosed = &b
		default:
			return nil, p.errExpected("add, drop, alter, rename or set")
		}
		return a, err
	case p.accept("relation"):
		a := &AlterRelation{At: at}
		var err error
		if a.Name, err = p.name("a relation name"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptWord("add"):
			a.AddProp, err = p.fieldDef(true)
		case p.accept("drop"):
			a.DropProp, err = p.name("a property name")
		case p.accept("alter"):
			a.Alter, err = p.fieldDef(true)
		case p.acceptWord("rename"):
			if err = p.expect("to"); err == nil {
				a.RenameTo, err = p.name("a relation name")
			}
		case p.accept("set"):
			o, ok, err2 := p.relOption()
			if err2 == nil && !ok {
				err2 = p.errExpected("a relation option")
			}
			a.SetOption, err = o, err2
		default:
			return nil, p.errExpected("add, drop, alter, rename or set")
		}
		return a, err
	case p.accept("user"):
		a := &AlterUser{At: at}
		var err error
		if a.Name, err = p.name("a user name"); err != nil {
			return nil, err
		}
		switch {
		case p.accept("password"):
			a.Password, err = p.expr()
		case p.acceptWord("add"):
			if err = p.expectWord("key"); err != nil {
				return nil, err
			}
			k, err := p.textLit("a key fingerprint")
			if err != nil {
				return nil, err
			}
			a.AddKey = &k
			if p.accept("as") {
				l, err := p.textLit("a key label")
				a.KeyLabel = &l
				return a, err
			}
		case p.accept("drop"):
			if err = p.expectWord("key"); err != nil {
				return nil, err
			}
			k, err := p.textLit("a key fingerprint")
			a.DropKey = &k
			return a, err
		default:
			return nil, p.errExpected("password, add key or drop key")
		}
		return a, err
	}
	return nil, p.errExpected("kind, relation or user")
}

func (p *parser) grant() (Stmt, error) {
	g := &Grant{At: p.at(), Revoke: p.tok().Text == "revoke"}
	p.advance()
	to := "to"
	if g.Revoke {
		to = "from"
	}
	isPerm := func(t Token) bool {
		for _, w := range []string{"read", "write", "relate", "define", "admin"} {
			if t.IsWord(w) {
				return true
			}
		}
		return false
	}
	if isPerm(p.tok()) && (p.peekTok(1).Is("on") || p.peekTok(1).Is(",")) {
		for {
			if !isPerm(p.tok()) {
				return nil, p.errExpected("read, write, relate, define or admin")
			}
			g.Perms = append(g.Perms, p.advance().Text)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect("on"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptWord("server"):
			g.Scope.What = "server"
		case p.accept("database"):
			g.Scope.What = "database"
			n, err := p.name("a database name")
			if err != nil {
				return nil, err
			}
			g.Scope.Database = n
		case p.accept("kind"):
			g.Scope.What = "kind"
			n, err := p.name("a kind name")
			if err != nil {
				return nil, err
			}
			if p.accept(".") {
				g.Scope.Database = n
				if n, err = p.name("a kind name"); err != nil {
					return nil, err
				}
			}
			g.Scope.Kind = n
		default:
			return nil, p.errExpected("server, database or kind")
		}
	} else {
		var err error
		if g.Role, err = p.name("a role name"); err != nil {
			return nil, err
		}
	}
	if err := p.expect(to); err != nil {
		return nil, err
	}
	var err error
	g.Who, err = p.name("a user or role name")
	return g, err
}

// --- data statements ---

// target reads a handle, ID, parameter or variable.
func (p *parser) target() (Expr, error) {
	t := p.tok()
	switch {
	case t.Kind == Handle:
		p.advance()
		return &HandleRef{At{t.Pos}, t.Text}, nil
	case t.Kind == Param:
		p.advance()
		return &ParamRef{At{t.Pos}, t.Text}, nil
	case t.Kind == Literal:
		if _, ok := t.Val.(value.ID); ok {
			p.advance()
			return &Lit{At{t.Pos}, t.Val}, nil
		}
	case t.Kind == Ident:
		p.advance()
		return &NameRef{At{t.Pos}, t.Text}, nil
	}
	return nil, p.errExpected("a handle, ID, parameter or variable")
}

func (p *parser) fieldRef() (FieldRef, error) {
	tg, err := p.target()
	if err != nil {
		return FieldRef{}, err
	}
	if err := p.expect("."); err != nil {
		return FieldRef{}, err
	}
	n, err := p.name("a field name")
	return FieldRef{tg, n}, err
}

func (p *parser) nodeRef() (NodeRef, error) {
	tg, err := p.target()
	if err != nil {
		return NodeRef{}, err
	}
	n := NodeRef{Target: tg}
	if p.accept(".") {
		n.Field, err = p.name("a field name")
	}
	return n, err
}

func (p *parser) create(at At) (Stmt, error) {
	c := &Create{At: at}
	var err error
	if p.tok().Kind == Ident {
		c.Kind = p.advance().Text
	}
	if p.tok().Kind == Handle {
		c.Handle = p.advance().Text
	}
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	for !p.accept("}") {
		in := &Init{At: p.at()}
		t := p.tok()
		switch {
		case t.Kind == Ident:
			in.Key = t.Text
			p.advance()
		case t.Kind == Literal:
			if s, ok := t.Val.(value.Text); ok {
				in.Key = s.Go()
				p.advance()
				break
			}
			fallthrough
		default:
			return nil, p.errExpected("a field name")
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		if in.Value, err = p.expr(); err != nil {
			return nil, err
		}
		if p.acceptWord("doc") {
			s, err := p.textLit("a doc string")
			if err != nil {
				return nil, err
			}
			in.Doc = &s
		}
		c.Inits = append(c.Inits, in)
		if !p.accept(",") {
			if err := p.expect("}"); err != nil {
				return nil, err
			}
			break
		}
	}
	for p.tok().IsWord("add") {
		adds, err := p.addClause()
		if err != nil {
			return nil, err
		}
		c.Adds = append(c.Adds, adds...)
	}
	return c, nil
}

func (p *parser) addClause() ([]*AddField, error) {
	if err := p.expectWord("add"); err != nil {
		return nil, err
	}
	var out []*AddField
	for {
		a := &AddField{At: p.at()}
		var err error
		if a.Name, err = p.name("a field name"); err != nil {
			return nil, err
		}
		if p.accept(":") {
			if a.Type, err = p.typ(); err != nil {
				return nil, err
			}
		}
		if err := p.expect("="); err != nil {
			return nil, err
		}
		if a.Value, err = p.expr(); err != nil {
			return nil, err
		}
		once := options{}
	opts:
		for {
			at := p.pos()
			var opt string
			switch t := p.tok(); {
			case t.IsWord("doc"), t.IsWord("check"), t.Is("history"):
				opt = t.Text
			case t.Is("on"):
				opt = "on delete"
			case t.Is("unique"), t.Is("default"):
				return nil, syntaxErr(at, "%s isn't allowed in an add clause: it only applies to kind fields", t.Text)
			}
			if opt != "" {
				if err := once.use(at, opt); err != nil {
					return nil, err
				}
			}
			switch {
			case p.acceptWord("doc"):
				s, err := p.textLit("a doc string")
				if err != nil {
					return nil, err
				}
				a.Doc = &s
			case p.tok().IsWord("check"):
				p.advance()
				if a.Check, err = p.expr(); err != nil {
					return nil, err
				}
			case p.tok().Is("on"):
				if a.OnDelete, err = p.onDeleteRef(); err != nil {
					return nil, err
				}
			case p.tok().Is("history"):
				if a.History, err = p.historyOpt(); err != nil {
					return nil, err
				}
			default:
				break opts
			}
		}
		out = append(out, a)
		if !p.accept(",") {
			return out, nil
		}
	}
}

var assignOps = []string{"=", "+=", "-=", "*=", "/=", "++=", "--="}

func (p *parser) update() (Stmt, error) {
	u := &Update{At: p.at()}
	p.advance()
	var err error
	if u.Target, err = p.target(); err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("set"):
			sa := &SetAction{}
			for {
				as, err := p.assign()
				if err != nil {
					return nil, err
				}
				sa.Assigns = append(sa.Assigns, as)
				if !p.accept(",") {
					break
				}
			}
			u.Actions = append(u.Actions, sa)
		case p.accept("unset"):
			names, err := p.nameList("a field name")
			if err != nil {
				return nil, err
			}
			u.Actions = append(u.Actions, &UnsetAction{names})
		case p.tok().IsWord("add"):
			fs, err := p.addClause()
			if err != nil {
				return nil, err
			}
			u.Actions = append(u.Actions, &AddAction{fs})
		case p.accept("drop"):
			names, err := p.nameList("a field name")
			if err != nil {
				return nil, err
			}
			u.Actions = append(u.Actions, &DropAction{names})
		default:
			if len(u.Actions) == 0 {
				return nil, p.errExpected("set, unset, add or drop")
			}
			return u, nil
		}
	}
}

func (p *parser) nameList(what string) ([]string, error) {
	var out []string
	for {
		n, err := p.name(what)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if !p.accept(",") {
			return out, nil
		}
	}
}

func (p *parser) assign() (*Assign, error) {
	a := &Assign{At: p.at(), Target: &LValue{}}
	var err error
	if p.accept("::") {
		a.Target.Meta = true
		a.Target.Name, err = p.metaName()
	} else {
		a.Target.Name, err = p.name("a field name")
	}
	if err != nil {
		return nil, err
	}
	for {
		if p.accept(".") {
			n, err := p.name("a field name")
			if err != nil {
				return nil, err
			}
			a.Target.Steps = append(a.Target.Steps, LStep{Field: n})
			continue
		}
		if p.accept("[") {
			ix, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			a.Target.Steps = append(a.Target.Steps, LStep{Index: ix})
			continue
		}
		break
	}
	for _, op := range assignOps {
		if p.accept(op) {
			a.Op = op
			a.Value, err = p.expr()
			return a, err
		}
	}
	return nil, p.errExpected("=, +=, -=, *=, /=, ++= or --=")
}

func (p *parser) relate() (Stmt, error) {
	r := &Relate{At: p.at()}
	p.advance()
	var err error
	if r.From, err = p.nodeRef(); err != nil {
		return nil, err
	}
	if r.Edge, err = p.edge(); err != nil {
		return nil, err
	}
	if r.To, err = p.nodeRef(); err != nil {
		return nil, err
	}
	if p.accept("as") {
		r.Handle, err = p.handle()
	}
	return r, err
}

func (p *parser) unrelate() (Stmt, error) {
	u := &Unrelate{At: p.at()}
	p.advance()
	from, err := p.nodeRef()
	if err != nil {
		return nil, err
	}
	if from.Field == "" && !p.tok().Is("~") && !p.tok().Is("<~") {
		u.Target = from.Target
		return u, nil
	}
	u.From = &from
	if u.Edge, err = p.edge(); err != nil {
		return nil, err
	}
	to, err := p.nodeRef()
	u.To = &to
	return u, err
}

func (p *parser) push() (Stmt, error) {
	s := &Push{At: p.at()}
	p.advance()
	var err error
	if s.Field, err = p.fieldRef(); err != nil {
		return nil, err
	}
	if s.Value, err = p.expr(); err != nil {
		return nil, err
	}
	if p.accept("priority") {
		s.Priority, err = p.expr()
	}
	return s, err
}

func (p *parser) pop() (Stmt, error) {
	s := &Pop{At: p.at()}
	p.advance()
	// "pop 10 from q" versus "pop q": try a count first.
	save := p.i
	if e, err := p.expr(); err == nil && p.accept("from") {
		s.Count = e
	} else {
		p.i = save
	}
	var err error
	if s.Field, err = p.fieldRef(); err != nil {
		return nil, err
	}
	if p.acceptWord("wait") {
		if s.Wait, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.accept("as") {
		s.As, err = p.name("a variable name")
	}
	return s, err
}

// --- pipelines ---

func (p *parser) pipeline() (*Pipeline, error) {
	pl := &Pipeline{At: p.at()}
	if err := p.expect("find"); err != nil {
		return nil, err
	}
	t := p.tok()
	_, isID := t.Val.(value.ID)
	if t.Kind == Handle || t.Kind == Param || t.Kind == Literal && isID {
		start, err := p.target()
		if err != nil {
			return nil, err
		}
		pl.Source.Start = start
		if err := p.expect("as"); err != nil {
			return nil, err
		}
		if pl.Source.StartAs, err = p.name("a variable name"); err != nil {
			return nil, err
		}
	} else {
		for {
			a, err := p.name("a kind or variable name")
			if err != nil {
				return nil, err
			}
			kv := KindVar{Var: a}
			if p.tok().Kind == Ident {
				kv = KindVar{Kind: a, Var: p.advance().Text}
			}
			pl.Source.Vars = append(pl.Source.Vars, kv)
			if !p.accept(",") {
				break
			}
		}
		if p.accept("in") {
			var err error
			if pl.Source.In, err = p.expr(); err != nil {
				return nil, err
			}
		}
	}
	if p.tok().Is("as") && p.peekTok(1).Is("of") {
		p.advance()
		p.advance()
		var err error
		if pl.AsOf, err = p.expr(); err != nil {
			return nil, err
		}
	}
	for p.accept("|") {
		s, err := p.step()
		if err != nil {
			return nil, err
		}
		pl.Steps = append(pl.Steps, s)
	}
	return pl, nil
}

// atStepEnd reports whether the current token ends a step's optional list.
func (p *parser) atStepEnd() bool {
	t := p.tok()
	return t.Kind == EOF || t.Is(";") || t.Is("|") || t.Is(")")
}

func (p *parser) step() (Step, error) {
	at := p.at()
	t := p.tok()
	var err error
	switch {
	case p.accept("where"):
		w := &Where{At: at}
		w.Cond, err = p.expr()
		return w, err
	case t.Is("follow") || t.Is("match"):
		p.advance()
		f := &Follow{At: at, Match: t.Text == "match"}
		if p.acceptWord("path") {
			if f.PathVar, err = p.name("a path variable"); err != nil {
				return nil, err
			}
			if err := p.expect("="); err != nil {
				return nil, err
			}
		}
		f.Pattern, err = p.pattern()
		return f, err
	case p.accept("let"):
		l := &Let{At: at}
		if l.Name, err = p.name("a variable name"); err != nil {
			return nil, err
		}
		if err := p.expect("="); err != nil {
			return nil, err
		}
		l.Value, err = p.expr()
		return l, err
	case p.accept("group"):
		if err := p.expect("by"); err != nil {
			return nil, err
		}
		g := &GroupBy{At: at}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("as"); err != nil {
				return nil, err
			}
			n, err := p.name("a group name")
			if err != nil {
				return nil, err
			}
			g.Keys = append(g.Keys, Named{e, n})
			if !p.accept(",") {
				return g, nil
			}
		}
	case p.accept("having"):
		h := &Having{At: at}
		h.Cond, err = p.expr()
		return h, err
	case p.accept("sort"):
		s := &Sort{At: at}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			k := SortKey{Expr: e}
			if p.accept("desc") {
				k.Desc = true
			} else {
				p.accept("asc")
			}
			s.Keys = append(s.Keys, k)
			if !p.accept(",") {
				return s, nil
			}
		}
	case p.accept("distinct"):
		d := &Distinct{At: at}
		for !p.atStepEnd() {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			d.Exprs = append(d.Exprs, e)
			if !p.accept(",") {
				break
			}
		}
		return d, nil
	case p.accept("skip"):
		s := &Skip{At: at}
		s.N, err = p.expr()
		return s, err
	case p.accept("limit"):
		l := &Limit{At: at}
		l.N, err = p.expr()
		return l, err
	case p.accept("show"):
		s := &Show{At: at}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			n := Named{Expr: e}
			if p.accept("as") {
				if n.Name, err = p.name("a column name"); err != nil {
					return nil, err
				}
			}
			s.Items = append(s.Items, n)
			if !p.accept(",") {
				return s, nil
			}
		}
	case p.accept("bind"):
		b := &Bind{At: at}
		if b.Var, err = p.name("a variable name"); err != nil {
			return nil, err
		}
		if err := p.expect("as"); err != nil {
			return nil, err
		}
		b.Handle, err = p.handle()
		return b, err
	case p.accept("union"):
		if err := p.expect("("); err != nil {
			return nil, err
		}
		sub, err := p.pipeline()
		if err != nil {
			return nil, err
		}
		return &UnionStep{at, sub}, p.expect(")")
	case t.Is("update"), t.Is("delete"), t.Is("unrelate"), t.Is("push"), t.Is("pop"):
		s, err := p.statement()
		return &DataStep{at, s}, err
	}
	words := []string{"where", "follow", "match", "let", "group", "having", "sort", "distinct", "skip", "limit",
		"show", "bind", "union", "update", "delete", "unrelate", "push", "pop"}
	if t.Kind == Ident || t.Kind == Keyword {
		if s := model.Suggest(strings.ToLower(t.Text), words); s != "" {
			return nil, syntaxErr(t.Pos, "unknown pipeline step %s (did you mean %s?)", t.Text, s)
		}
	}
	return nil, p.errExpected("a pipeline step")
}

// --- patterns ---

func (p *parser) pattern() (*Pattern, error) {
	pt := &Pattern{}
	n, err := p.pnode()
	if err != nil {
		return nil, err
	}
	pt.Nodes = append(pt.Nodes, n)
	for p.tok().Is("~") || p.tok().Is("<~") {
		e, err := p.edge()
		if err != nil {
			return nil, err
		}
		n, err := p.pnode()
		if err != nil {
			return nil, err
		}
		pt.Edges = append(pt.Edges, e)
		pt.Nodes = append(pt.Nodes, n)
	}
	if len(pt.Edges) == 0 {
		return nil, p.errExpected("a relation step such as ~knows~>")
	}
	return pt, nil
}

func (p *parser) pnode() (*PNode, error) {
	n := &PNode{At: p.at()}
	t := p.tok()
	switch {
	case t.Kind == Handle:
		p.advance()
		n.Handle = t.Text
	case t.Kind == Literal:
		id, ok := t.Val.(value.ID)
		if !ok {
			return nil, p.errExpected("a node: [Kind] variable, @handle or #id")
		}
		p.advance()
		n.ID = &id
		return n, nil
	default:
		a, err := p.name("a node: [Kind] variable, @handle or #id")
		if err != nil {
			return nil, err
		}
		n.Var = a
		if p.tok().Kind == Ident {
			n.Kind, n.Var = a, p.advance().Text
		}
	}
	if p.accept(".") {
		var err error
		if n.Field, err = p.name("a field name"); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (p *parser) edge() (*Edge, error) {
	e := &Edge{At: p.at()}
	in := false
	switch {
	case p.accept("<~"):
		in = true
	case p.accept("~"):
	default:
		return nil, p.errExpected("~ or <~")
	}
	if p.accept("*") {
		e.AnyType = true
	} else {
		for {
			n, err := p.name("a relation type or *")
			if err != nil {
				return nil, err
			}
			e.Types = append(e.Types, n)
			if !p.accept("|") {
				break
			}
		}
	}
	if p.tok().Kind == Ident {
		e.Var = p.advance().Text
	}
	if p.tok().Is("{") {
		b, err := p.braceLit()
		if err != nil {
			return nil, err
		}
		m, ok := b.(*MapLit)
		if !ok {
			return nil, syntaxErr(b.Position(), "relation properties must be written {name: value, …}")
		}
		e.Props = m
	}
	if p.accept("*") {
		e.HasDepth, e.Min, e.Max = true, 1, -1
		if p.tok().Kind == Literal {
			n, err := p.intLit("a depth")
			if err != nil {
				return nil, err
			}
			e.Min, e.Max = n, n
			if p.accept("..") {
				if e.Max, err = p.intLit("a maximum depth"); err != nil {
					return nil, err
				}
			}
		}
	}
	switch {
	case in:
		if err := p.expect("~"); err != nil {
			return nil, err
		}
		e.Dir = DirIn
	case p.accept("~>"):
		e.Dir = DirOut
	case p.accept("~"):
		e.Dir = DirEither
	default:
		return nil, p.errExpected("~> or ~ to end the relation step")
	}
	return e, nil
}

// ===== types =====

var typeWords = map[string]bool{"bool": true, "int": true, "bigint": true, "float": true, "decimal": true,
	"text": true, "bytes": true, "time": true, "date": true, "duration": true, "id": true, "ref": true,
	"image": true, "any": true, "doc": true, "list": true, "array": true, "set": true, "map": true,
	"pair": true, "queue": true, "record": true, "union": true}

func isTypeStart(t Token) bool {
	return (t.Kind == Ident && !t.Quoted || t.Kind == Keyword) && typeWords[t.Text]
}

// typ parses a type expression and validates it.
func (p *parser) typ() (*value.Type, error) {
	start := p.pos()
	t, err := p.typeExpr(1)
	if err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, &Error{Pos: start, Code: "type", Msg: err.Error()}
	}
	return t, nil
}

func (p *parser) typeExpr(depth int) (*value.Type, error) {
	if depth > value.MaxNesting {
		return nil, syntaxErr(p.pos(), "type nested more than %d levels deep", value.MaxNesting)
	}
	t, err := p.baseType(depth)
	if err != nil {
		return nil, err
	}
	if p.accept("?") {
		if t.Code == value.CodeAny {
			return nil, syntaxErr(p.pos(), "any? is not allowed: any already includes null")
		}
		t = value.OptionalOf(t)
	}
	return t, nil
}

func (p *parser) typeArg(depth int) (*value.Type, error) {
	if err := p.expect("<"); err != nil {
		return nil, err
	}
	return p.typeExpr(depth + 1)
}

func (p *parser) baseType(depth int) (*value.Type, error) {
	t := p.tok()
	if !isTypeStart(t) {
		if t.Kind == Ident {
			if s := model.Suggest(t.Text, mapKeysSorted(typeWords)); s != "" {
				return nil, syntaxErr(t.Pos, "unknown type %s (did you mean %s?)", t.Text, s)
			}
		}
		return nil, p.errExpected("a type")
	}
	p.advance()
	scalars := map[string]*value.Type{"bool": value.TypeBool, "int": value.TypeInt, "bigint": value.TypeBigInt,
		"float": value.TypeFloat, "text": value.TypeText, "bytes": value.TypeBytes, "time": value.TypeTime,
		"date": value.TypeDate, "duration": value.TypeDuration, "id": value.TypeID, "image": value.TypeImage,
		"any": value.TypeAny}
	if s, ok := scalars[t.Text]; ok {
		cp := *s
		return &cp, nil
	}
	switch t.Text {
	case "decimal":
		if p.accept("(") {
			prec, err := p.intLit("a precision")
			if err != nil {
				return nil, err
			}
			if err := p.expect(","); err != nil {
				return nil, err
			}
			scale, err := p.intLit("a scale")
			if err != nil {
				return nil, err
			}
			return value.DecimalOf(prec, scale), p.expect(")")
		}
		return &value.Type{Code: value.CodeDecimal}, nil
	case "ref":
		if p.accept("<") {
			n, err := p.name("a kind name")
			if err != nil {
				return nil, err
			}
			return value.RefTo(n), p.expect(">")
		}
		return &value.Type{Code: value.CodeRef}, nil
	case "doc":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		n, err := p.name("a format name")
		if err != nil {
			return nil, err
		}
		return value.DocOf(n), p.expect(">")
	case "list", "set":
		e, err := p.typeArg(depth)
		if err != nil {
			return nil, err
		}
		if t.Text == "list" {
			return value.ListOf(e), p.expect(">")
		}
		return value.SetOf(e), p.expect(">")
	case "array":
		e, err := p.typeArg(depth)
		if err != nil {
			return nil, err
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
		n, err := p.intLit("the array length")
		if err != nil {
			return nil, err
		}
		return value.ArrayOf(e, n), p.expect(">")
	case "map", "pair":
		k, err := p.typeArg(depth)
		if err != nil {
			return nil, err
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
		v, err := p.typeExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		if t.Text == "map" {
			return value.MapOf(k, v), p.expect(">")
		}
		return value.PairOf(k, v), p.expect(">")
	case "queue":
		if err := p.expect("."); err != nil {
			return nil, err
		}
		kindTok := p.tok()
		if kindTok.Kind != Ident && kindTok.Kind != Keyword {
			return nil, p.errExpected("fifo, lifo, ring or priority")
		}
		kind := p.advance().Text // priority is a keyword
		e, err := p.typeArg(depth)
		if err != nil {
			return nil, err
		}
		switch kind {
		case "fifo", "lifo", "priority":
			code := map[string]value.Code{"fifo": value.CodeFifo, "lifo": value.CodeLifo, "priority": value.CodePriority}[kind]
			q := value.QueueOf(code, e, 0)
			if kind == "priority" && p.tok().Is(",") && (p.peekTok(1).IsWord("high") || p.peekTok(1).IsWord("low")) {
				p.advance()
				q.LowFirst = p.advance().Text == "low"
			}
			if p.accept(",") {
				if err := p.expectWord("max"); err != nil {
					return nil, err
				}
				if q.N, err = p.intLit("a maximum size"); err != nil {
					return nil, err
				}
			}
			return q, p.expect(">")
		case "ring":
			if err := p.expect(","); err != nil {
				return nil, err
			}
			n, err := p.intLit("the ring capacity")
			if err != nil {
				return nil, err
			}
			return value.RingOf(e, n), p.expect(">")
		}
		return nil, syntaxErr(kindTok.Pos, "unknown queue kind %s: use fifo, lifo, ring or priority", kind)
	case "record":
		if err := p.expect("{"); err != nil {
			return nil, err
		}
		var fields []value.RecordField
		for !p.accept("}") {
			n, err := p.name("a field name")
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			ft, err := p.typeExpr(depth + 1)
			if err != nil {
				return nil, err
			}
			f := value.RecordField{Name: n, Type: ft}
			if d, err := p.optDoc(true); err != nil {
				return nil, err
			} else if d != nil {
				f.Doc = *d
			}
			fields = append(fields, f)
			p.accept(",")
		}
		return value.RecordOf(fields...), nil
	case "union":
		if err := p.expect("<"); err != nil {
			return nil, err
		}
		var alts []*value.Type
		for {
			a, err := p.typeExpr(depth + 1)
			if err != nil {
				return nil, err
			}
			alts = append(alts, a)
			if !p.accept("|") {
				break
			}
		}
		return value.UnionOf(alts...), p.expect(">")
	}
	return nil, syntaxErr(t.Pos, "unknown type %s", t.Text)
}

func mapKeysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
