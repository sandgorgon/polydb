package lang

import (
	"strconv"
	"strings"

	"github.com/sandgorgon/polydb/value"
)

// Format returns a statement as canonical Rhizome text, without the final
// ";". Parsing the result gives an equivalent statement.
func Format(s Stmt) string {
	var f formatter
	f.stmt(s)
	return f.b.String()
}

// FormatExpr returns an expression as canonical Rhizome text, with only
// the parentheses its structure needs.
func FormatExpr(e Expr) string {
	var f formatter
	f.expr(e, 0)
	return f.b.String()
}

// FormatStep returns one pipeline step as Rhizome text, without the "|".
func FormatStep(st Step) string {
	var f formatter
	f.step(st)
	return f.b.String()
}

type formatter struct{ b strings.Builder }

func (f *formatter) w(ss ...string) {
	for _, s := range ss {
		f.b.WriteString(s)
	}
}

// name writes an identifier, in backticks if it isn't a plain identifier
// or is a keyword.
func (f *formatter) name(n string) { f.w(quoteName(n)) }

func quoteName(n string) string {
	if value.IsIdentifier(n) && !Keywords[n] {
		return n
	}
	return "`" + n + "`"
}

// metaName formats a metadata name, which may be a keyword.
func metaName(n string) string {
	if Keywords[n] {
		return n
	}
	return quoteName(n)
}

func (f *formatter) doc(d *string) {
	if d != nil {
		f.w(" doc ", value.QuoteText(*d))
	}
}

func (f *formatter) history(h *HistoryOpt) {
	if h == nil {
		return
	}
	f.w(" history")
	if h.Keep != nil {
		f.w(" keep ")
		f.expr(h.Keep, 0)
	}
}

func (f *formatter) fieldDef(d *FieldDef) {
	f.name(d.Name)
	f.w(": ", d.Type.String())
	f.doc(d.Doc)
	if d.Unique {
		f.w(" unique")
	}
	if d.Default != nil {
		f.w(" default ")
		f.expr(d.Default, precOr)
	}
	if d.Check != nil {
		f.w(" check ")
		f.expr(d.Check, precOr)
	}
	if d.OnDelete != "" {
		f.w(" on delete ", d.OnDelete)
	}
	f.history(d.History)
	if d.Column != nil {
		f.w(" at ", strconv.Itoa(*d.Column))
	}
}

func (f *formatter) fields(fs []*FieldDef) {
	f.w(" {")
	for i, d := range fs {
		if i > 0 {
			f.w(",")
		}
		f.w(" ")
		f.fieldDef(d)
	}
	f.w(" }")
}

func (f *formatter) addFields(as []*AddField) {
	f.w(" add ")
	for i, a := range as {
		if i > 0 {
			f.w(", ")
		}
		f.name(a.Name)
		if a.Type != nil {
			f.w(": ", a.Type.String())
		}
		f.w(" = ")
		f.expr(a.Value, precOr)
		f.doc(a.Doc)
		if a.Check != nil {
			f.w(" check ")
			f.expr(a.Check, precOr)
		}
		if a.OnDelete != "" {
			f.w(" on delete ", a.OnDelete)
		}
		f.history(a.History)
	}
}

func (f *formatter) relOption(o *RelOption) {
	f.doc(o.Doc)
	if o.MaxOut != nil {
		f.w(" max out ", strconv.Itoa(*o.MaxOut))
	}
	if o.MaxIn != nil {
		f.w(" max in ", strconv.Itoa(*o.MaxIn))
	}
	if o.OnDelete != "" {
		f.w(" on delete ", o.OnDelete)
	}
}

func (f *formatter) target(e Expr) {
	if n, ok := e.(*NameRef); ok {
		f.name(n.Name)
		return
	}
	f.expr(e, precPostfix)
}

func (f *formatter) nodeRef(n NodeRef) {
	f.target(n.Target)
	if n.Field != "" {
		f.w(".")
		f.name(n.Field)
	}
}

func (f *formatter) fieldRef(r FieldRef) { f.nodeRef(NodeRef{r.Target, r.Field}) }

func (f *formatter) stmt(s Stmt) {
	switch s := s.(type) {
	case *CreateDatabase:
		f.w("create database ")
		f.name(s.Name)
	case *DropDatabase:
		f.w("drop database ")
		f.name(s.Name)
	case *Use:
		f.w("use ")
		f.name(s.Name)
	case *SetSetting:
		f.w("set ")
		f.name(s.Name)
		f.w(" = ")
		f.expr(s.Value, 0)
	case *DefineKind:
		f.w("define kind ")
		f.name(s.Name)
		f.doc(s.Doc)
		if s.Closed {
			f.w(" closed")
		}
		f.history(s.History)
		f.fields(s.Fields)
	case *DefineRelation:
		f.w("define relation ")
		f.name(s.Name)
		f.w(" from ")
		f.name(s.From)
		f.w(" to ")
		f.name(s.To)
		f.relOption(&RelOption{s.Doc, s.MaxOut, s.MaxIn, s.OnDelete})
		if len(s.Props) > 0 {
			f.fields(s.Props)
		}
	case *DefineFormat:
		f.w("define format ")
		f.name(s.Name)
		f.w(" : ")
		f.name(s.Base)
		f.doc(s.Doc)
		if s.Header {
			f.w(" header")
		}
		if len(s.Fields) > 0 {
			f.fields(s.Fields)
		}
	case *DefineIndex:
		f.w("define ")
		if s.Unique {
			f.w("unique ")
		}
		if s.Text {
			f.w("text ")
		}
		f.w("index ")
		f.name(s.Name)
		f.w(" on ")
		f.name(s.Kind)
		f.w("(")
		for i, k := range s.Keys {
			if i > 0 {
				f.w(", ")
			}
			f.name(k.Field)
			for _, st := range k.Path {
				f.docStep(st)
			}
		}
		f.w(")")
	case *AlterKind:
		f.w("alter kind ")
		f.name(s.Name)
		switch {
		case s.AddField != nil:
			f.w(" add ")
			f.fieldDef(s.AddField)
		case s.DropField != "":
			f.w(" drop ")
			f.name(s.DropField)
		case s.Alter != nil:
			f.w(" alter ")
			f.fieldDef(s.Alter)
		case s.RenameTo != "":
			f.w(" rename to ")
			f.name(s.RenameTo)
		case s.SetClosed != nil:
			if *s.SetClosed {
				f.w(" set closed")
			} else {
				f.w(" set extensible")
			}
		}
	case *AlterRelation:
		f.w("alter relation ")
		f.name(s.Name)
		switch {
		case s.AddProp != nil:
			f.w(" add ")
			f.fieldDef(s.AddProp)
		case s.DropProp != "":
			f.w(" drop ")
			f.name(s.DropProp)
		case s.Alter != nil:
			f.w(" alter ")
			f.fieldDef(s.Alter)
		case s.RenameTo != "":
			f.w(" rename to ")
			f.name(s.RenameTo)
		case s.SetOption != nil:
			f.w(" set")
			f.relOption(s.SetOption)
		}
	case *AlterUser:
		f.w("alter user ")
		f.name(s.Name)
		switch {
		case s.Password != nil:
			f.w(" password ")
			f.expr(s.Password, 0)
		case s.AddKey != nil:
			f.w(" add key ", value.QuoteText(*s.AddKey))
			if s.KeyLabel != nil {
				f.w(" as ", value.QuoteText(*s.KeyLabel))
			}
		case s.DropKey != nil:
			f.w(" drop key ", value.QuoteText(*s.DropKey))
		}
	case *Drop:
		f.w("drop ", s.What, " ")
		f.name(s.Name)
	case *Create:
		f.w("create")
		if s.Kind != "" {
			f.w(" ")
			f.name(s.Kind)
		}
		if s.Handle != "" {
			f.w(" @", quoteName(s.Handle))
		}
		f.w(" {")
		for i, in := range s.Inits {
			if i > 0 {
				f.w(",")
			}
			f.w(" ")
			if value.IsIdentifier(in.Key) && !Keywords[in.Key] {
				f.w(in.Key)
			} else {
				f.w(value.QuoteText(in.Key))
			}
			f.w(": ")
			f.expr(in.Value, precOr)
			f.doc(in.Doc)
		}
		f.w(" }")
		if len(s.Adds) > 0 {
			f.addFields(s.Adds)
		}
	case *Update:
		f.w("update ")
		f.target(s.Target)
		for _, a := range s.Actions {
			switch a := a.(type) {
			case *SetAction:
				f.w(" set ")
				for i, as := range a.Assigns {
					if i > 0 {
						f.w(", ")
					}
					if as.Target.Meta {
						f.w("::", metaName(as.Target.Name))
					} else {
						f.name(as.Target.Name)
					}
					for _, st := range as.Target.Steps {
						if st.Index != nil {
							f.w("[")
							f.expr(st.Index, 0)
							f.w("]")
						} else {
							f.w(".")
							f.name(st.Field)
						}
					}
					f.w(" ", as.Op, " ")
					f.expr(as.Value, precOr)
				}
			case *UnsetAction:
				f.w(" unset ")
				f.names(a.Names)
			case *AddAction:
				f.addFields(a.Fields)
			case *DropAction:
				f.w(" drop ")
				f.names(a.Names)
			}
		}
	case *Delete:
		f.w("delete ")
		f.target(s.Target)
	case *Relate:
		f.w("relate ")
		f.nodeRef(s.From)
		f.w(" ")
		f.edge(s.Edge)
		f.w(" ")
		f.nodeRef(s.To)
		if s.Handle != "" {
			f.w(" as @", quoteName(s.Handle))
		}
	case *Unrelate:
		f.w("unrelate ")
		if s.Target != nil {
			f.target(s.Target)
			return
		}
		f.nodeRef(*s.From)
		f.w(" ")
		f.edge(s.Edge)
		f.w(" ")
		f.nodeRef(*s.To)
	case *Push:
		f.w("push ")
		f.fieldRef(s.Field)
		f.w(" ")
		f.expr(s.Value, precOr)
		if s.Priority != nil {
			f.w(" priority ")
			f.expr(s.Priority, precOr)
		}
	case *Pop:
		f.w("pop ")
		if s.Count != nil {
			f.expr(s.Count, precOr)
			f.w(" from ")
		}
		f.fieldRef(s.Field)
		if s.Wait != nil {
			f.w(" wait ")
			f.expr(s.Wait, precOr)
		}
		if s.As != "" {
			f.w(" as ")
			f.name(s.As)
		}
	case *Pipeline:
		f.pipeline(s)
	case *Begin:
		f.w("begin")
		if s.ReadOnly {
			f.w(" read only")
		}
	case *Commit:
		f.w("commit")
	case *Rollback:
		f.w("rollback")
	case *CreateUser:
		f.w("create user ")
		f.name(s.Name)
		f.w(" password ")
		f.expr(s.Password, 0)
	case *CreateRole:
		f.w("create role ")
		f.name(s.Name)
	case *Grant:
		to := " to "
		if s.Revoke {
			f.w("revoke ")
			to = " from "
		} else {
			f.w("grant ")
		}
		if s.Role != "" {
			f.name(s.Role)
		} else {
			f.w(strings.Join(s.Perms, ", "), " on ", s.Scope.What)
			switch s.Scope.What {
			case "database":
				f.w(" ")
				f.name(s.Scope.Database)
			case "kind":
				f.w(" ")
				if s.Scope.Database != "" {
					f.name(s.Scope.Database)
					f.w(".")
				}
				f.name(s.Scope.Kind)
			}
		}
		f.w(to)
		f.name(s.Who)
	case *Describe:
		f.w("describe ")
		if s.Target != nil {
			f.target(s.Target)
		} else {
			f.w(s.What, " ")
			f.name(s.Name)
		}
	case *ShowStmt:
		f.w("show ", s.What)
		if s.For != "" {
			if s.What == "stats" {
				f.w(" kind ")
			} else {
				f.w(" for ")
			}
			f.name(s.For)
		}
	case *Explain:
		f.w("explain ")
		if s.Analyze {
			f.w("analyze ")
		}
		f.stmt(s.Stmt)
	case *History:
		f.w("history ")
		f.fieldRef(s.Field)
	}
}

func (f *formatter) names(ns []string) {
	for i, n := range ns {
		if i > 0 {
			f.w(", ")
		}
		f.name(n)
	}
}

func (f *formatter) pipeline(p *Pipeline) {
	f.w("find ")
	if p.Source.Start != nil {
		f.target(p.Source.Start)
		f.w(" as ")
		f.name(p.Source.StartAs)
	} else {
		for i, kv := range p.Source.Vars {
			if i > 0 {
				f.w(", ")
			}
			if kv.Kind != "" {
				f.name(kv.Kind)
				f.w(" ")
			}
			f.name(kv.Var)
		}
		if p.Source.In != nil {
			f.w(" in ")
			f.expr(p.Source.In, precAdd)
		}
	}
	if p.AsOf != nil {
		f.w(" as of ")
		f.expr(p.AsOf, precOr)
	}
	for _, s := range p.Steps {
		f.w(" | ")
		f.step(s)
	}
}

func (f *formatter) named(items []Named) {
	for i, n := range items {
		if i > 0 {
			f.w(", ")
		}
		f.expr(n.Expr, precOr)
		if n.Name != "" {
			f.w(" as ")
			f.name(n.Name)
		}
	}
}

func (f *formatter) step(s Step) {
	switch s := s.(type) {
	case *Where:
		f.w("where ")
		f.expr(s.Cond, 0)
	case *Follow:
		if s.Match {
			f.w("match ")
		} else {
			f.w("follow ")
		}
		if s.PathVar != "" {
			f.w("path ")
			f.name(s.PathVar)
			f.w(" = ")
		}
		f.pattern(s.Pattern)
	case *Let:
		f.w("let ")
		f.name(s.Name)
		f.w(" = ")
		f.expr(s.Value, 0)
	case *GroupBy:
		f.w("group by ")
		f.named(s.Keys)
	case *Having:
		f.w("having ")
		f.expr(s.Cond, 0)
	case *Sort:
		f.w("sort ")
		for i, k := range s.Keys {
			if i > 0 {
				f.w(", ")
			}
			f.expr(k.Expr, precOr)
			if k.Desc {
				f.w(" desc")
			}
		}
	case *Distinct:
		f.w("distinct")
		for i, e := range s.Exprs {
			if i > 0 {
				f.w(",")
			}
			f.w(" ")
			f.expr(e, precOr)
		}
	case *Skip:
		f.w("skip ")
		f.expr(s.N, 0)
	case *Limit:
		f.w("limit ")
		f.expr(s.N, 0)
	case *Show:
		f.w("show ")
		f.named(s.Items)
	case *Bind:
		f.w("bind ")
		f.name(s.Var)
		f.w(" as @", quoteName(s.Handle))
	case *UnionStep:
		f.w("union (")
		f.pipeline(s.Pipeline)
		f.w(")")
	case *DataStep:
		f.stmt(s.Stmt)
	}
}

func (f *formatter) pattern(p *Pattern) {
	for i, n := range p.Nodes {
		if i > 0 {
			f.w(" ")
			f.edge(p.Edges[i-1])
			f.w(" ")
		}
		switch {
		case n.ID != nil:
			f.w(n.ID.String())
			continue
		case n.Handle != "":
			f.w("@", quoteName(n.Handle))
		default:
			if n.Kind != "" {
				f.name(n.Kind)
				f.w(" ")
			}
			f.name(n.Var)
		}
		if n.Field != "" {
			f.w(".")
			f.name(n.Field)
		}
	}
}

func (f *formatter) edge(e *Edge) {
	if e.Dir == DirIn {
		f.w("<~")
	} else {
		f.w("~")
	}
	if e.AnyType {
		f.w("*")
	} else {
		for i, t := range e.Types {
			if i > 0 {
				f.w("|")
			}
			f.name(t)
		}
	}
	if e.Var != "" {
		f.w(" ")
		f.name(e.Var)
	}
	if e.Props != nil {
		f.expr(e.Props, 0)
	}
	if e.HasDepth {
		f.w("*")
		if !(e.Min == 1 && e.Max == -1) {
			f.w(strconv.Itoa(e.Min))
			if e.Max != e.Min {
				f.w("..", strconv.Itoa(e.Max))
			}
		}
	}
	if e.Dir == DirOut {
		f.w("~>")
	} else {
		f.w("~")
	}
}

func (f *formatter) docStep(s DocStep) {
	f.w("->")
	switch {
	case s.Index != nil:
		f.w("[")
		f.expr(s.Index, 0)
		f.w("]")
	case s.Attr && value.IsIdentifier(s.Name) && !s.Quoted:
		f.w("@", s.Name)
	case s.Attr:
		f.w("@", value.QuoteText(s.Name))
	case s.Quoted || !value.IsIdentifier(s.Name) || Keywords[s.Name]:
		f.w(value.QuoteText(s.Name))
	default:
		f.w(s.Name)
	}
}

// Precedence levels, lowest first (spec/rhizome.md §8.1).
const (
	precPair = iota + 1
	precOr
	precAnd
	precNot
	precCmp
	precAdd
	precMul
	precUnary
	precPostfix
)

func binaryPrec(op string) int {
	switch op {
	case "=>":
		return precPair
	case "or":
		return precOr
	case "and":
		return precAnd
	case "+", "-", "++", "--":
		return precAdd
	case "*", "/", "%":
		return precMul
	}
	return precCmp
}

func exprPrec(e Expr) int {
	switch e := e.(type) {
	case *Binary:
		return binaryPrec(e.Op)
	case *Unary:
		if e.Op == "not" {
			return precNot
		}
		return precUnary
	case *Is, *Between:
		return precCmp
	case *Lit:
		// Negative numbers print with a sign, which parses as unary minus.
		if s := e.Val.String(); strings.HasPrefix(s, "-") {
			return precUnary
		}
	}
	return precPostfix
}

// expr writes e, in parentheses if its precedence is below min.
func (f *formatter) expr(e Expr, min int) {
	if exprPrec(e) < min {
		f.w("(")
		f.expr(e, 0)
		f.w(")")
		return
	}
	switch e := e.(type) {
	case *Lit:
		f.w(e.Val.String())
	case *NameRef:
		f.name(e.Name)
	case *HandleRef:
		f.w("@", quoteName(e.Name))
	case *ParamRef:
		f.w("$", quoteName(e.Name))
	case *MetaRef:
		f.w("::", metaName(e.Name))
	case *Unary:
		if e.Op == "not" {
			f.w("not ")
			f.expr(e.X, precNot)
			return
		}
		f.w("-")
		// Avoid "--x" (the -- operator) and "-(-1)" ambiguity.
		if u, ok := e.X.(*Unary); ok && u.Op == "-" || exprPrec(e.X) == precUnary {
			f.w("(")
			f.expr(e.X, 0)
			f.w(")")
			return
		}
		f.expr(e.X, precUnary)
	case *Binary:
		p := binaryPrec(e.Op)
		left, right := p, p+1
		if p == precCmp || p == precPair {
			left = p + 1 // comparisons and pairs don't chain
		}
		f.expr(e.X, left)
		f.w(" ", e.Op, " ")
		f.expr(e.Y, right)
	case *Is:
		f.expr(e.X, precAdd)
		f.w(" is ")
		if e.Not {
			f.w("not ")
		}
		switch {
		case e.Null:
			f.w("null")
		case e.Type != nil:
			f.w(e.Type.String())
		default:
			f.name(e.Kind)
		}
	case *Between:
		f.expr(e.X, precAdd)
		if e.Not {
			f.w(" not")
		}
		f.w(" between ")
		f.expr(e.Lo, precAdd)
		f.w(" and ")
		f.expr(e.Hi, precAdd)
	case *FieldExpr:
		f.expr(e.X, precPostfix)
		f.w(".")
		f.name(e.Name)
	case *MetaExpr:
		f.expr(e.X, precPostfix)
		f.w("::", metaName(e.Name))
	case *IndexExpr:
		f.expr(e.X, precPostfix)
		f.w("[")
		f.expr(e.Index, 0)
		f.w("]")
	case *DocPath:
		f.expr(e.X, precPostfix)
		f.docStep(e.Step)
	case *Call:
		if e.Name == "sort" {
			f.w("sort")
		} else {
			f.name(e.Name)
		}
		f.args(e.Args)
	case *If:
		f.w("if")
		f.args([]Expr{e.Cond, e.Then, e.Else})
	case *Exists:
		f.w("exists")
		f.args([]Expr{e.X})
	case *Cast:
		f.w("cast(")
		f.expr(e.X, 0)
		f.w(" as ", e.Type.String(), ")")
	case *ListLit:
		f.w("[")
		f.list(e.Elems)
		f.w("]")
	case *SetLit:
		f.w("{")
		f.list(e.Elems)
		f.w("}")
	case *MapLit:
		f.w("{")
		for i, en := range e.Entries {
			if i > 0 {
				f.w(", ")
			}
			if en.Bare {
				f.name(en.Key.(*Lit).Val.(value.Text).Go())
			} else {
				f.expr(en.Key, precPair+1)
			}
			f.w(": ")
			f.expr(en.Value, precPair+1)
		}
		f.w("}")
	}
}

func (f *formatter) list(es []Expr) {
	for i, x := range es {
		if i > 0 {
			f.w(", ")
		}
		f.expr(x, 0)
	}
}

func (f *formatter) args(es []Expr) {
	f.w("(")
	f.list(es)
	f.w(")")
}
