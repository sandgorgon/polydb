package lang

import (
	"math"

	"github.com/sandgorgon/polydb/value"
)

// Expression parsing follows spec/rhizome.md §15.4, lowest precedence
// first.

func (p *parser) expr() (Expr, error) {
	x, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	if p.tok().Is("=>") {
		at := p.at()
		p.advance()
		y, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		return &Binary{at, "=>", x, y}, nil
	}
	return x, nil
}

func (p *parser) orExpr() (Expr, error) {
	x, err := p.andExpr()
	for err == nil && p.tok().Is("or") {
		at := p.at()
		p.advance()
		var y Expr
		if y, err = p.andExpr(); err == nil {
			x = &Binary{at, "or", x, y}
		}
	}
	return x, err
}

func (p *parser) andExpr() (Expr, error) {
	x, err := p.notExpr()
	for err == nil && p.tok().Is("and") {
		at := p.at()
		p.advance()
		var y Expr
		if y, err = p.notExpr(); err == nil {
			x = &Binary{at, "and", x, y}
		}
	}
	return x, err
}

func (p *parser) notExpr() (Expr, error) {
	if p.tok().Is("not") {
		at := p.at()
		p.advance()
		x, err := p.notExpr()
		return &Unary{at, "not", x}, err
	}
	return p.cmpExpr()
}

var cmpOps = []string{"=", "!=", "<", "<=", ">", ">="}

func (p *parser) cmpExpr() (Expr, error) {
	x, err := p.addExpr()
	if err != nil {
		return nil, err
	}
	at := p.at()
	t := p.tok()
	bin := func(op string) (Expr, error) {
		y, err := p.addExpr()
		return &Binary{at, op, x, y}, err
	}
	for _, op := range cmpOps {
		if t.Is(op) {
			p.advance()
			e, err := bin(op)
			if err == nil && p.isCmpStart() {
				return nil, syntaxErr(p.pos(), "comparisons don't chain; use and: a < b and b < c")
			}
			return e, err
		}
	}
	switch {
	case t.Is("in"):
		p.advance()
		return bin("in")
	case t.Is("not") && p.peekTok(1).Is("in"):
		p.advance()
		p.advance()
		return bin("not in")
	case t.Is("not") && p.peekTok(1).Is("between"), t.Is("between"):
		not := p.accept("not")
		p.advance()
		lo, err := p.addExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expect("and"); err != nil {
			return nil, err
		}
		hi, err := p.addExpr()
		return &Between{at, x, lo, hi, not}, err
	case t.Is("has"):
		p.advance()
		return bin("has")
	case t.Is("matches"):
		p.advance()
		return bin("matches")
	case t.IsWord("contains"):
		p.advance()
		return bin("contains")
	case t.IsWord("starts") && p.peekTok(1).Is("with"):
		p.advance()
		p.advance()
		return bin("starts with")
	case t.IsWord("ends") && p.peekTok(1).Is("with"):
		p.advance()
		p.advance()
		return bin("ends with")
	case t.Is("is"):
		p.advance()
		is := &Is{At: at, X: x, Not: p.accept("not")}
		switch {
		case p.accept("null"):
			is.Null = true
		case isTypeStart(p.tok()):
			if is.Type, err = p.typ(); err != nil {
				return nil, err
			}
		default:
			if is.Kind, err = p.name("null, a type or a kind"); err != nil {
				return nil, err
			}
		}
		return is, nil
	}
	return x, nil
}

func (p *parser) isCmpStart() bool {
	t := p.tok()
	for _, op := range cmpOps {
		if t.Is(op) {
			return true
		}
	}
	return t.Is("in") || t.Is("between") || t.Is("is") || t.Is("has") || t.Is("matches")
}

func (p *parser) addExpr() (Expr, error) {
	x, err := p.mulExpr()
	for err == nil {
		t := p.tok()
		if !(t.Is("+") || t.Is("-") || t.Is("++") || t.Is("--")) {
			break
		}
		at := p.at()
		p.advance()
		var y Expr
		if y, err = p.mulExpr(); err == nil {
			x = &Binary{at, t.Text, x, y}
		}
	}
	return x, err
}

func (p *parser) mulExpr() (Expr, error) {
	x, err := p.unary()
	for err == nil {
		t := p.tok()
		if !(t.Is("*") || t.Is("/") || t.Is("%")) {
			break
		}
		at := p.at()
		p.advance()
		var y Expr
		if y, err = p.unary(); err == nil {
			x = &Binary{at, t.Text, x, y}
		}
	}
	return x, err
}

func (p *parser) unary() (Expr, error) {
	if p.tok().Is("-") {
		at := p.at()
		p.advance()
		x, err := p.unary()
		return &Unary{at, "-", x}, err
	}
	return p.postfix()
}

func (p *parser) postfix() (Expr, error) {
	x, err := p.primary()
	for err == nil {
		at := p.at()
		switch {
		case p.accept("."):
			var n string
			if n, err = p.name("a field name"); err == nil {
				x = &FieldExpr{at, x, n}
			}
		case p.accept("::"):
			var n string
			if n, err = p.metaName(); err == nil {
				x = &MetaExpr{at, x, n}
			}
		case p.accept("["):
			var ix Expr
			if ix, err = p.expr(); err == nil {
				err = p.expect("]")
				x = &IndexExpr{at, x, ix}
			}
		case p.accept("->"):
			var st DocStep
			if st, err = p.docStep(); err == nil {
				x = &DocPath{at, x, st}
			}
		default:
			return x, nil
		}
	}
	return nil, err
}

func (p *parser) docStep() (DocStep, error) {
	t := p.tok()
	switch {
	case t.Kind == Ident:
		p.advance()
		return DocStep{Name: t.Text}, nil
	case t.Kind == Handle:
		p.advance()
		return DocStep{Name: t.Text, Attr: true}, nil
	case t.Is("@"):
		p.advance()
		s, err := p.textLit("an attribute name")
		return DocStep{Name: s, Attr: true, Quoted: true}, err
	case t.Kind == Literal:
		if s, ok := t.Val.(value.Text); ok {
			p.advance()
			return DocStep{Name: s.Go(), Quoted: true}, nil
		}
	case p.accept("["):
		ix, err := p.expr()
		if err != nil {
			return DocStep{}, err
		}
		return DocStep{Index: ix}, p.expect("]")
	}
	return DocStep{}, p.errExpected("a document step: name, \"key\", [index] or @attribute")
}

func (p *parser) primary() (Expr, error) {
	t := p.tok()
	at := At{t.Pos}
	switch t.Kind {
	case Literal:
		p.advance()
		return &Lit{at, t.Val}, nil
	case Handle:
		p.advance()
		return &HandleRef{at, t.Text}, nil
	case Param:
		p.advance()
		return &ParamRef{at, t.Text}, nil
	case Ident:
		p.advance()
		if p.tok().Is("(") {
			args, err := p.args()
			return &Call{at, t.Text, args}, err
		}
		return &NameRef{at, t.Text}, nil
	case Keyword:
		switch t.Text {
		case "true", "false":
			p.advance()
			return &Lit{at, value.Bool(t.Text == "true")}, nil
		case "null":
			p.advance()
			return &Lit{at, value.Null}, nil
		case "nan":
			p.advance()
			return &Lit{at, value.NewFloat(math.NaN())}, nil
		case "inf":
			p.advance()
			return &Lit{at, value.NewFloat(math.Inf(1))}, nil
		case "if":
			p.advance()
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			if len(args) != 3 {
				return nil, syntaxErr(t.Pos, "if takes 3 arguments: if(condition, then, else)")
			}
			return &If{at, args[0], args[1], args[2]}, nil
		case "exists":
			p.advance()
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			if len(args) != 1 {
				return nil, syntaxErr(t.Pos, "exists takes 1 argument")
			}
			return &Exists{at, args[0]}, nil
		case "sort":
			// sort(list) is a function; the keyword names the pipeline
			// step everywhere else.
			if p.peekTok(1).Is("(") {
				p.advance()
				args, err := p.args()
				return &Call{at, t.Text, args}, err
			}
		case "cast":
			p.advance()
			if err := p.expect("("); err != nil {
				return nil, err
			}
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("as"); err != nil {
				return nil, err
			}
			ty, err := p.typ()
			if err != nil {
				return nil, err
			}
			return &Cast{at, x, ty}, p.expect(")")
		}
	case Punct:
		switch t.Text {
		case "::":
			p.advance()
			n, err := p.metaName()
			return &MetaRef{at, n}, err
		case "(":
			p.advance()
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			return x, p.expect(")")
		case "[":
			p.advance()
			l := &ListLit{At: at}
			for !p.accept("]") {
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				l.Elems = append(l.Elems, e)
				if !p.accept(",") {
					if err := p.expect("]"); err != nil {
						return nil, err
					}
					break
				}
			}
			return l, nil
		case "{":
			return p.braceLit()
		}
	}
	if t.Kind == Keyword {
		return nil, syntaxErr(t.Pos, "expected an expression, found the keyword %s", t.Text)
	}
	return nil, p.errExpected("an expression")
}

func (p *parser) args() ([]Expr, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var args []Expr
	for !p.accept(")") {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		args = append(args, e)
		if !p.accept(",") {
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			break
		}
	}
	return args, nil
}

// braceLit parses {…}: a set ({a, b}), or a map or record ({k: v}). {} is
// an empty map.
func (p *parser) braceLit() (Expr, error) {
	at := p.at()
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	if p.accept("}") {
		return &MapLit{At: at}, nil
	}
	var set *SetLit
	var m *MapLit
	for {
		var key Expr
		bare := false
		if t := p.tok(); t.Kind == Ident && p.peekTok(1).Is(":") && !p.peekTok(1).Is("::") {
			p.advance()
			key, bare = &Lit{At{t.Pos}, value.MustText(t.Text)}, true
		} else {
			var err error
			if key, err = p.expr(); err != nil {
				return nil, err
			}
		}
		if p.accept(":") {
			if set != nil {
				return nil, syntaxErr(p.pos(), "can't mix set elements and map entries in one literal")
			}
			v, err := p.expr()
			if err != nil {
				return nil, err
			}
			if m == nil {
				m = &MapLit{At: at}
			}
			m.Entries = append(m.Entries, Entry{key, bare, v})
		} else {
			if m != nil {
				return nil, syntaxErr(key.Position(), "expected \":\" after a map key")
			}
			if set == nil {
				set = &SetLit{At: at}
			}
			set.Elems = append(set.Elems, key)
		}
		if !p.accept(",") {
			break
		}
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	if m != nil {
		return m, nil
	}
	return set, nil
}
