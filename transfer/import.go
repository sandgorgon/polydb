package transfer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// ImportOptions configure an import.
type ImportOptions struct {
	// NewIDs gives every entity, field and relation a new ID, and
	// rewrites the refs (ref<…> values) that point at them. Plain id
	// values are kept. Without it, IDs are kept, and an ID that exists
	// already is an error.
	NewIDs bool
}

// An import transaction writes up to batchSize entities and relations,
// or about batchBytes of their records, so each durable commit carries
// a good amount of work.
const (
	batchSize  = 4000
	batchBytes = 8 << 20
)

// ImportDatabase reads a database export from r into db. Definitions db
// lacks are created; ones it has must be the same. Entities and
// relations are written in batches of transactions, so a failed import
// leaves the batches before the failure, which the error counts.
func ImportDatabase(ctx context.Context, db *engine.DB, r io.Reader, opts ImportOptions) (*Stats, error) {
	rr, err := newRecordReader(r)
	if err != nil {
		return nil, err
	}
	rr.formats = func(name string) (value.Format, bool) { return db.Catalog().Format(name) }
	im := &importer{ctx: ctx, db: db, opts: opts, rr: rr, st: &Stats{}, uploads: map[[32]byte]*engine.Upload{}}
	if opts.NewIDs {
		im.ids = map[value.ID]value.ID{}
	}
	err = im.run()
	if err != nil {
		err = fmt.Errorf("after %d entities and %d relations: %w", im.st.Entities, im.st.Relations, err)
	}
	return im.st, err
}

type importer struct {
	ctx     context.Context
	db      *engine.DB
	opts    ImportOptions
	rr      *recordReader
	st      *Stats
	uploads map[[32]byte]*engine.Upload
	ids     map[value.ID]value.ID // NewIDs: old → new

	batch                []func(*engine.Tx) error
	batchEnts, batchRels int64
	batchSize            int // bytes, roughly
}

func (im *importer) run() error {
	head, err := im.rr.next()
	if err != nil {
		return fmt.Errorf("reading the header: %w", unexpected(err))
	}
	if what, _ := textField(head, "polydb"); what != "database" {
		return errors.New("this isn't a database export")
	}
	if f, _ := intField(head, "format"); f != exportFormat {
		return fmt.Errorf("an export in format %d, which this version doesn't read", f)
	}
	var defs []string
	var m value.Map
	for {
		if m, err = im.rr.next(); err != nil {
			return unexpected(err)
		}
		if kindOf(m) != "define" {
			break
		}
		s, err := textField(m, "define")
		if err != nil {
			return err
		}
		defs = append(defs, s)
	}
	if im.st.Definitions, err = applyDefinitions(im.ctx, im.db, defs); err != nil {
		return err
	}
	for {
		if err := im.ctx.Err(); err != nil {
			return err
		}
		switch kindOf(m) {
		case "blob":
			err = im.blob(m)
		case "entity":
			err = im.entity(m)
		case "relation":
			err = im.relation(m)
		case "end":
			if err := im.flush(); err != nil {
				return err
			}
			ents, _ := intField(m, "entities")
			rels, _ := intField(m, "relations")
			if ents != im.st.Entities || rels != im.st.Relations {
				return fmt.Errorf("the export says %d entities and %d relations, and holds %d and %d",
					ents, rels, im.st.Entities, im.st.Relations)
			}
			return nil
		default:
			err = fmt.Errorf("unexpected %s record", kindOf(m))
		}
		if err != nil {
			return err
		}
		if m, err = im.rr.next(); err != nil {
			if err == io.EOF {
				return errors.New("the export ends early (no end record)")
			}
			return err
		}
	}
}

// applyDefinitions creates the definitions db lacks and checks the ones
// it has against the export's. It returns how many it created.
func applyDefinitions(ctx context.Context, db *engine.DB, defs []string) (int, error) {
	type subject struct{ what, name string }
	var stmts []lang.Stmt
	var subjects []subject
	for _, src := range defs {
		ss, err := lang.ParseScript(src)
		if err != nil || len(ss) != 1 {
			return 0, fmt.Errorf("definition %q: %v", src, err)
		}
		var sub subject
		switch s := ss[0].(type) {
		case *lang.DefineFormat:
			sub = subject{"format", s.Name}
		case *lang.DefineKind:
			sub = subject{"kind", s.Name}
		case *lang.AlterKind:
			if s.AddField == nil {
				return 0, fmt.Errorf("definition %q: only alter kind … add field may follow a kind's definition", src)
			}
			sub = subject{"kind", s.Name}
		case *lang.DefineRelation:
			sub = subject{"relation", s.Name}
		case *lang.DefineIndex:
			sub = subject{"index", s.Name}
		default:
			// An export is untrusted input: it may only define things.
			return 0, fmt.Errorf("an export may only hold definitions, not %q", src)
		}
		stmts, subjects = append(stmts, ss[0]), append(subjects, sub)
	}

	// Run them in a scratch database, which checks them and gives the
	// definitions they make, to compare with db's.
	scratch, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1})
	if err != nil {
		return 0, err
	}
	defer scratch.Close()
	sdb, err := scratch.CreateDatabase("import")
	if err != nil {
		return 0, err
	}
	if err := runDefs(ctx, sdb, stmts, nil); err != nil {
		return 0, fmt.Errorf("the export's definitions: %w", err)
	}
	cur, mine := db.Catalog(), sdb.Catalog()
	missing := map[subject]bool{}
	for _, sub := range subjects {
		want, _ := canonical(mine, sub.what, sub.name)
		have, ok := canonical(cur, sub.what, sub.name)
		switch {
		case !ok:
			missing[sub] = true
		case have != want:
			return 0, fmt.Errorf("%s %s differs: the database has\n  %s\nand the export\n  %s", sub.what, sub.name, have, want)
		}
	}
	var run []lang.Stmt
	created := 0
	for i, s := range stmts {
		if missing[subjects[i]] {
			run = append(run, s)
			if _, isAlter := s.(*lang.AlterKind); !isAlter {
				created++
			}
		}
	}
	if len(run) == 0 {
		return 0, nil
	}
	return created, runDefs(ctx, db, run, nil)
}

// runDefs runs definition statements in one transaction.
func runDefs(ctx context.Context, db *engine.DB, stmts []lang.Stmt, _ any) error {
	return db.Update(func(tx *engine.Tx) error {
		for _, s := range stmts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := query.Exec(&query.Context{Tx: tx}, s); err != nil {
				return fmt.Errorf("%s: %w", lang.Format(s), err)
			}
		}
		return nil
	})
}

// --- blobs ---

// dataReader reads a blob's content from the data records that follow
// its blob record.
type dataReader struct {
	rr   *recordReader
	left int64 // bytes the blob still has
	buf  []byte
}

func (d *dataReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.left == 0 {
			return 0, io.EOF
		}
		m, err := d.rr.next()
		if err != nil {
			return 0, unexpected(err)
		}
		if kindOf(m) != "data" {
			return 0, fmt.Errorf("a blob ends early, at a %s record", kindOf(m))
		}
		b, ok := field(m, "data").(value.Bytes)
		if !ok {
			return 0, errors.New("a data record without bytes")
		}
		d.buf, _, _ = value.Large(b)
		if int64(len(d.buf)) > d.left {
			return 0, errors.New("a blob holds more data than its length")
		}
		d.left -= int64(len(d.buf))
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

// uploadTTL keeps an import's blobs from being collected before the
// entities using them are written.
const uploadTTL = 24 * time.Hour

func (im *importer) blob(m value.Map) error {
	hs, err := textField(m, "blob")
	if err != nil {
		return err
	}
	h, err := hex.DecodeString(hs)
	if err != nil || len(h) != 32 {
		return fmt.Errorf("blob %q isn't a SHA-256", hs)
	}
	typ, _ := textField(m, "type")
	of := map[string]value.Code{"text": value.CodeText, "bytes": value.CodeBytes, "image": value.CodeImage}[typ]
	if of == 0 {
		return fmt.Errorf("blob %s of type %q", hs[:12], typ)
	}
	n, err := intField(m, "len")
	if err != nil || n < 0 || n > value.MaxBytesLen {
		return fmt.Errorf("blob %s: bad length", hs[:12])
	}
	u, err := im.db.StoreUpload(&dataReader{rr: im.rr, left: n}, of, value.MaxBytesLen, uploadTTL)
	if err != nil {
		return err
	}
	if u.Hash != [32]byte(h) || u.Len != n {
		return fmt.Errorf("blob %s doesn't match its content", hs[:12])
	}
	im.uploads[u.Hash] = u
	im.st.Blobs++
	return nil
}

// --- entities and relations ---

// id maps an exported ID to the one to write.
func (im *importer) id(old value.ID) value.ID {
	if im.ids == nil {
		return old
	}
	if n, ok := im.ids[old]; ok {
		return n
	}
	n := im.db.NewID()
	im.ids[old] = n
	return n
}

// fix prepares a field value: a large value from its blob, refs mapped
// to new IDs.
func (im *importer) fix(v value.Value) (value.Value, error) {
	if r, ok := v.(value.BlobRef); ok {
		u, ok := im.uploads[r.Hash]
		if !ok {
			return nil, fmt.Errorf("a field holds blob %x, which the export doesn't hold before it", r.Hash[:6])
		}
		return im.db.UploadValue(u, r.Media)
	}
	if im.ids == nil {
		return v, nil
	}
	return remapRefs(v, im.id)
}

func (im *importer) entity(m value.Map) error {
	old, err := idField(m, "entity")
	if err != nil {
		return err
	}
	ie := engine.ImportedEntity{ID: im.id(old)}
	if ie.Kind, err = textField(m, "kind"); err != nil {
		return err
	}
	meta := &ie.Meta
	var ok bool
	if meta.Created, ok = field(m, "created").(value.Time); !ok {
		return fmt.Errorf("entity %s: no created time", old)
	}
	if meta.Updated, ok = field(m, "updated").(value.Time); !ok {
		meta.Updated = meta.Created
	}
	v, err := intField(m, "version")
	if err != nil {
		return err
	}
	meta.Version = uint64(max(v, 1))
	if meta.Owner, err = textField(m, "owner"); err != nil {
		return err
	}
	if l := field(m, "labels"); l != nil {
		if meta.Labels, ok = l.(value.Set); !ok {
			return fmt.Errorf("entity %s: labels aren't a set", old)
		}
	}
	if meta.Props, err = mapField(m, "props"); err != nil {
		return err
	}
	fields, err := mapField(m, "fields")
	if err != nil {
		return err
	}
	ids, err := mapField(m, "field_ids")
	if err != nil {
		return err
	}
	added, err := mapField(m, "added")
	if err != nil {
		return err
	}
	for k, fid := range ids.All() {
		name, _ := k.(value.Text)
		id, ok := fid.(value.ID)
		if !ok {
			return fmt.Errorf("entity %s: field %s has no id", old, name.Go())
		}
		f := engine.ImportedField{ID: im.id(id), Name: name.Go(), Value: value.Null}
		if v, ok := fields.Get(k); ok {
			if f.Value, err = im.fix(v); err != nil {
				return fmt.Errorf("entity %s field %s: %w", old, f.Name, err)
			}
		}
		if d, ok := added.Get(k); ok {
			if f.Def, err = fieldDefOf(d); err != nil {
				return fmt.Errorf("entity %s field %s: %w", old, f.Name, err)
			}
		}
		ie.Fields = append(ie.Fields, f)
	}
	im.batch = append(im.batch, func(tx *engine.Tx) error { return tx.ImportEntity(ie) })
	im.batchEnts++
	im.batchSize += len(value.AppendValue(nil, fields)) + 64
	return im.maybeFlush()
}

// fieldDefOf reads an added field's definition.
func fieldDefOf(v value.Value) (*model.FieldDef, error) {
	m, ok := v.(value.Map)
	if !ok {
		return nil, errors.New("its definition isn't a map")
	}
	ts, _ := textField(m, "type")
	t, err := value.ParseType(ts)
	if err != nil {
		return nil, err
	}
	fd := &model.FieldDef{Type: t}
	fd.Doc, _ = textField(m, "doc")
	fd.Check, _ = textField(m, "check")
	fd.Default, _ = textField(m, "default")
	if b, _ := field(m, "history").(value.Bool); b {
		fd.History.On = true
		if d, ok := field(m, "keep").(value.Duration); ok {
			fd.History.Keep = time.Duration(d)
		}
	}
	if od, _ := textField(m, "on_delete"); od == "restrict" {
		fd.OnDelete = model.RefRestrict
	}
	return fd, nil
}

func (im *importer) relation(m value.Map) error {
	old, err := idField(m, "relation")
	if err != nil {
		return err
	}
	typ, err := textField(m, "type")
	if err != nil || typ == "" {
		return fmt.Errorf("relation %s has no type", old)
	}
	from, err := idField(m, "from")
	if err != nil {
		return err
	}
	to, err := idField(m, "to")
	if err != nil {
		return err
	}
	if im.ids != nil {
		f, okf := im.ids[from]
		t, okt := im.ids[to]
		if !okf || !okt {
			return fmt.Errorf("relation %s connects nodes the export doesn't hold", old)
		}
		from, to = f, t
	}
	pm, err := mapField(m, "props")
	if err != nil {
		return err
	}
	props := engine.Fields{}
	for k, v := range pm.All() {
		t, ok := k.(value.Text)
		if !ok {
			return fmt.Errorf("relation %s: a property name isn't text", old)
		}
		if props[t.Go()], err = im.fix(v); err != nil {
			return err
		}
	}
	id := im.id(old)
	im.batch = append(im.batch, func(tx *engine.Tx) error { return tx.ImportRelation(id, from, typ, to, props) })
	im.batchRels++
	return im.maybeFlush()
}

func (im *importer) maybeFlush() error {
	if len(im.batch) >= batchSize || im.batchSize >= batchBytes {
		return im.flush()
	}
	return nil
}

// flush writes the batch in one transaction, retrying conflicts.
func (im *importer) flush() error {
	if len(im.batch) == 0 {
		return nil
	}
	err := im.db.Retry(5, func(tx *engine.Tx) error {
		for _, fn := range im.batch {
			if err := fn(tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	im.st.Entities += im.batchEnts
	im.st.Relations += im.batchRels
	im.batch, im.batchEnts, im.batchRels, im.batchSize = im.batch[:0], 0, 0, 0
	return nil
}

// remapRefs rewrites the refs in v with id.
func remapRefs(v value.Value, id func(value.ID) value.ID) (value.Value, error) {
	all := func(vs []value.Value) ([]value.Value, error) {
		out := make([]value.Value, len(vs))
		for i, e := range vs {
			var err error
			if out[i], err = remapRefs(e, id); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	switch x := v.(type) {
	case value.Ref:
		return value.Ref{Target: id(x.Target)}, nil
	case value.List:
		vs, err := all(x.Values())
		if err != nil {
			return nil, err
		}
		return value.NewList(vs...)
	case value.Array:
		vs, err := all(x.Values())
		if err != nil {
			return nil, err
		}
		return value.NewArray(vs...)
	case value.Set:
		vs, err := all(x.Values())
		if err != nil {
			return nil, err
		}
		return value.NewSet(vs...)
	case value.Map:
		ks, err := all(x.Keys())
		if err != nil {
			return nil, err
		}
		var vals []value.Value
		for _, v := range x.All() {
			vals = append(vals, v)
		}
		if vals, err = all(vals); err != nil {
			return nil, err
		}
		return value.NewMap(ks, vals)
	case value.Pair:
		vs, err := all([]value.Value{x.Key(), x.Value()})
		if err != nil {
			return nil, err
		}
		return value.NewPair(vs[0], vs[1])
	case value.Record:
		vs, err := all(func() []value.Value {
			var out []value.Value
			for _, v := range x.All() {
				out = append(out, v)
			}
			return out
		}())
		if err != nil {
			return nil, err
		}
		return value.NewRecord(x.Names(), vs)
	case value.Queue:
		vs, err := all(x.Items())
		if err != nil {
			return nil, err
		}
		var prios []int64
		if x.Code() == value.CodePriority {
			prios = x.Priorities()
		}
		return value.NewQueueOf(&value.Type{Code: x.Code(), N: x.Limit(), LowFirst: x.LowFirst()}, vs, prios)
	}
	return v, nil
}
