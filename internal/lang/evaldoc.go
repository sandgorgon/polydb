package lang

import (
	"strings"

	"github.com/sandgorgon/polydb/value"
)

// Document paths (spec/rhizome.md §8.6).

// parsed returns a document's parsed value, cached for the statement.
func (ev *Evaluator) parsed(e Expr, d value.Doc) (value.Value, string, error) {
	f, ok := ev.Graph.Format(d.Format())
	if !ok {
		return nil, "", runErr(e, "not_found", "no format %s", d.Format())
	}
	base := f
	for {
		df, ok := base.(*value.DerivedFormat)
		if !ok {
			break
		}
		base = df.Base()
	}
	k := docKey{d.Format(), d.Text()}
	if v, ok := ev.docs[k]; ok {
		return v, base.Name(), nil
	}
	v, err := f.Parse(d.Text())
	if err != nil {
		return nil, "", runErr(e, "type", "the %s document doesn't parse: %v", d.Format(), err)
	}
	if ev.docs == nil {
		ev.docs = map[docKey]value.Value{}
	}
	ev.docs[k] = v
	return v, base.Name(), nil
}

// docPath evaluates a chain of -> steps and reports whether the last step
// found something.
func (ev *Evaluator) docPath(e *DocPath, f *Frame) (value.Value, bool, error) {
	var steps []*DocPath
	var base Expr = e
	for {
		d, ok := base.(*DocPath)
		if !ok {
			break
		}
		steps = append(steps, d)
		base = d.X
	}
	v, err := ev.Eval(base, f)
	if err != nil {
		return nil, false, err
	}
	if value.IsNull(v) {
		return value.Null, false, nil
	}
	d, ok := v.(value.Doc)
	if !ok {
		return nil, false, runErr(e, "type", "-> reads inside documents, not %s", typeName(v))
	}
	cur, format, err := ev.parsed(base, d)
	if err != nil {
		return nil, false, err
	}
	found := true
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		var idx value.Value
		if st.Step.Index != nil {
			if idx, err = ev.Eval(st.Step.Index, f); err != nil {
				return nil, false, err
			}
		}
		if cur, found, err = docStep(st, format, cur, st.Step, idx); err != nil {
			return nil, false, err
		}
		if !found {
			return value.Null, false, nil
		}
	}
	return cur, found, nil
}

// docStep takes one step from v. idx is the evaluated [index], if any.
func docStep(e Expr, format string, v value.Value, st DocStep, idx value.Value) (value.Value, bool, error) {
	if format == "xml" {
		if el, ok := v.(value.Record); ok && isElement(el) {
			return xmlStep(e, el, st, idx)
		}
		return value.Null, false, nil
	}
	if st.Attr {
		return nil, false, runErr(e, "type", "@%s is an XML attribute step, and this is a %s document", st.Name, format)
	}
	if st.Index == nil {
		idx = value.MustText(st.Name)
	}
	if value.IsNull(idx) {
		return value.Null, false, nil
	}
	switch v.(type) {
	case value.List, value.Array:
		if _, ok := idx.(value.Int); !ok {
			return value.Null, false, nil
		}
	case value.Map, value.Record:
		if _, ok := idx.(value.Text); !ok {
			return value.Null, false, nil
		}
	default:
		return value.Null, false, nil
	}
	return index(e, v, idx)
}

// isElement reports whether r is an XML element record.
func isElement(r value.Record) bool {
	names := r.Names()
	return len(names) == 3 && names[0] == "name" && names[1] == "attrs" && names[2] == "children"
}

func children(el value.Record) []value.Value {
	c, _ := el.Get("children")
	return seqValues(c)
}

// xmlStep steps from an element: a child element by name, an attribute,
// or the nth child node.
func xmlStep(e Expr, el value.Record, st DocStep, idx value.Value) (value.Value, bool, error) {
	switch {
	case st.Attr:
		attrs, _ := el.Get("attrs")
		if m, ok := attrs.(value.Map); ok {
			v, ok := m.Get(value.MustText(st.Name))
			return orNull(v), ok, nil
		}
		return value.Null, false, nil
	case st.Index != nil:
		n, ok := idx.(value.Int)
		if !ok {
			if value.IsNull(idx) {
				return value.Null, false, nil
			}
			return nil, false, runErr(e, "type", "xml position steps take an int, not %s", typeName(idx))
		}
		kids := children(el)
		if n < 0 || int64(n) >= int64(len(kids)) {
			return value.Null, false, nil
		}
		return kids[n], true, nil
	}
	for _, k := range children(el) {
		if r, ok := k.(value.Record); ok && isElement(r) {
			if name, _ := r.Get("name"); name != nil && name.(value.Text).Go() == st.Name {
				return r, true, nil
			}
		}
	}
	return value.Null, false, nil
}

// innerText joins the text inside an XML element, depth first.
func innerText(v value.Value) string {
	var b strings.Builder
	var walk func(value.Value)
	walk = func(v value.Value) {
		switch x := v.(type) {
		case value.Text:
			b.WriteString(x.Go())
		case value.Record:
			if isElement(x) {
				for _, k := range children(x) {
					walk(k)
				}
			}
		}
	}
	walk(v)
	return b.String()
}
