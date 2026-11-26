package engine

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// Default and check expressions (spec/rhizome.md §5.1) are compiled with
// the Rhizome checker and cached per catalog version.

type exprKey struct {
	version uint64
	check   bool
	src     string
	sig     string // the types the expression was checked against
}

// compiled returns a cached compilation, or compiles one. A transaction
// that changes definitions compiles against its own catalog without
// caching.
func (tx *Tx) compiled(k exprKey, compile func() (*lang.Compiled, error)) (*lang.Compiled, error) {
	if tx.edit != nil {
		return compile()
	}
	k.version = tx.Catalog().Version()
	if c, ok := tx.db.exprs.Load(k); ok {
		return c.(*lang.Compiled), nil
	}
	c, err := compile()
	if err != nil {
		return nil, err
	}
	tx.db.exprs.Store(k, c)
	return c, nil
}

func (tx *Tx) defaultExpr(fd *model.FieldDef) (*lang.Compiled, error) {
	c, err := tx.compiled(exprKey{src: fd.Default, sig: fd.Type.String()}, func() (*lang.Compiled, error) {
		return lang.CompileDefault(tx.Catalog(), fd.Default, fd.Type)
	})
	if err != nil {
		return nil, exprErr(fd.Name, "default", fd.Default, err)
	}
	return c, nil
}

func (tx *Tx) checkExpr(fd *model.FieldDef, types map[string]*value.Type) (*lang.Compiled, error) {
	var sig strings.Builder
	for _, n := range slices.Sorted(maps.Keys(types)) {
		sig.WriteString(n + ":" + types[n].String() + ",")
	}
	c, err := tx.compiled(exprKey{check: true, src: fd.Check, sig: sig.String()}, func() (*lang.Compiled, error) {
		return lang.CompileCheck(tx.Catalog(), fd.Check, types)
	})
	if err != nil {
		return nil, exprErr(fd.Name, "check", fd.Check, err)
	}
	return c, nil
}

// exprErr reports an expression error with the field and expression it
// belongs to, keeping the Rhizome error's code.
func exprErr(field, what, src string, err error) error {
	code := CodeType
	var le *lang.Error
	if errors.As(err, &le) {
		switch le.Code {
		case "unknown_field", "not_found", "constraint", "limit", "unsupported":
			code = Code(le.Code)
		}
		err = errors.New(le.Code + ": " + le.Msg)
	}
	var ee *Error
	if errors.As(err, &ee) {
		return ee
	}
	return &Error{Code: code, Msg: fmt.Sprintf("field %s: %s %s: %v", field, what, src, err), Err: err}
}

// fieldTypes returns the types of fields by name.
func fieldTypes(fields []model.FieldDef) map[string]*value.Type {
	out := make(map[string]*value.Type, len(fields))
	for i := range fields {
		out[fields[i].Name] = fields[i].Type
	}
	return out
}

// validateExprs compiles the defaults and checks of a set of field
// definitions, so that mistakes are reported when they are defined.
// Checks read the fields in scope.
func (tx *Tx) validateExprs(fields []model.FieldDef, scope map[string]*value.Type) error {
	for i := range fields {
		fd := &fields[i]
		if fd.Default != "" {
			if _, err := tx.defaultExpr(fd); err != nil {
				return err
			}
		}
		if fd.Check != "" {
			if _, err := tx.checkExpr(fd, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// evalDefault evaluates a field's default and conforms it to the field.
func (tx *Tx) evalDefault(fd *model.FieldDef) (value.Value, error) {
	c, err := tx.defaultExpr(fd)
	if err != nil {
		return nil, err
	}
	v, err := c.Eval(tx.Catalog(), tx.timeNow().Go(), nil, tx.maxMemory())
	if err != nil {
		return nil, exprErr(fd.Name, "default", fd.Default, err)
	}
	return tx.conform(fd, v)
}

// runCheck evaluates one check against field values. A field's check
// only applies while the field holds a value, so an optional field that is
// absent or null passes (spec/rhizome.md §5.1).
func (tx *Tx) runCheck(owner string, fd *model.FieldDef, types map[string]*value.Type, vals map[string]value.Value) error {
	if v := vals[fd.Name]; v == nil || value.IsNull(v) {
		return nil
	}
	c, err := tx.checkExpr(fd, types)
	if err != nil {
		return err
	}
	ok, err := c.Passes(tx.Catalog(), tx.timeNow().Go(), vals, tx.maxMemory())
	if err != nil {
		return exprErr(fd.Name, "check", fd.Check, err)
	}
	if !ok {
		return errorf(CodeConstraint, "%s: check failed for %s: %s", owner, fd.Name, fd.Check)
	}
	return nil
}

// checkEntity runs the checks of an entity's fields: the kind's checks,
// which read the declared fields, and the checks of fields the entity
// added, which read all its fields.
func (tx *Tx) checkEntity(st *entState) error {
	vals := map[string]value.Value{}
	all := map[string]*value.Type{}
	var declared map[string]*value.Type
	owner := "entity " + st.e.ID.String()
	if st.kind != nil {
		declared = fieldTypes(st.kind.Fields)
		maps.Copy(all, declared)
		owner = st.kind.Name + " " + st.e.ID.String()
	}
	for name, f := range st.fields {
		if f.Value != nil {
			vals[name] = f.Value
		}
		if def := fieldDef(st.kind, f); def != nil {
			all[name] = def.Type
		}
	}
	if st.kind != nil {
		for i := range st.kind.Fields {
			if fd := &st.kind.Fields[i]; fd.Check != "" {
				if err := tx.runCheck(owner, fd, declared, vals); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(st.fields)) {
		f := st.fields[name]
		if !f.Declared && f.Def != nil && f.Def.Check != "" {
			if err := tx.runCheck(owner, f.Def, all, vals); err != nil {
				return err
			}
		}
	}
	return nil
}

// tableDefaults evaluates the defaults of a table format's columns into
// their DefaultValue, for definitions given as Rhizome text.
func (tx *Tx) tableDefaults(f *model.Format) error {
	if !f.IsTable() {
		return nil
	}
	f.Fields = slices.Clone(f.Fields)
	for i := range f.Fields {
		fd := &f.Fields[i]
		if fd.Default == "" || fd.DefaultValue != nil {
			continue
		}
		v, err := tx.evalDefault(fd)
		if err != nil {
			return err
		}
		fd.DefaultValue = v
	}
	return nil
}
