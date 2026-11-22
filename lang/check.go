package lang

import (
	"fmt"
	"maps"
	"slices"

	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Env is what the checker knows about the session a statement runs in.
type Env struct {
	// Catalog is the current database's catalog; nil when no database is
	// in use.
	Catalog *catalog.Catalog
	// Handles are the session's handles, with what each refers to.
	Handles map[string]Static
	// Params are the parameter values supplied with the statement.
	Params map[string]value.Value
}

// RefKind says what a bare name refers to.
type RefKind uint8

const (
	RefVar    RefKind = iota + 1 // a pipeline variable, group name or show column
	RefField                     // a field of the update target, or of the entity a check reads
	RefColumn                    // an expression that names a show column, e.g. "p.name" after "show p.name"
)

// Ref is what a name, or an expression naming a column, refers to.
type Ref struct {
	Kind RefKind
	Name string
}

// Column is one output column of a pipeline.
type Column struct {
	Name string
	Type Static
}

// Info is what the checker learned about a statement, for the evaluator
// and executor.
type Info struct {
	// Types is the static type of every checked expression.
	Types map[Expr]Static
	// Consts holds literals converted for a field write (spec/types.md
	// §10.1): text to time, date, duration, id or doc, and IDs to refs.
	Consts map[Expr]value.Value
	// Refs says what each bare name, and each expression naming a show
	// column, refers to.
	Refs map[Expr]Ref
	// Columns are each pipeline's output columns.
	Columns map[*Pipeline][]Column
	// Grouped marks steps that aggregate the whole stream as one group
	// because they use aggregates without a "group by" before them.
	Grouped map[Step]bool
	// Warnings are problems that don't stop the statement, such as a
	// cross product or a relation type that has never been used.
	Warnings []*Error
}

// Check checks s against env and returns what it learned. Errors are
// *Error values with a position and a code: type, not_found,
// unknown_field, constraint or syntax (for the rules of §15.5).
func Check(env *Env, s Stmt) (*Info, error) {
	c := newChecker(env)
	if err := c.stmt(s, nil); err != nil {
		return c.info, err
	}
	return c.info, nil
}

// CheckExpr checks a standalone expression with the given variables in
// scope and returns its type.
func CheckExpr(env *Env, e Expr, vars map[string]Static) (*Info, Static, error) {
	c := newChecker(env)
	sc := newScope()
	for _, n := range slices.Sorted(maps.Keys(vars)) {
		sc.bind(n, vars[n])
	}
	t, err := c.expr(e, sc)
	return c.info, t, err
}

type checker struct {
	env  *Env
	info *Info
}

func newChecker(env *Env) *checker {
	if env == nil {
		env = &Env{}
	}
	return &checker{env: env, info: &Info{
		Types:   map[Expr]Static{},
		Consts:  map[Expr]value.Value{},
		Refs:    map[Expr]Ref{},
		Columns: map[*Pipeline][]Column{},
		Grouped: map[Step]bool{},
	}}
}

func errAt(p Pos, code, format string, args ...any) *Error {
	return &Error{Pos: p, Code: code, Msg: fmt.Sprintf(format, args...)}
}

func typeErr(p Pos, format string, args ...any) *Error {
	return errAt(p, "type", format, args...)
}

func (c *checker) warn(p Pos, format string, args ...any) {
	c.info.Warnings = append(c.info.Warnings, errAt(p, "warning", format, args...))
}

// didYouMean returns " (did you mean x?)" for the closest candidate.
func didYouMean(name string, candidates []string) string {
	if s := model.Suggest(name, candidates); s != "" {
		return fmt.Sprintf(" (did you mean %s?)", s)
	}
	return ""
}

func (c *checker) cat() (*catalog.Catalog, error) {
	if c.env.Catalog == nil {
		return nil, errAt(Pos{}, "not_found", "no database in use: run use <database> first")
	}
	return c.env.Catalog, nil
}

// kind looks up a kind, with a suggestion when it doesn't exist.
func (c *checker) kind(at Pos, name string) (*model.Kind, error) {
	cat, err := c.cat()
	if err != nil {
		return nil, err
	}
	if k, ok := cat.Kind(name); ok {
		return k, nil
	}
	return nil, errAt(at, "not_found", "no kind %s%s", name, didYouMean(name, kindNames(cat)))
}

func kindNames(cat *catalog.Catalog) []string {
	var out []string
	for _, k := range cat.Kinds() {
		out = append(out, k.Name)
	}
	return out
}

// isKindName reports whether name is a kind, for catching "find Person"
// written without a variable.
func (c *checker) isKindName(name string) bool {
	if c.env.Catalog == nil {
		return false
	}
	_, ok := c.env.Catalog.Kind(name)
	return ok
}

// ===== Scopes =====

// scope is the set of names an expression can use.
type scope struct {
	vars  map[string]Static
	order []string // binding order, for output columns
	// target is the entity an update changes: bare names may be its
	// fields and ::name its metadata.
	target *Static
	// added are fields this statement adds to the target, by name.
	added map[string]*value.Type
	// fields, when set, are the fields a check or default expression can
	// read (the kind being defined, or a relation's properties).
	fields map[string]*value.Type
	// noRows forbids names: for skip, limit and defaults.
	noRows string
	// grouped: aggregates are allowed, and their arguments are checked
	// in pre, the scope before grouping.
	grouped bool
	pre     *scope
	// inAgg is set while checking an aggregate's arguments.
	inAgg bool
	// columns maps the text of each show column to its name, so that
	// later steps can refer to "p.name" after "show p.name".
	columns map[string]string
}

func newScope() *scope { return &scope{vars: map[string]Static{}} }

func (s *scope) bind(name string, t Static) {
	if _, ok := s.vars[name]; !ok {
		s.order = append(s.order, name)
	}
	s.vars[name] = t
}

func (s *scope) lookup(name string) (Static, bool) {
	t, ok := s.vars[name]
	return t, ok
}

func (s *scope) names() []string {
	out := slices.Clone(s.order)
	out = append(out, slices.Collect(maps.Keys(s.fields))...)
	return out
}

// sub returns a copy of s for checking an aggregate's arguments.
func (s *scope) clone() *scope {
	c := *s
	c.vars = maps.Clone(s.vars)
	c.order = slices.Clone(s.order)
	return &c
}

// ===== Statements =====

func (c *checker) stmt(s Stmt, sc *scope) error {
	switch s := s.(type) {
	case *CreateDatabase, *DropDatabase, *Use, *Begin, *Commit, *Rollback,
		*CreateUser, *CreateRole, *Grant, *AlterUser, *Drop:
		return nil
	case *SetSetting:
		return c.setSetting(s)
	case *DefineKind:
		return c.defineKind(s)
	case *AlterKind:
		return c.alterKind(s)
	case *DefineRelation:
		return c.defineRelation(s)
	case *AlterRelation:
		return c.alterRelation(s)
	case *DefineFormat:
		return c.defineFormat(s)
	case *DefineIndex:
		return c.defineIndex(s)
	case *Create:
		return c.create(s)
	case *Update:
		return c.update(s, sc)
	case *Delete:
		t, err := c.target(s.Target, sc)
		if err == nil && t.Form == FormRelation {
			err = errAt(s.P, "type", "delete removes entities; use unrelate to remove a relation")
		}
		return err
	case *Relate:
		return c.relate(s, sc)
	case *Unrelate:
		return c.unrelate(s, sc)
	case *Push:
		return c.push(s, sc)
	case *Pop:
		_, err := c.pop(s, sc)
		return err
	case *Pipeline:
		_, err := c.pipeline(s)
		return err
	case *Describe:
		return c.describe(s)
	case *ShowStmt:
		if s.What == "stats" && s.For != "" {
			_, err := c.kind(s.P, s.For)
			return err
		}
		return nil
	case *Explain:
		return c.stmt(s.Stmt, sc)
	case *History:
		_, _, err := c.fieldRef(s.P, s.Field, sc)
		return err
	}
	return errAt(s.Position(), "internal", "checker: unexpected statement %T", s)
}

// settings are the session settings "set" accepts, with their types.
var settings = map[string]*value.Type{
	"timeout":   value.TypeDuration,
	"max_rows":  value.TypeInt,
	"max_depth": value.TypeInt,
}

func (c *checker) setSetting(s *SetSetting) error {
	t, ok := settings[s.Name]
	if !ok {
		return errAt(s.P, "not_found", "no setting %s%s", s.Name, didYouMean(s.Name, slices.Collect(maps.Keys(settings))))
	}
	sc := newScope()
	sc.noRows = "a setting"
	return c.write(s.Value, t, sc)
}

func (c *checker) describe(s *Describe) error {
	if s.Target != nil {
		_, err := c.target(s.Target, nil)
		return err
	}
	cat, err := c.cat()
	if err != nil {
		return err
	}
	var names []string
	switch s.What {
	case "kind":
		_, err := c.kind(s.P, s.Name)
		return err
	case "relation":
		if _, ok := cat.RelationType(s.Name); ok {
			return nil
		}
		for _, r := range cat.RelationTypes() {
			names = append(names, r.Name)
		}
	case "format":
		if _, ok := cat.Format(s.Name); ok {
			return nil
		}
		for _, f := range cat.Formats() {
			names = append(names, f.Name)
		}
		names = append(names, "json", "xml", "csv", "tsv")
	case "index":
		if _, ok := cat.Index(s.Name); ok {
			return nil
		}
		for _, ix := range cat.Indexes() {
			names = append(names, ix.Name)
		}
	}
	return errAt(s.P, "not_found", "no %s %s%s", s.What, s.Name, didYouMean(s.Name, names))
}

// ===== Definitions =====

// fieldScope returns a scope in which check expressions can read the
// given fields by name.
func fieldScope(defs []*FieldDef, existing []model.FieldDef) *scope {
	sc := newScope()
	sc.fields = map[string]*value.Type{}
	for i := range existing {
		sc.fields[existing[i].Name] = existing[i].Type
	}
	for _, d := range defs {
		sc.fields[d.Name] = d.Type
	}
	return sc
}

// fieldDef checks a field definition's expressions: the default must be a
// constant of the field's type, the check a condition on the fields in sc,
// and history keep a duration.
func (c *checker) fieldDef(d *FieldDef, sc *scope, what string) error {
	if d.Default != nil {
		ds := newScope()
		ds.noRows = "a default"
		if err := c.write(d.Default, d.Type, ds); err != nil {
			return err
		}
	}
	if d.Check != nil {
		if err := c.cond(d.Check, sc, "check"); err != nil {
			return err
		}
	}
	if d.OnDelete == "restrict" && value.Required(d.Type).Code != value.CodeRef {
		return errAt(d.P, "type", "on delete restrict only applies to ref fields, and %s is %s", d.Name, d.Type)
	}
	return c.history(d.History)
}

func (c *checker) history(h *HistoryOpt) error {
	if h == nil || h.Keep == nil {
		return nil
	}
	sc := newScope()
	sc.noRows = "history keep"
	return c.write(h.Keep, value.TypeDuration, sc)
}

// fieldDefs checks a list of field definitions. self names a kind being
// defined, which its own fields may refer to.
func (c *checker) fieldDefs(defs []*FieldDef, sc *scope, what string, self ...string) error {
	seen := map[string]bool{}
	for _, d := range defs {
		if seen[d.Name] {
			return errAt(d.P, "constraint", "%s %s is defined twice", what, d.Name)
		}
		seen[d.Name] = true
		if err := c.typeRefs(d.P, d.Type, self...); err != nil {
			return err
		}
		if err := c.fieldDef(d, sc, what); err != nil {
			return err
		}
	}
	return nil
}

// typeRefs checks that the kinds and formats a type names exist. A kind
// being defined may refer to itself; the catalog accepts that, so names
// that don't exist yet are left to it when they match self.
func (c *checker) typeRefs(at Pos, t *value.Type, self ...string) error {
	if t == nil || c.env.Catalog == nil {
		return nil
	}
	switch t.Code {
	case value.CodeRef:
		if t.Name != "" && !slices.Contains(self, t.Name) {
			if _, err := c.kind(at, t.Name); err != nil {
				return err
			}
		}
	case value.CodeDoc:
		if _, ok := c.env.Catalog.Format(t.Name); !ok {
			return errAt(at, "not_found", "no format %s", t.Name)
		}
	}
	for _, sub := range []*value.Type{t.Elem, t.Key} {
		if err := c.typeRefs(at, sub, self...); err != nil {
			return err
		}
	}
	for _, f := range t.Fields {
		if err := c.typeRefs(at, f.Type, self...); err != nil {
			return err
		}
	}
	for _, a := range t.Alts {
		if err := c.typeRefs(at, a, self...); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) defineKind(s *DefineKind) error {
	if _, err := c.cat(); err != nil {
		return err
	}
	if err := c.fieldDefs(s.Fields, fieldScope(s.Fields, nil), "field", s.Name); err != nil {
		return err
	}
	return c.history(s.History)
}

func (c *checker) alterKind(s *AlterKind) error {
	k, err := c.kind(s.P, s.Name)
	if err != nil {
		return err
	}
	d := s.AddField
	if d == nil {
		d = s.Alter
	}
	switch {
	case d != nil:
		_, exists := k.Field(d.Name)
		if s.AddField != nil && exists {
			return errAt(d.P, "constraint", "kind %s already has a field %s; use alter kind %s alter to change it", k.Name, d.Name, k.Name)
		}
		if s.Alter != nil && !exists {
			return errAt(d.P, "unknown_field", "kind %s has no field %s%s", k.Name, d.Name, didYouMean(d.Name, fieldNames(k.Fields)))
		}
		if err := c.typeRefs(d.P, d.Type, k.Name); err != nil {
			return err
		}
		return c.fieldDef(d, fieldScope([]*FieldDef{d}, k.Fields), "field")
	case s.DropField != "":
		if _, ok := k.Field(s.DropField); !ok {
			return errAt(s.P, "unknown_field", "kind %s has no field %s%s", k.Name, s.DropField, didYouMean(s.DropField, fieldNames(k.Fields)))
		}
	}
	return nil
}

// endpoint checks a relation endpoint: a kind, any or field.
func (c *checker) endpoint(at Pos, name string) error {
	if name == "any" || name == "field" {
		return nil
	}
	_, err := c.kind(at, name)
	return err
}

func (c *checker) defineRelation(s *DefineRelation) error {
	if err := c.endpoint(s.P, s.From); err != nil {
		return err
	}
	if err := c.endpoint(s.P, s.To); err != nil {
		return err
	}
	return c.fieldDefs(s.Props, fieldScope(s.Props, nil), "property")
}

func (c *checker) relType(at Pos, name string) (*model.RelationType, error) {
	cat, err := c.cat()
	if err != nil {
		return nil, err
	}
	if r, ok := cat.RelationType(name); ok {
		return r, nil
	}
	var names []string
	for _, r := range cat.RelationTypes() {
		names = append(names, r.Name)
	}
	return nil, errAt(at, "not_found", "no relation type %s%s", name, didYouMean(name, names))
}

func (c *checker) alterRelation(s *AlterRelation) error {
	r, err := c.relType(s.P, s.Name)
	if err != nil {
		return err
	}
	d := s.AddProp
	if d == nil {
		d = s.Alter
	}
	switch {
	case d != nil:
		_, exists := r.Prop(d.Name)
		if s.AddProp != nil && exists {
			return errAt(d.P, "constraint", "relation %s already has a property %s", r.Name, d.Name)
		}
		if s.Alter != nil && !exists {
			return errAt(d.P, "unknown_field", "relation %s has no property %s%s", r.Name, d.Name, didYouMean(d.Name, fieldNames(r.Props)))
		}
		if err := c.typeRefs(d.P, d.Type); err != nil {
			return err
		}
		return c.fieldDef(d, fieldScope([]*FieldDef{d}, r.Props), "property")
	case s.DropProp != "":
		if _, ok := r.Prop(s.DropProp); !ok {
			return errAt(s.P, "unknown_field", "relation %s has no property %s%s", r.Name, s.DropProp, didYouMean(s.DropProp, fieldNames(r.Props)))
		}
	}
	return nil
}

func (c *checker) defineFormat(s *DefineFormat) error {
	if _, err := c.cat(); err != nil {
		return err
	}
	if _, ok := value.LookupFormat(s.Base); !ok {
		return errAt(s.P, "not_found", "no base format %s%s", s.Base, didYouMean(s.Base, []string{"json", "xml", "csv", "tsv"}))
	}
	return c.fieldDefs(s.Fields, newScope(), "field")
}

func (c *checker) defineIndex(s *DefineIndex) error {
	k, err := c.kind(s.P, s.Kind)
	if err != nil {
		return err
	}
	for _, key := range s.Keys {
		fd, ok := k.Field(key.Field)
		if !ok {
			return errAt(s.P, "unknown_field", "kind %s has no field %s%s", k.Name, key.Field, didYouMean(key.Field, fieldNames(k.Fields)))
		}
		t := fd.Type
		if len(key.Path) > 0 {
			for _, st := range key.Path {
				if st.Index != nil {
					if l, ok := st.Index.(*Lit); !ok || (l.Val.Code() != value.CodeInt && l.Val.Code() != value.CodeText) {
						return errAt(st.Index.Position(), "type", "index paths need constant steps: a name or [n]")
					}
				}
			}
			st, err := c.docSteps(s.P, valueOf(t), key.Path, newScope())
			if err != nil {
				return err
			}
			t = st.Type
		}
		if s.Text && value.Required(t).Code != value.CodeText && !dynamic(t) {
			return errAt(s.P, "type", "a text index needs text fields, and %s is %s", key.Field, t)
		}
	}
	return nil
}

// ===== Data statements =====

func (c *checker) create(s *Create) error {
	var k *model.Kind
	if s.Kind != "" {
		var err error
		if k, err = c.kind(s.P, s.Kind); err != nil {
			return err
		}
	} else if _, err := c.cat(); err != nil {
		return err
	}
	given := map[string]bool{}
	for _, in := range s.Inits {
		if given[in.Key] {
			return errAt(in.P, "constraint", "field %s is given twice", in.Key)
		}
		given[in.Key] = true
		if k == nil {
			if _, err := c.expr(in.Value, newScope()); err != nil {
				return err
			}
			continue
		}
		fd, ok := k.Field(in.Key)
		if !ok {
			hint := didYouMean(in.Key, fieldNames(k.Fields))
			if hint == "" && !k.Closed {
				hint = fmt.Sprintf("; to give this entity a field of its own, use add %s = …", in.Key)
			}
			return errAt(in.P, "unknown_field", "kind %s has no field %s%s", k.Name, in.Key, hint)
		}
		if in.Doc != nil {
			return errAt(in.P, "constraint", "%s is declared by kind %s, which sets its doc", in.Key, k.Name)
		}
		if err := c.write(in.Value, fd.Type, newScope()); err != nil {
			return err
		}
	}
	if k != nil {
		for i := range k.Fields {
			fd := &k.Fields[i]
			if !given[fd.Name] && !fd.Type.Nullable() && fd.Default == "" && !fd.Type.Code.IsQueue() {
				return errAt(s.P, "constraint", "%s needs a value for required field %s", k.Name, fd.Name)
			}
		}
	}
	sc := newScope()
	sc.added = map[string]*value.Type{}
	for _, in := range s.Inits {
		sc.added[in.Key] = value.TypeAny
	}
	if k != nil {
		for i := range k.Fields {
			sc.added[k.Fields[i].Name] = k.Fields[i].Type
		}
	}
	return c.adds(s.Adds, k, sc)
}

// adds checks "add" clauses. sc.added holds the fields the entity already
// has, where known; the new fields are added to it.
func (c *checker) adds(adds []*AddField, k *model.Kind, sc *scope) error {
	for _, a := range adds {
		if k != nil && k.Closed {
			return errAt(a.P, "constraint", "kind %s is closed: its entities can't add fields", k.Name)
		}
		if _, ok := sc.added[a.Name]; ok {
			return errAt(a.P, "constraint", "the entity already has a field %s; use set to change it", a.Name)
		}
		t := a.Type
		if t == nil {
			t = value.TypeAny
		}
		if err := c.typeRefs(a.P, t); err != nil {
			return err
		}
		vs := newScope()
		if sc.target == nil {
			vs.noRows = "a create"
		} else {
			vs = sc
		}
		if err := c.write(a.Value, t, vs); err != nil {
			return err
		}
		sc.added[a.Name] = t
		if a.Check != nil {
			cs := newScope()
			cs.fields = maps.Clone(sc.added)
			if err := c.cond(a.Check, cs, "check"); err != nil {
				return err
			}
		}
		if a.OnDelete == "restrict" && value.Required(t).Code != value.CodeRef {
			return errAt(a.P, "type", "on delete restrict only applies to ref fields, and %s is %s", a.Name, t)
		}
		if err := c.history(a.History); err != nil {
			return err
		}
	}
	return nil
}

// target checks the target of a data statement: a handle, ID, parameter,
// or (in a pipeline) a variable.
func (c *checker) target(e Expr, sc *scope) (Static, error) {
	if _, err := c.cat(); err != nil {
		return Static{}, err
	}
	switch e := e.(type) {
	case *HandleRef:
		t, err := c.handle(e)
		c.info.Types[e] = t
		return t, err
	case *Lit:
		t := Static{Form: FormNode}
		c.info.Types[e] = t
		return t, nil
	case *ParamRef:
		v, ok := c.env.Params[e.Name]
		if !ok {
			return Static{}, errAt(e.P, "not_found", "parameter $%s isn't set", e.Name)
		}
		switch v.(type) {
		case value.ID, value.Ref:
		default:
			return Static{}, typeErr(e.P, "$%s must be an id or ref to name an entity, not %s", e.Name, v.Code())
		}
		t := Static{Form: FormNode}
		c.info.Types[e] = t
		return t, nil
	case *NameRef:
		if sc == nil {
			return Static{}, errAt(e.P, "syntax", "%s: a bare name only names a target inside a pipeline; use @handle or #id", e.Name)
		}
		t, ok := sc.lookup(e.Name)
		if !ok {
			return Static{}, errAt(e.P, "not_found", "no variable %s%s", e.Name, didYouMean(e.Name, sc.order))
		}
		if t.Form == FormValue || t.Form == FormPath {
			return Static{}, typeErr(e.P, "%s is %s, not an entity or relation", e.Name, t)
		}
		c.info.Types[e] = t
		c.info.Refs[e] = Ref{RefVar, e.Name}
		return t, nil
	}
	return Static{}, errAt(e.Position(), "syntax", "expected a handle, ID, parameter or variable")
}

func (c *checker) handle(e *HandleRef) (Static, error) {
	t, ok := c.env.Handles[e.Name]
	if !ok {
		var names []string
		for n := range c.env.Handles {
			names = append(names, "@"+n)
		}
		return Static{}, errAt(e.P, "not_found", "no handle @%s%s", e.Name, didYouMean("@"+e.Name, names))
	}
	return t, nil
}

// entityTarget is target for statements that change an entity.
func (c *checker) entityTarget(e Expr, sc *scope, verb string) (Static, error) {
	t, err := c.target(e, sc)
	if err == nil && t.Form == FormRelation {
		err = typeErr(e.Position(), "%s needs an entity, and %s is a relation", verb, FormatExpr(e))
	}
	return t, err
}

// fieldOf returns the type of field name of an entity with kind k (nil if
// unknown), or nil if it can only be known at run time.
func fieldOf(at Pos, k *model.Kind, name string, added map[string]*value.Type) (*value.Type, error) {
	if t, ok := added[name]; ok {
		return t, nil
	}
	if k == nil {
		return value.TypeAny, nil
	}
	if fd, ok := k.Field(name); ok {
		return fd.Type, nil
	}
	if k.Closed {
		return nil, errAt(at, "unknown_field", "kind %s has no field %s%s", k.Name, name, didYouMean(name, fieldNames(k.Fields)))
	}
	return value.TypeAny, nil // a field the entity may have added
}

func (c *checker) update(s *Update, outer *scope) error {
	t, err := c.entityTarget(s.Target, outer, "update")
	if err != nil {
		return err
	}
	sc := newScope()
	if outer != nil {
		sc = outer.clone()
	}
	sc.target = &t
	sc.added = map[string]*value.Type{}
	for _, a := range s.Actions {
		switch a := a.(type) {
		case *SetAction:
			for _, as := range a.Assigns {
				if err := c.assign(as, sc); err != nil {
					return err
				}
			}
		case *UnsetAction:
			for _, n := range a.Names {
				ft, err := fieldOf(s.P, t.Kind, n, sc.added)
				if err != nil {
					return err
				}
				if t.Kind != nil {
					if _, declared := t.Kind.Field(n); declared && !ft.Nullable() {
						return errAt(s.P, "constraint", "%s is a required field of %s; it can't be unset", n, t.Kind.Name)
					}
				}
			}
		case *AddAction:
			if err := c.adds(a.Fields, t.Kind, sc); err != nil {
				return err
			}
		case *DropAction:
			for _, n := range a.Names {
				if t.Kind != nil {
					if _, declared := t.Kind.Field(n); declared {
						return errAt(s.P, "constraint", "%s is declared by kind %s and can't be dropped from one entity; use unset, or alter kind %s drop %s", n, t.Kind.Name, t.Kind.Name, n)
					}
				}
				delete(sc.added, n)
			}
		}
	}
	return nil
}

// metaWritable are the metadata properties update can change.
var metaWritable = map[string]*value.Type{
	"labels": value.SetOf(value.TypeText),
	"props":  value.MapOf(value.TypeText, value.TypeAny),
}

func (c *checker) assign(as *Assign, sc *scope) error {
	lv := as.Target
	var t *value.Type
	if lv.Meta {
		var ok bool
		if t, ok = metaWritable[lv.Name]; !ok {
			if _, known := entityMeta[lv.Name]; known {
				return errAt(as.P, "constraint", "::%s is read-only; only ::labels and ::props can be changed", lv.Name)
			}
			return errAt(as.P, "not_found", "no metadata ::%s%s", lv.Name, didYouMean(lv.Name, slices.Collect(maps.Keys(entityMeta))))
		}
	} else {
		var err error
		if t, err = fieldOf(as.P, sc.target.Kind, lv.Name, sc.added); err != nil {
			return err
		}
	}
	for _, st := range lv.Steps {
		if st.Index != nil {
			it, err := c.expr(st.Index, sc)
			if err != nil {
				return err
			}
			if t, err = indexType(st.Index.Position(), t, it); err != nil {
				return err
			}
			continue
		}
		var err error
		if t, err = memberType(as.P, t, st.Field); err != nil {
			return err
		}
	}
	return c.assignOp(as, t, sc)
}

func (c *checker) assignOp(as *Assign, t *value.Type, sc *scope) error {
	switch as.Op {
	case "=":
		return c.write(as.Value, t, sc)
	case "+=", "-=", "*=", "/=":
		r, err := c.expr(as.Value, sc)
		if err != nil {
			return err
		}
		op := as.Op[:1]
		res, err := arith(as.P, op, t, r.asValue(), nil, as.Value)
		if err != nil {
			return err
		}
		if !assignable(res, t) {
			return typeErr(as.P, "%s %s %s gives %s, which can't be stored in %s", t, op, r, res, t)
		}
		return nil
	case "++=", "--=":
		lt := value.Required(t)
		if dynamic(lt) {
			_, err := c.expr(as.Value, sc)
			return err
		}
		switch lt.Code {
		case value.CodeText, value.CodeBytes, value.CodeList, value.CodeSet, value.CodeMap:
		default:
			if as.Op == "++=" && isNum(lt) {
				return typeErr(as.P, "++= joins text, bytes and collections; use += to add numbers")
			}
			return typeErr(as.P, "%s works on text, bytes, lists, sets and maps, not %s", as.Op, t)
		}
		want := lt
		if as.Op == "--=" {
			switch lt.Code {
			case value.CodeText, value.CodeBytes:
				return typeErr(as.P, "--= removes from lists, sets and maps, not %s", t)
			case value.CodeMap:
				want = value.SetOf(lt.Key) // keys to remove
			}
		}
		return c.write(as.Value, want, sc)
	}
	return errAt(as.P, "syntax", "unknown assignment %s", as.Op)
}

// node checks one end of a relate or unrelate.
func (c *checker) node(at Pos, n NodeRef, sc *scope) (Static, error) {
	t, err := c.target(n.Target, sc)
	if err != nil || n.Field == "" {
		return t, err
	}
	if t.Form == FormRelation {
		return t, typeErr(at, "relations don't have fields")
	}
	if _, err := fieldOf(at, t.Kind, n.Field, nil); err != nil {
		return t, err
	}
	return Static{Form: FormNode}, nil
}

// oneType checks an edge in relate or unrelate: exactly one relation type,
// no variable, no depth (spec/rhizome.md §15.5).
func oneType(e *Edge, verb string) error {
	switch {
	case e.AnyType || len(e.Types) != 1:
		return errAt(e.P, "syntax", "%s needs exactly one relation type", verb)
	case e.Var != "":
		return errAt(e.P, "syntax", "%s can't bind a relation variable; use as @handle after the statement", verb)
	case e.HasDepth:
		return errAt(e.P, "syntax", "%s works on one relation, so it can't have a depth like *2", verb)
	case e.Dir == DirEither:
		return errAt(e.P, "syntax", "%s needs a direction: ~%s~> or <~%s~", verb, e.Types[0], e.Types[0])
	}
	return nil
}

func (c *checker) relate(s *Relate, sc *scope) error {
	if err := oneType(s.Edge, "relate"); err != nil {
		return err
	}
	from, err := c.node(s.P, s.From, sc)
	if err != nil {
		return err
	}
	to, err := c.node(s.P, s.To, sc)
	if err != nil {
		return err
	}
	if s.Edge.Dir == DirIn {
		from, to = to, from
	}
	rt, _ := c.env.Catalog.RelationType(s.Edge.Types[0])
	if rt != nil && rt.Defined {
		if err := c.endpointFits(s.Edge.P, rt, "start", rt.From, from); err != nil {
			return err
		}
		if err := c.endpointFits(s.Edge.P, rt, "end", rt.To, to); err != nil {
			return err
		}
	}
	return c.relProps(s.Edge, rt, sc, true)
}

// endpointFits checks a relation end against the type's endpoint rule
// where the static type is known.
func (c *checker) endpointFits(at Pos, rt *model.RelationType, end string, ep model.Endpoint, n Static) error {
	switch ep.Mode {
	case model.EndpointKind:
		k, _ := c.env.Catalog.KindByID(ep.Kind)
		if n.Form == FormEntity && n.Kind != nil && k != nil && n.Kind.ID != k.ID {
			return errAt(at, "constraint", "%s relations must %s at a %s, not %s", rt.Name, end, k.Name, n)
		}
	case model.EndpointField:
		if n.Form == FormEntity {
			return errAt(at, "constraint", "%s relations must %s at a field: write x.field", rt.Name, end)
		}
	}
	return nil
}

// relProps checks relation properties: on relate they are values to
// write, in a pattern they are values to compare.
func (c *checker) relProps(e *Edge, rt *model.RelationType, sc *scope, write bool) error {
	if e.Props == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, en := range e.Props.Entries {
		name, ok := entryName(en)
		if !ok {
			return errAt(en.Key.Position(), "syntax", "relation property names must be names or text")
		}
		if seen[name] {
			return errAt(en.Key.Position(), "constraint", "property %s is given twice", name)
		}
		seen[name] = true
		t := value.TypeAny
		if rt != nil && rt.Defined {
			fd, ok := rt.Prop(name)
			if !ok {
				return errAt(en.Key.Position(), "unknown_field", "relation %s has no property %s%s", rt.Name, name, didYouMean(name, fieldNames(rt.Props)))
			}
			t = fd.Type
		}
		if write {
			if err := c.write(en.Value, t, sc); err != nil {
				return err
			}
			continue
		}
		vt, err := c.expr(en.Value, sc)
		if err != nil {
			return err
		}
		if err := comparable(en.Value.Position(), "=", valueOf(t), vt); err != nil {
			return err
		}
	}
	return nil
}

// entryName returns the name of a map entry written as name: or "name":.
func entryName(en Entry) (string, bool) {
	l, ok := en.Key.(*Lit)
	if !ok {
		return "", false
	}
	s, ok := l.Val.(value.Text)
	return s.Go(), ok
}

func (c *checker) unrelate(s *Unrelate, sc *scope) error {
	if s.Target != nil {
		t, err := c.target(s.Target, sc)
		if err == nil && t.Form == FormEntity {
			err = typeErr(s.P, "unrelate with one target removes a relation, and %s is an entity; write unrelate a ~type~> b", FormatExpr(s.Target))
		}
		return err
	}
	if err := oneType(s.Edge, "unrelate"); err != nil {
		return err
	}
	if s.Edge.Props != nil {
		return errAt(s.Edge.P, "syntax", "unrelate removes every relation of the type between the two nodes; it can't filter on properties")
	}
	if _, err := c.node(s.P, *s.From, sc); err != nil {
		return err
	}
	_, err := c.node(s.P, *s.To, sc)
	return err
}

// fieldRef checks target.field and returns the entity and field types.
func (c *checker) fieldRef(at Pos, f FieldRef, sc *scope) (Static, *value.Type, error) {
	t, err := c.entityTarget(f.Target, sc, "this")
	if err != nil {
		return t, nil, err
	}
	ft, err := fieldOf(at, t.Kind, f.Field, nil)
	return t, ft, err
}

// queueType returns the queue type of a field, or nil if only known at
// run time.
func queueType(at Pos, f FieldRef, ft *value.Type) (*value.Type, error) {
	q := value.Required(ft)
	if dynamic(q) {
		return nil, nil
	}
	if !q.Code.IsQueue() {
		return nil, typeErr(at, "%s is %s, not a queue", f.Field, ft)
	}
	return q, nil
}

func (c *checker) push(s *Push, sc *scope) error {
	_, ft, err := c.fieldRef(s.P, s.Field, sc)
	if err != nil {
		return err
	}
	q, err := queueType(s.P, s.Field, ft)
	if err != nil {
		return err
	}
	elem := value.TypeAny
	if q != nil {
		elem = q.Elem
		switch {
		case q.Code == value.CodePriority && s.Priority == nil:
			return errAt(s.P, "constraint", "%s is a priority queue: give a priority, push … priority n", s.Field.Field)
		case q.Code != value.CodePriority && s.Priority != nil:
			return errAt(s.Priority.Position(), "constraint", "%s is a %s, which has no priorities", s.Field.Field, q.Code)
		}
	}
	if err := c.write(s.Value, elem, scopeOr(sc)); err != nil {
		return err
	}
	if s.Priority != nil {
		return c.write(s.Priority, value.TypeInt, scopeOr(sc))
	}
	return nil
}

func scopeOr(sc *scope) *scope {
	if sc == nil {
		return newScope()
	}
	return sc
}

// pop checks a pop and returns the type of what it gives.
func (c *checker) pop(s *Pop, sc *scope) (Static, error) {
	if s.As != "" && sc == nil {
		return Static{}, errAt(s.P, "syntax", "pop … as names a variable, so it only works as a pipeline step")
	}
	_, ft, err := c.fieldRef(s.P, s.Field, sc)
	if err != nil {
		return Static{}, err
	}
	q, err := queueType(s.P, s.Field, ft)
	if err != nil {
		return Static{}, err
	}
	elem := value.TypeAny
	if q != nil {
		elem = value.OptionalOf(q.Elem)
	}
	if s.Count != nil {
		if err := c.write(s.Count, value.TypeInt, scopeOr(sc)); err != nil {
			return Static{}, err
		}
		elem = value.ListOf(value.Required(elem))
	}
	if s.Wait != nil {
		if err := c.write(s.Wait, value.TypeDuration, scopeOr(sc)); err != nil {
			return Static{}, err
		}
	}
	return valueOf(elem), nil
}
