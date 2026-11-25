package query

import (
	"slices"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

func (x *executor) definition(s lang.Stmt) (*Result, error) {
	var err error
	switch s := s.(type) {
	case *lang.DefineKind:
		var k model.Kind
		if k, err = x.kindDef(s); err == nil {
			_, err = x.tx.DefineKind(k)
		}
	case *lang.AlterKind:
		err = x.alterKind(s)
	case *lang.Rebuild:
		err = x.tx.RebuildIndex(s.Index)
	case *lang.DefineRelation:
		var d engine.RelationDef
		if d, err = x.relationDef(s); err == nil {
			_, err = x.tx.DefineRelation(d)
		}
	case *lang.AlterRelation:
		err = x.alterRelation(s)
	case *lang.DefineFormat:
		f := model.Format{Name: s.Name, Base: s.Base, Doc: str(s.Doc), Header: s.Header}
		if f.Fields, err = x.fieldDefs(s.Fields); err == nil {
			_, err = x.tx.DefineFormat(f)
		}
	case *lang.DefineIndex:
		ks := make([]model.IndexKey, len(s.Keys))
		for i, k := range s.Keys {
			ks[i] = model.IndexKey{Field: k.Field}
			for _, st := range k.Path {
				ks[i].Path = append(ks[i].Path, pathStep(st))
			}
		}
		_, err = x.tx.DefineIndex(s.Name, s.Kind, s.Unique, s.Text, ks...)
	case *lang.Drop:
		switch s.What {
		case "kind":
			err = x.tx.DropKind(s.Name)
		case "relation":
			err = x.tx.DropRelation(s.Name)
		case "format":
			err = x.tx.DropFormat(s.Name)
		case "index":
			err = x.tx.DropIndex(s.Name)
		default:
			return nil, errAt(s, "unsupported", "users and roles arrive with security in a later phase")
		}
	}
	if err != nil {
		return nil, fail(s, err)
	}
	x.graph.reset()
	res := &Result{}
	if a, ok := s.(*lang.AlterKind); ok && x.tx.AlterStarted() {
		res.Warnings = append(res.Warnings, &lang.Error{Pos: a.Position(), Code: "background",
			Msg: "kind " + a.Name + " is large, so it is being altered in the background: writes to it wait until that finishes (show alters)"})
	}
	return res, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pathStep converts a constant doc step (checked to be a name, a quoted
// name, an attribute or a literal position or key).
func pathStep(st lang.DocStep) model.PathStep {
	if st.Index != nil {
		switch v := st.Index.(*lang.Lit).Val.(type) {
		case value.Int:
			return model.PathStep{IsPos: true, Pos: int(v)}
		case value.Text:
			return model.PathStep{Name: v.Go()}
		}
	}
	return model.PathStep{Name: st.Name, Attr: st.Attr}
}

// constant evaluates a constant expression (checked to use no rows).
func (x *executor) constant(e lang.Expr) (value.Value, error) {
	v, err := x.ev.Eval(e, nil)
	return v, fail(e, err)
}

func (x *executor) history(h *lang.HistoryOpt) (model.History, error) {
	if h == nil {
		return model.History{}, nil
	}
	out := model.History{On: true}
	if h.Keep != nil {
		v, err := x.constant(h.Keep)
		if err != nil {
			return out, err
		}
		if d, ok := v.(value.Duration); ok {
			out.Keep = time.Duration(d)
		}
	}
	return out, nil
}

// fieldDef converts a field definition. Defaults and checks are kept as
// canonical Rhizome text; the engine compiles them.
func (x *executor) fieldDef(d *lang.FieldDef) (model.FieldDef, error) {
	fd := model.FieldDef{Name: d.Name, Type: d.Type, Doc: str(d.Doc), Unique: d.Unique, Column: d.Column}
	if d.Default != nil {
		fd.Default = lang.FormatExpr(d.Default)
	}
	if d.Check != nil {
		fd.Check = lang.FormatExpr(d.Check)
	}
	if d.OnDelete == "restrict" {
		fd.OnDelete = model.RefRestrict
	}
	var err error
	fd.History, err = x.history(d.History)
	return fd, err
}

func (x *executor) fieldDefs(ds []*lang.FieldDef) ([]model.FieldDef, error) {
	out := make([]model.FieldDef, len(ds))
	for i, d := range ds {
		var err error
		if out[i], err = x.fieldDef(d); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (x *executor) kindDef(s *lang.DefineKind) (model.Kind, error) {
	k := model.Kind{Name: s.Name, Doc: str(s.Doc), Closed: s.Closed}
	var err error
	if k.History, err = x.history(s.History); err != nil {
		return k, err
	}
	k.Fields, err = x.fieldDefs(s.Fields)
	return k, err
}

func (x *executor) alterKind(s *lang.AlterKind) error {
	var fd model.FieldDef
	if d := s.AddField; d != nil {
		var err error
		if fd, err = x.fieldDef(d); err != nil {
			return err
		}
	} else if d := s.Alter; d != nil {
		var err error
		if fd, err = x.fieldDef(d); err != nil {
			return err
		}
	}
	return x.tx.AlterKind(s.Name, func(k *model.Kind) error {
		i := slices.IndexFunc(k.Fields, func(f model.FieldDef) bool { return f.Name == fd.Name || f.Name == s.DropField })
		switch {
		case s.AddField != nil:
			k.Fields = append(k.Fields, fd)
		case s.Alter != nil:
			k.Fields[i] = fd
		case s.DropField != "":
			k.Fields = slices.Delete(k.Fields, i, i+1)
		case s.RenameTo != "":
			k.Name = s.RenameTo
		case s.SetClosed != nil:
			k.Closed = *s.SetClosed
		}
		return nil
	})
}

func (x *executor) relationDef(s *lang.DefineRelation) (engine.RelationDef, error) {
	d := engine.RelationDef{Name: s.Name, Doc: str(s.Doc), From: s.From, To: s.To}
	applyRelOption(&d, &lang.RelOption{MaxOut: s.MaxOut, MaxIn: s.MaxIn, OnDelete: s.OnDelete})
	var err error
	d.Props, err = x.fieldDefs(s.Props)
	return d, err
}

func applyRelOption(d *engine.RelationDef, o *lang.RelOption) {
	if o.Doc != nil {
		d.Doc = *o.Doc
	}
	if o.MaxOut != nil {
		d.MaxOut = *o.MaxOut
	}
	if o.MaxIn != nil {
		d.MaxIn = *o.MaxIn
	}
	switch o.OnDelete {
	case "restrict":
		d.OnDelete = model.RelRestrict
	case "cascade":
		d.OnDelete = model.RelCascade
	}
}

// alterRelation rebuilds the relation's definition with one change.
func (x *executor) alterRelation(s *lang.AlterRelation) error {
	cat := x.tx.Catalog()
	rt, _ := cat.RelationType(s.Name) // the checker made sure it exists
	endpoint := func(ep model.Endpoint) string {
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
	d := engine.RelationDef{Name: rt.Name, Doc: rt.Doc, From: endpoint(rt.From), To: endpoint(rt.To),
		MaxOut: rt.MaxOut, MaxIn: rt.MaxIn, OnDelete: rt.OnDelete, Props: slices.Clone(rt.Props)}
	switch {
	case s.AddProp != nil:
		fd, err := x.fieldDef(s.AddProp)
		if err != nil {
			return err
		}
		d.Props = append(d.Props, fd)
	case s.Alter != nil:
		fd, err := x.fieldDef(s.Alter)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(d.Props, func(f model.FieldDef) bool { return f.Name == fd.Name })
		d.Props[i] = fd
	case s.DropProp != "":
		d.Props = slices.DeleteFunc(d.Props, func(f model.FieldDef) bool { return f.Name == s.DropProp })
	case s.RenameTo != "":
		d.Name = s.RenameTo
	case s.SetOption != nil:
		applyRelOption(&d, s.SetOption)
	}
	return x.tx.AlterRelation(s.Name, d)
}
