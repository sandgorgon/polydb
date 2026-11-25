package transfer

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// exportFormat is the version of the export layout.
const exportFormat = 1

// A database export holds, in order:
//
//	{"polydb": "database", "format": 1, "database": …, "created": …}
//	{"define": "<a Rhizome define or alter statement>"}   …
//	{"blob": "<sha256>", "type": …, "len": …, "media": …}  before the
//	{"data": <bytes>}   …                                  first entity
//	                                                       using it
//	{"entity": <id>, "kind": …, "created": …, "updated": …, "version": …,
//	 "owner": …, "labels": …, "props": …,
//	 "fields": {name: value}, "field_ids": {name: id},
//	 "added": {name: {"type": …, "doc": …, "check": …, "default": …,
//	                  "history": …, "keep": …, "on_delete": …}}}  …
//	{"relation": <id>, "type": …, "from": <id>, "to": <id>, "props": …}  …
//	{"end": true, "entities": n, "relations": n, "blobs": n}
//
// A field holding a large value holds {"$blob": hash, …}, and the blob's
// content comes in data records before the entity, once per export.
// History versions aren't exported: an export is the current state.

// Stats counts what an export or import moved.
type Stats struct {
	Definitions int // created, on import
	Entities    int64
	Relations   int64
	Blobs       int64
	Bytes       int64 // written, on export
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ExportDatabase writes db as of one snapshot to w.
func ExportDatabase(ctx context.Context, db *engine.DB, w io.Writer, format Format) (*Stats, error) {
	cw := &countWriter{w: w}
	rw, err := newRecordWriter(cw, format)
	if err != nil {
		return nil, err
	}
	tx := db.Begin(engine.TxOptions{ReadOnly: true})
	defer tx.Rollback()
	cat := tx.Catalog()
	created, _ := value.NewTime(time.Now())
	if err := rw.write(rec(pair{"polydb", value.MustText("database")}, pair{"format", value.Int(exportFormat)},
		pair{"database", value.MustText(db.Name())}, pair{"created", created})); err != nil {
		return nil, err
	}
	for _, s := range definitions(cat) {
		if err := rw.write(rec(pair{"define", value.MustText(s)})); err != nil {
			return nil, err
		}
	}

	st := &Stats{}
	blobs := map[[32]byte]bool{}
	var failed error
	err = tx.AllEntities(func(id value.ID) bool {
		if st.Entities%256 == 0 {
			if failed = ctx.Err(); failed != nil {
				return false
			}
		}
		e, err := tx.Get(id)
		if err != nil {
			failed = err
			return false
		}
		if failed = writeEntity(rw, e, blobs, st); failed != nil {
			return false
		}
		st.Entities++
		return true
	})
	if err == nil {
		err = failed
	}
	if err != nil {
		return nil, err
	}
	err = tx.AllRelations(func(r *model.Relation) bool {
		if st.Relations%1024 == 0 {
			if failed = ctx.Err(); failed != nil {
				return false
			}
		}
		rt, _ := cat.RelationTypeByID(r.Type)
		var props value.Value
		if r.Props.Len() > 0 {
			props = r.Props
		}
		failed = rw.write(rec(pair{"relation", r.ID}, pair{"type", value.MustText(rt.Name)},
			pair{"from", r.From}, pair{"to", r.To}, pair{"props", props}))
		st.Relations++
		return failed == nil
	})
	if err == nil {
		err = failed
	}
	if err != nil {
		return nil, err
	}
	if err := rw.write(rec(pair{"end", value.Bool(true)}, pair{"entities", value.Int(st.Entities)},
		pair{"relations", value.Int(st.Relations)}, pair{"blobs", value.Int(st.Blobs)})); err != nil {
		return nil, err
	}
	if err := rw.flush(); err != nil {
		return nil, err
	}
	st.Bytes = cw.n
	return st, nil
}

// writeEntity writes an entity's record, after the blobs its large
// values need that the export hasn't written yet.
func writeEntity(rw *recordWriter, e *engine.Entity, blobs map[[32]byte]bool, st *Stats) error {
	var fks, fvs, iks, ivs, aks, avs []value.Value
	for _, f := range e.Fields {
		v := f.Value
		if l, ok := v.(value.Lazy); ok {
			if !blobs[l.Ref.Hash] {
				if err := writeBlob(rw, l); err != nil {
					return err
				}
				blobs[l.Ref.Hash] = true
				st.Blobs++
			}
			v = l.Ref
		}
		name := value.MustText(f.Name)
		if v != nil {
			fks, fvs = append(fks, name), append(fvs, v)
		}
		iks, ivs = append(iks, name), append(ivs, f.ID)
		if !f.Declared && f.Def != nil {
			aks, avs = append(aks, name), append(avs, addedDef(f.Def))
		}
	}
	m := e.Meta
	var kind, labels, props, added value.Value
	if e.Kind != nil {
		kind = value.MustText(e.Kind.Name)
	}
	if m.Labels.Len() > 0 {
		labels = m.Labels
	}
	if m.Props.Len() > 0 {
		props = m.Props
	}
	fields, err := value.NewMap(fks, fvs)
	if err != nil {
		return err
	}
	ids, err := value.NewMap(iks, ivs)
	if err != nil {
		return err
	}
	if len(aks) > 0 {
		if added, err = value.NewMap(aks, avs); err != nil {
			return err
		}
	}
	var owner value.Value
	if m.Owner != "" {
		owner = value.MustText(m.Owner)
	}
	return rw.write(rec(pair{"entity", e.ID}, pair{"kind", kind}, pair{"created", m.Created}, pair{"updated", m.Updated},
		pair{"version", value.Int(m.Version)}, pair{"owner", owner}, pair{"labels", labels}, pair{"props", props},
		pair{"fields", fields}, pair{"field_ids", ids}, pair{"added", added}))
}

// addedDef describes the definition of a field an entity added itself.
func addedDef(fd *model.FieldDef) value.Value {
	t := value.TypeAny
	if fd.Type != nil {
		t = fd.Type
	}
	ps := []pair{{"type", value.MustText(t.String())}}
	if fd.Doc != "" {
		ps = append(ps, pair{"doc", value.MustText(fd.Doc)})
	}
	if fd.Check != "" {
		ps = append(ps, pair{"check", value.MustText(fd.Check)})
	}
	if fd.Default != "" {
		ps = append(ps, pair{"default", value.MustText(fd.Default)})
	}
	if fd.History.On {
		ps = append(ps, pair{"history", value.Bool(true)})
		if fd.History.Keep > 0 {
			ps = append(ps, pair{"keep", value.Duration(fd.History.Keep)})
		}
	}
	if fd.OnDelete == model.RefRestrict {
		ps = append(ps, pair{"on_delete", value.MustText("restrict")})
	}
	return rec(ps...)
}

// blobPiece is how much of a large value one data record holds.
const blobPiece = 1 << 20

// writeBlob writes a large value's blob record and its content in data
// records, reading it a piece at a time.
func writeBlob(rw *recordWriter, l value.Lazy) error {
	var media value.Value
	if l.Ref.Media != "" {
		media = value.MustText(l.Ref.Media)
	}
	if err := rw.write(rec(pair{"blob", value.MustText(hex.EncodeToString(l.Ref.Hash[:]))},
		pair{"type", value.MustText(l.Ref.Of.String())}, pair{"len", value.Int(l.Ref.Len)}, pair{"media", media})); err != nil {
		return err
	}
	buf := make([]byte, blobPiece)
	for off := int64(0); off < l.Ref.Len; {
		n := min(int64(blobPiece), l.Ref.Len-off)
		if _, err := l.Src.ReadAt(buf[:n], off); err != nil {
			return fmt.Errorf("reading a large value: %w", err)
		}
		if err := rw.write(rec(pair{"data", value.BytesOf(buf[:n], "")})); err != nil {
			return err
		}
		off += n
	}
	return nil
}
