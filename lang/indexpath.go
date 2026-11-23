package lang

import (
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// IndexValue applies an index key's path (define index … on K(f->a->b),
// spec/rhizome.md §5.4) to a document, with the rules of -> steps
// (§8.6). ok is false when the path finds nothing or the document
// doesn't parse; such entities aren't indexed by that key.
func IndexValue(f value.Format, d value.Doc, path []model.PathStep) (value.Value, bool) {
	cur, err := f.Parse(d.Text())
	if err != nil {
		return nil, false
	}
	base := f
	for {
		df, ok := base.(*value.DerivedFormat)
		if !ok {
			break
		}
		base = df.Base()
	}
	at := &DocPath{}
	for _, p := range path {
		st := DocStep{Name: p.Name, Attr: p.Attr}
		var idx value.Value
		if p.IsPos {
			idx = value.Int(int64(p.Pos))
			st.Index = &Lit{Val: idx}
		}
		var found bool
		if cur, found, err = docStep(at, base.Name(), cur, st, idx); err != nil || !found {
			return nil, false
		}
	}
	if value.IsNull(cur) {
		return nil, false
	}
	return cur, true
}
