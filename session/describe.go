package session

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// Introspection results are tables (spec/rhizome.md §12) whose cells are
// mostly text, so they read well in the CLI.

func table(cols ...string) *query.Result {
	r := &query.Result{}
	for _, c := range cols {
		r.Columns = append(r.Columns, query.Column{Name: c, Type: lang.Static{Type: value.TypeAny}})
	}
	return r
}

// add appends a row. Strings become text, nil becomes null, values stay.
func add(r *query.Result, cells ...any) {
	row := make([]any, len(cells))
	for i, c := range cells {
		switch c := c.(type) {
		case nil:
			row[i] = value.Null
		case string:
			row[i] = value.MustText(c)
		case int:
			row[i] = value.Int(c)
		default:
			row[i] = c
		}
	}
	r.Rows = append(r.Rows, row)
}

func orAbsent(v value.Value) value.Value {
	if v == nil {
		return value.Null
	}
	return v
}

// fieldOptions describes a field definition's options.
func fieldOptions(fd *model.FieldDef) string {
	var opts []string
	if fd.Unique {
		opts = append(opts, "unique")
	}
	if fd.Column != nil {
		opts = append(opts, fmt.Sprintf("at %d", *fd.Column))
	}
	if fd.Default != "" {
		opts = append(opts, "default "+fd.Default)
	}
	if fd.Check != "" {
		opts = append(opts, "check "+fd.Check)
	}
	if fd.OnDelete == model.RefRestrict {
		opts = append(opts, "on delete restrict")
	}
	if fd.History.On {
		h := "history"
		if fd.History.Keep > 0 {
			h += " keep " + fd.History.Keep.String()
		}
		opts = append(opts, h)
	}
	return strings.Join(opts, ", ")
}

func (s *env) describe(qc *query.Context, st *lang.Describe) (*query.Result, error) {
	if _, err := lang.Check(envOf(qc), st); err != nil {
		return nil, err
	}
	tx, cat := qc.Tx, qc.Tx.Catalog()
	if st.Target != nil {
		id, err := s.targetID(qc, st.Target)
		if err != nil {
			return nil, err
		}
		if n, ok, _ := tx.Exists(id); ok && n == model.NodeEntity {
			if e, err := tx.Get(id); err == nil {
				if err := s.need(st, security.Read, e.Kind); err != nil {
					return nil, err
				}
			}
		}
		return describeNode(tx, st, id)
	}
	switch st.What {
	case "kind":
		k, _ := cat.Kind(st.Name)
		n, err := tx.CountEntities(k.Name)
		if err != nil {
			return nil, fail(st, err)
		}
		mode := "extensible"
		if k.Closed {
			mode = "closed"
		}
		r := table("section", "name", "type", "detail", "doc")
		add(r, "kind", k.Name, mode, count(n, "entity", "entities"), k.Doc)
		for i := range k.Fields {
			fd := &k.Fields[i]
			add(r, "field", fd.Name, fd.Type.String(), fieldOptions(fd), fd.Doc)
		}
		for _, ix := range cat.IndexesOn(k.ID) {
			add(r, "index", ix.Name, indexType(ix), indexKeys(ix), "")
		}
		return r, nil
	case "relation":
		rt, _ := cat.RelationType(st.Name)
		n, err := tx.CountRelations(rt.Name)
		if err != nil {
			return nil, fail(st, err)
		}
		r := table("section", "name", "type", "detail", "doc")
		add(r, "relation", rt.Name, "from "+endpoint(cat, rt.From)+" to "+endpoint(cat, rt.To),
			relationOptions(rt, n), rt.Doc)
		for i := range rt.Props {
			fd := &rt.Props[i]
			add(r, "property", fd.Name, fd.Type.String(), fieldOptions(fd), fd.Doc)
		}
		return r, nil
	case "format":
		r := table("section", "name", "type", "detail", "doc")
		f, ok := cat.FormatDef(st.Name)
		if !ok {
			add(r, "format", st.Name, "base format", "", "built in")
			return r, nil
		}
		detail := ""
		if f.Header {
			detail = "header"
		}
		add(r, "format", f.Name, f.Base, detail, f.Doc)
		what := "field"
		if f.IsTable() {
			what = "column"
		}
		for i := range f.Fields {
			fd := &f.Fields[i]
			add(r, what, fd.Name, fd.Type.String(), fieldOptions(fd), fd.Doc)
		}
		return r, nil
	case "index":
		ix, _ := cat.Index(st.Name)
		k, _ := cat.KindByID(ix.Kind)
		r := table("section", "name", "type", "detail", "doc")
		add(r, "index", ix.Name, indexType(ix), "on "+k.Name+"("+indexKeys(ix)+"), "+indexStatus(qc.Tx, ix), "")
		return r, nil
	}
	return nil, sessionErr(st, "syntax", "can't describe %s", st.What)
}

func endpoint(cat interface {
	KindByID(value.ID) (*model.Kind, bool)
}, ep model.Endpoint) string {
	switch ep.Mode {
	case model.EndpointKind:
		if k, ok := cat.KindByID(ep.Kind); ok {
			return k.Name
		}
	case model.EndpointField:
		return "field"
	}
	return "any"
}

func relationOptions(rt *model.RelationType, n int) string {
	opts := []string{count(n, "relation", "relations")}
	if !rt.Defined {
		opts = append(opts, "not defined")
	}
	if rt.MaxOut > 0 {
		opts = append(opts, fmt.Sprintf("max out %d", rt.MaxOut))
	}
	if rt.MaxIn > 0 {
		opts = append(opts, fmt.Sprintf("max in %d", rt.MaxIn))
	}
	if rt.OnDelete == model.RelRestrict {
		opts = append(opts, "on delete restrict")
	}
	return strings.Join(opts, ", ")
}

// count writes n with the singular or plural noun.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func indexType(ix *model.Index) string {
	switch {
	case ix.Unique:
		return "unique"
	case ix.Text:
		return "text"
	}
	return "index"
}

func indexKeys(ix *model.Index) string {
	var ks []string
	for _, k := range ix.Keys {
		s := k.Field
		for _, p := range k.Path {
			switch {
			case p.IsPos:
				s += fmt.Sprintf("->[%d]", p.Pos)
			case p.Attr:
				s += "->@" + p.Name
			default:
				s += "->" + p.Name
			}
		}
		ks = append(ks, s)
	}
	return strings.Join(ks, ", ")
}

// indexStatus says whether an index is ready, or how far its background
// build has got.
func indexStatus(tx *engine.Tx, ix *model.Index) string {
	if ix.Text {
		return "recorded, not built yet"
	}
	st, err := tx.IndexState(ix)
	switch {
	case err != nil:
		return "unknown: " + err.Error()
	case st.Ready:
		return "ready"
	}
	return fmt.Sprintf("building: %d entities indexed", st.Done)
}

// targetID resolves a describe target to a node ID.
func (s *env) targetID(qc *query.Context, e lang.Expr) (value.ID, error) {
	var v value.Value
	switch e := e.(type) {
	case *lang.HandleRef:
		h, ok := qc.Handles[e.Name]
		if !ok {
			return value.ID{}, sessionErr(e, "not_found", "no handle @%s", e.Name)
		}
		return h.ID, nil
	case *lang.Lit:
		v = e.Val
	case *lang.ParamRef:
		v = qc.Params[e.Name]
	}
	switch v := v.(type) {
	case value.ID:
		return v, nil
	case value.Ref:
		return v.Target, nil
	}
	return value.ID{}, sessionErr(e, "type", "describe needs a handle, an id or an id parameter")
}

func describeNode(tx *engine.Tx, st *lang.Describe, id value.ID) (*query.Result, error) {
	n, ok, err := tx.Exists(id)
	if err != nil {
		return nil, failAt(st, err)
	}
	if !ok {
		return nil, sessionErr(st, "not_found", "nothing has id %s", id)
	}
	cat := tx.Catalog()
	r := table("section", "name", "type", "value", "doc")
	switch n {
	case model.NodeRelation:
		rel, err := tx.Relation(id)
		if err != nil {
			return nil, failAt(st, err)
		}
		rt, _ := cat.RelationTypeByID(rel.Type)
		add(r, "meta", "id", "id", rel.ID, "")
		add(r, "meta", "type", "text", value.MustText(rt.Name), rt.Doc)
		add(r, "meta", "from", "id", rel.From, "")
		add(r, "meta", "to", "id", rel.To, "")
		for k, v := range rel.Props.All() {
			name := k.(value.Text).Go()
			t, doc := "any", ""
			if fd, ok := rt.Prop(name); ok {
				t, doc = fd.Type.String(), fd.Doc
			}
			add(r, "property", name, t, orAbsent(v), doc)
		}
		return r, nil
	case model.NodeField:
		f, err := tx.FieldByID(id)
		if err != nil {
			return nil, failAt(st, err)
		}
		add(r, "meta", "id", "id", f.ID, "")
		add(r, "meta", "entity", "id", f.Entity, "")
		add(r, "field", f.Name, "", orAbsent(f.Value), "")
		return r, nil
	}
	e, err := tx.Get(id)
	if err != nil {
		return nil, failAt(st, err)
	}
	kind := value.Value(value.Null)
	if e.Kind != nil {
		kind = value.MustText(e.Kind.Name)
	}
	m := e.Meta
	add(r, "meta", "id", "id", e.ID, "")
	add(r, "meta", "kind", "text", kind, "")
	add(r, "meta", "created", "time", m.Created, "")
	add(r, "meta", "updated", "time", m.Updated, "")
	add(r, "meta", "version", "int", value.Int(int64(m.Version)), "")
	add(r, "meta", "owner", "text", value.MustText(m.Owner), "")
	if m.Labels.Len() > 0 {
		add(r, "meta", "labels", "set<text>", m.Labels, "")
	}
	if m.Props.Len() > 0 {
		add(r, "meta", "props", "map<text, any>", m.Props, "")
	}
	for _, f := range e.Fields {
		def := e.Def(f)
		section := "field"
		if !f.Declared {
			section = "added field"
		}
		add(r, section, f.Name, def.Type.String(), orAbsent(f.Value), def.Doc)
	}
	// Relations of the entity and of its fields.
	nodes := []value.ID{id}
	for _, f := range e.Fields {
		nodes = append(nodes, f.ID)
	}
	for i, node := range nodes {
		prefix := ""
		if i > 0 {
			prefix = e.Fields[i-1].Name + " "
		}
		rels, err := tx.Relations(node, engine.Both, "")
		if err != nil {
			return nil, failAt(st, err)
		}
		slices.SortFunc(rels, func(a, b *model.Relation) int { return strings.Compare(a.ID.String(), b.ID.String()) })
		for _, rel := range rels {
			rt, _ := cat.RelationTypeByID(rel.Type)
			section, other := prefix+"out", rel.To
			if rel.To == node && rel.From != node {
				section, other = prefix+"in", rel.From
			}
			props := ""
			if rel.Props.Len() > 0 {
				props = rel.Props.String()
			}
			add(r, section, rt.Name, "relation", other, props)
		}
	}
	return r, nil
}

func failAt(st lang.Node, err error) error {
	return &lang.Error{Pos: st.Position(), Code: string(engine.CodeOf(err)), Msg: errMsg(err)}
}

// envOf builds a checker environment for statements the session checks
// itself.
func envOf(qc *query.Context) *lang.Env {
	env := &lang.Env{Catalog: qc.Tx.Catalog(), Params: qc.Params, Handles: map[string]lang.Static{}}
	for n, h := range qc.Handles {
		form := lang.FormEntity
		if h.Node == model.NodeRelation {
			form = lang.FormRelation
		}
		env.Handles[n] = lang.Static{Form: form}
	}
	return env
}

func (s *env) show(qc *query.Context, st *lang.ShowStmt) (*query.Result, error) {
	tx, cat := qc.Tx, qc.Tx.Catalog()
	switch st.What {
	case "kinds":
		r := table("name", "mode", "fields", "entities", "doc")
		for _, k := range cat.Kinds() {
			if !s.access().Can(security.Read, k) {
				continue
			}
			n, err := tx.CountEntities(k.Name)
			if err != nil {
				return nil, failAt(st, err)
			}
			mode := "extensible"
			if k.Closed {
				mode = "closed"
			}
			add(r, k.Name, mode, len(k.Fields), n, k.Doc)
		}
		return r, nil
	case "relations":
		r := table("name", "from", "to", "defined", "relations", "doc")
		for _, rt := range cat.RelationTypes() {
			n, err := tx.CountRelations(rt.Name)
			if err != nil {
				return nil, failAt(st, err)
			}
			add(r, rt.Name, endpoint(cat, rt.From), endpoint(cat, rt.To), value.Bool(rt.Defined), n, rt.Doc)
		}
		return r, nil
	case "formats":
		r := table("name", "base", "doc")
		for _, b := range []string{"csv", "json", "tsv", "xml"} {
			add(r, b, nil, "built in")
		}
		for _, f := range cat.Formats() {
			add(r, f.Name, f.Base, f.Doc)
		}
		return r, nil
	case "indexes":
		r := table("name", "kind", "type", "keys", "status")
		for _, ix := range cat.Indexes() {
			k, _ := cat.KindByID(ix.Kind)
			add(r, ix.Name, k.Name, indexType(ix), indexKeys(ix), indexStatus(qc.Tx, ix))
		}
		return r, nil
	case "stats":
		if st.For != "" {
			return s.kindStats(qc, st)
		}
		r := table("item", "count")
		total, err := tx.CountEntities("")
		if err != nil {
			return nil, failAt(st, err)
		}
		add(r, "entities", total)
		for _, k := range cat.Kinds() {
			if !s.access().Can(security.Read, k) {
				continue
			}
			n, err := tx.CountEntities(k.Name)
			if err != nil {
				return nil, failAt(st, err)
			}
			add(r, "kind "+k.Name, n)
		}
		rels, err := tx.CountRelations("")
		if err != nil {
			return nil, failAt(st, err)
		}
		add(r, "relations", rels)
		for _, rt := range cat.RelationTypes() {
			n, err := tx.CountRelations(rt.Name)
			if err != nil {
				return nil, failAt(st, err)
			}
			add(r, "relation "+rt.Name, n)
		}
		return r, nil
	}
	return nil, sessionErr(st, "unsupported", "show %s arrives with security and the server in a later phase", st.What)
}

// kindStats shows, for each declared field of a kind, how many entities
// have a value, how many hold null, and how many distinct values there
// are.
func (s *env) kindStats(qc *query.Context, st *lang.ShowStmt) (*query.Result, error) {
	tx := qc.Tx
	k, ok := tx.Catalog().Kind(st.For)
	if !ok {
		_, err := lang.Check(envOf(qc), st) // for the suggestion
		return nil, err
	}
	if err := s.need(st, security.Read, k); err != nil {
		return nil, err
	}
	type stat struct {
		present, nulls int
		distinct       map[string]bool
	}
	stats := make([]stat, len(k.Fields))
	for i := range stats {
		stats[i].distinct = map[string]bool{}
	}
	var ids []value.ID
	if err := tx.Entities(k.Name, func(id value.ID) bool { ids = append(ids, id); return true }); err != nil {
		return nil, failAt(st, err)
	}
	for _, id := range ids {
		e, err := tx.Get(id)
		if err != nil {
			return nil, failAt(st, err)
		}
		for i, fd := range k.Fields {
			f, ok := e.Field(fd.Name)
			if !ok || f.Value == nil {
				continue
			}
			if value.IsNull(f.Value) {
				stats[i].nulls++
				continue
			}
			stats[i].present++
			key, err := value.Key(f.Value)
			if err != nil {
				key = value.Encode(f.Value)
			}
			stats[i].distinct[string(key)] = true
		}
	}
	r := table("field", "type", "present", "null", "distinct")
	for i, fd := range k.Fields {
		add(r, fd.Name, fd.Type.String(), stats[i].present, stats[i].nulls, len(stats[i].distinct))
	}
	return r, nil
}
