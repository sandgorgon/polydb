// Package transfer exports and imports databases, query results and
// security settings (spec/rhizome.md §4, spec/types.md §12.3).
package transfer

import (
	"slices"

	"github.com/sandgorgon/polydb/internal/catalog"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// Definitions are exported as Rhizome statements, which import runs like
// any define: readable, editable, and checked by the same code.

// definitions returns the statements that define cat's formats, kinds,
// defined relation types and indexes, in an order that runs: a
// definition comes after what it names. A kind whose fields name kinds
// in a cycle is defined without those fields, which alter kind adds
// once every kind exists.
func definitions(cat *catalog.Catalog) []string {
	var out []string
	add := func(s lang.Stmt) { out = append(out, lang.Format(s)) }

	formats := cat.Formats()
	done := map[string]bool{}
	for len(formats) > 0 {
		i := max(slices.IndexFunc(formats, func(f *model.Format) bool { return satisfied(f.Fields, value.CodeDoc, done, f.Name) }), 0)
		add(formatStmt(formats[i]))
		done[formats[i].Name] = true
		formats = slices.Delete(formats, i, i+1)
	}

	kinds := cat.Kinds()
	defined := map[string]bool{}
	var later []lang.Stmt
	for len(kinds) > 0 {
		i := slices.IndexFunc(kinds, func(k *model.Kind) bool { return satisfied(k.Fields, value.CodeRef, defined, k.Name) })
		k := kinds[max(i, 0)]
		kinds = slices.Delete(kinds, max(i, 0), max(i, 0)+1)
		s := kindStmt(k)
		if i < 0 {
			// A cycle: leave out the fields naming kinds not defined yet.
			var now []*lang.FieldDef
			for j, fd := range k.Fields {
				if satisfied(k.Fields[j:j+1], value.CodeRef, defined, k.Name) {
					now = append(now, s.Fields[j])
				} else {
					later = append(later, &lang.AlterKind{Name: k.Name, AddField: fieldDef(&fd)})
				}
			}
			s.Fields = now
		}
		add(s)
		defined[k.Name] = true
	}
	for _, s := range later {
		add(s)
	}
	for _, rt := range cat.RelationTypes() {
		if rt.Defined {
			add(relationStmt(cat, rt))
		}
	}
	for _, ix := range cat.Indexes() {
		add(indexStmt(cat, ix))
	}
	return out
}

// satisfied reports whether the types of fields name only things done
// already, or self, among names of code c (kinds or formats).
func satisfied(fields []model.FieldDef, c value.Code, done map[string]bool, self string) bool {
	ok := true
	for _, fd := range fields {
		if fd.Type == nil {
			continue
		}
		fd.Type.Names(func(nc value.Code, n string) {
			isKind := nc == value.CodeRef
			if (c == value.CodeRef) == isKind && n != self && !done[n] {
				if c == value.CodeDoc {
					if _, builtin := value.LookupFormat(n); builtin {
						return
					}
				}
				ok = false
			}
		})
	}
	return ok
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func history(h model.History) *lang.HistoryOpt {
	if !h.On {
		return nil
	}
	o := &lang.HistoryOpt{}
	if h.Keep > 0 {
		o.Keep = &lang.Lit{Val: value.Duration(h.Keep)}
	}
	return o
}

// expr parses an expression the catalog keeps as Rhizome text.
func expr(src string) lang.Expr {
	if src == "" {
		return nil
	}
	e, err := lang.ParseExpr(src)
	if err != nil {
		// The catalog only holds expressions that parsed; keep the text
		// visible rather than drop it.
		return &lang.Lit{Val: value.MustText(src)}
	}
	return e
}

func fieldDef(fd *model.FieldDef) *lang.FieldDef {
	d := &lang.FieldDef{Name: fd.Name, Type: fd.Type, Doc: strp(fd.Doc), Unique: fd.Unique,
		Default: expr(fd.Default), Check: expr(fd.Check), History: history(fd.History), Column: fd.Column}
	if fd.OnDelete == model.RefRestrict {
		d.OnDelete = "restrict"
	}
	return d
}

func fieldDefs(fds []model.FieldDef) []*lang.FieldDef {
	out := make([]*lang.FieldDef, len(fds))
	for i := range fds {
		out[i] = fieldDef(&fds[i])
	}
	return out
}

func kindStmt(k *model.Kind) *lang.DefineKind {
	return &lang.DefineKind{Name: k.Name, Doc: strp(k.Doc), Closed: k.Closed, History: history(k.History),
		Fields: fieldDefs(k.Fields)}
}

func formatStmt(f *model.Format) *lang.DefineFormat {
	return &lang.DefineFormat{Name: f.Name, Base: f.Base, Doc: strp(f.Doc), Header: f.Header, Fields: fieldDefs(f.Fields)}
}

func endpointName(cat *catalog.Catalog, ep model.Endpoint) string {
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

func relationStmt(cat *catalog.Catalog, rt *model.RelationType) *lang.DefineRelation {
	s := &lang.DefineRelation{Name: rt.Name, From: endpointName(cat, rt.From), To: endpointName(cat, rt.To),
		Doc: strp(rt.Doc), Props: fieldDefs(rt.Props)}
	if rt.MaxOut > 0 {
		s.MaxOut = &rt.MaxOut
	}
	if rt.MaxIn > 0 {
		s.MaxIn = &rt.MaxIn
	}
	if rt.OnDelete == model.RelRestrict {
		s.OnDelete = "restrict"
	}
	return s
}

func indexStmt(cat *catalog.Catalog, ix *model.Index) *lang.DefineIndex {
	s := &lang.DefineIndex{Name: ix.Name, Unique: ix.Unique, Text: ix.Text}
	if k, ok := cat.KindByID(ix.Kind); ok {
		s.Kind = k.Name
	}
	for _, k := range ix.Keys {
		key := lang.IndexKey{Field: k.Field}
		for _, st := range k.Path {
			switch {
			case st.IsPos:
				key.Path = append(key.Path, lang.DocStep{Index: &lang.Lit{Val: value.Int(st.Pos)}})
			case st.Attr || value.IsIdentifier(st.Name):
				key.Path = append(key.Path, lang.DocStep{Name: st.Name, Attr: st.Attr})
			default:
				key.Path = append(key.Path, lang.DocStep{Index: &lang.Lit{Val: value.MustText(st.Name)}})
			}
		}
		s.Keys = append(s.Keys, key)
	}
	return s
}

// canonical returns the one statement that defines a whole definition,
// for comparing an export's definitions with a database's.
func canonical(cat *catalog.Catalog, what, name string) (string, bool) {
	switch what {
	case "format":
		if f, ok := cat.FormatDef(name); ok {
			return lang.Format(formatStmt(f)), true
		}
	case "kind":
		if k, ok := cat.Kind(name); ok {
			return lang.Format(kindStmt(k)), true
		}
	case "relation":
		if rt, ok := cat.RelationType(name); ok && rt.Defined {
			return lang.Format(relationStmt(cat, rt)), true
		}
	case "index":
		if ix, ok := cat.Index(name); ok {
			return lang.Format(indexStmt(cat, ix)), true
		}
	}
	return "", false
}
