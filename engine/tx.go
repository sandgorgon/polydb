package engine

import (
	"bytes"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/blob"
	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/txn"
	"github.com/sandgorgon/polydb/value"
)

// Tx is a transaction on one database (spec/rhizome.md §9): it sees the
// database as it was when it began plus its own changes. Definition
// changes made in a transaction roll back with it. A Tx is not safe for
// concurrent use.
type Tx struct {
	db   *DB
	t    *txn.Txn
	base *catalog.Catalog
	edit *catalog.Edit
	user string
	now  time.Time

	guarded    bool // the catalog version key is guarded
	failed     error
	startBuild bool             // it defined an index to build in the background
	changed    map[value.ID]int // entities saved per kind, for statistics

	blobsPut  map[[32]byte]bool // blobs stored or reused (blob.go)
	blobSync  []blob.Loc        // chunks to sync before committing
	commitSeq *uint64           // set to the commit's sequence number
}

// Catalog returns the catalog as this transaction sees it, including its
// own uncommitted definition changes.
func (tx *Tx) Catalog() *catalog.Catalog {
	if tx.edit != nil {
		return tx.edit.Catalog()
	}
	return tx.base
}

// ReadOnly reports whether the transaction is read-only.
func (tx *Tx) ReadOnly() bool { return tx.t.ReadOnly() }

// Commit commits the transaction. On a conflict it returns an error with
// CodeConflict; the transaction can then be retried from the start.
func (tx *Tx) Commit() error {
	if tx.failed != nil {
		tx.t.Rollback()
		return tx.failed
	}
	var newCat *catalog.Catalog
	if tx.edit != nil && tx.edit.Changed() {
		var err error
		if newCat, err = tx.edit.Commit(tx.t); err != nil {
			tx.t.Rollback()
			return wrap(CodeInternal, err)
		}
		tx.t.Set(keys.CatalogVersion, value.Encode(value.Int(newCat.Version())))
	}
	if err := tx.syncBlobs(); err != nil {
		tx.t.Rollback()
		return err
	}
	seq, err := tx.t.Commit()
	if err != nil {
		code := CodeInternal
		if err == txn.ErrReadOnly {
			code = CodeConstraint
		}
		return wrap(code, err)
	}
	if tx.commitSeq != nil {
		*tx.commitSeq = seq
	}
	if newCat != nil {
		tx.db.setCatalog(newCat)
	}
	if tx.startBuild {
		tx.db.kickBuilder()
	}
	for kind, n := range tx.changed {
		tx.db.changes(kind).Add(int64(n))
	}
	return nil
}

// Rollback discards the transaction. It is safe to call after Commit.
func (tx *Tx) Rollback() { tx.t.Rollback() }

// writable checks the transaction may write, and guards the catalog
// version so a concurrent definition change makes one of the two fail.
func (tx *Tx) writable() error {
	if tx.t.ReadOnly() {
		return errorf(CodeConstraint, "the transaction is read-only")
	}
	if !tx.guarded {
		tx.t.GuardKey(keys.CatalogVersion)
		tx.guarded = true
	}
	return nil
}

// editing returns the transaction's catalog edit, starting one if needed.
func (tx *Tx) editing() (*catalog.Edit, error) {
	if err := tx.writable(); err != nil {
		return nil, err
	}
	if tx.edit == nil {
		tx.edit = tx.base.Edit(tx.db.eng.ids)
	}
	return tx.edit, nil
}

// fail records an error that leaves the transaction's buffered writes
// inconsistent; Commit will refuse.
func (tx *Tx) fail(err error) error {
	if tx.failed == nil {
		tx.failed = err
	}
	return err
}

func (tx *Tx) newID() value.ID { return tx.db.eng.ids.New() }

// Now returns the transaction's time: when it began, from the engine's
// clock. Every statement in the transaction sees the same time.
func (tx *Tx) Now() time.Time { return tx.timeNow().Go() }

func (tx *Tx) timeNow() value.Time {
	t, err := value.NewTime(tx.now)
	if err != nil {
		t, _ = value.NewTime(time.Unix(0, 0))
	}
	return t
}

// --- low-level reads ---

func (tx *Tx) get(k []byte) ([]byte, bool, error) {
	v, ok, err := tx.t.Get(k)
	if err != nil {
		return nil, false, wrap(CodeInternal, err)
	}
	return v, ok, nil
}

// scan calls fn for each key with the given prefix. If guard is set, the
// range is guarded so the result stays valid until commit.
func (tx *Tx) scan(prefix []byte, guard bool, fn func(k, v []byte) error) error {
	opts := keys.Range(prefix)
	if guard {
		tx.t.Guard(opts.Lower, opts.Upper)
	}
	it := tx.t.Iter(opts)
	defer it.Close()
	for it.Next() {
		if err := fn(it.Key(), it.Value()); err != nil {
			return err
		}
	}
	return wrap(CodeInternal, it.Err())
}

// node returns what id names.
func (tx *Tx) node(id value.ID, guard bool) (keys.NodeRef, bool, error) {
	k := keys.Node(id)
	if guard {
		tx.t.GuardKey(k)
	}
	b, ok, err := tx.get(k)
	if err != nil || !ok {
		return keys.NodeRef{}, false, err
	}
	n, err := keys.DecodeNode(b)
	return n, true, wrap(CodeInternal, err)
}

// --- entity state ---

// entState is an entity being changed: loaded, modified in memory, then
// saved with save, which writes only what changed and keeps the side
// records (kind membership, node records, unique and back-ref keys) in
// step.
type entState struct {
	e        *model.Entity
	kind     *model.Kind // nil for kindless
	origKind *model.Kind
	fields   map[string]*model.Field
	orig     map[string]*model.Field // as loaded; nil for a new entity
	isNew    bool
	deleted  bool
	// blobs names the loaded fields whose records held blob references.
	blobs map[string]bool
}

func (tx *Tx) loadEntity(id value.ID) (*entState, error) { return tx.loadEntityAs(id, true) }

// loadEntityAs loads an entity, leaving large values as blob references
// unless resolve is set. Such a state is only for reading.
func (tx *Tx) loadEntityAs(id value.ID, resolve bool) (*entState, error) {
	b, ok, err := tx.get(keys.Entity(id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeNotFound, "no entity %s", id)
	}
	e, err := keys.DecodeEntity(id, b)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	st := &entState{e: e, fields: map[string]*model.Field{}, orig: map[string]*model.Field{}}
	if !e.Kind.IsZero() {
		k, ok := tx.Catalog().KindByID(e.Kind)
		if !ok {
			return nil, errorf(CodeInternal, "entity %s has unknown kind %s", id, e.Kind)
		}
		st.kind, st.origKind = k, k
	}
	cat := tx.Catalog()
	err = tx.scan(keys.Fields(id), false, func(k, v []byte) error {
		_, name, err := keys.ParseField(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		f, err := keys.DecodeField(id, name, v, cat)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if _, ok := f.Value.(value.BlobRef); ok {
			if !resolve {
				st.fields[name], st.orig[name] = f, f
				return nil
			}
			if f.Value, err = tx.resolve(f.Value); err != nil {
				return err
			}
			if st.blobs == nil {
				st.blobs = map[string]bool{}
			}
			st.blobs[name] = true
		}
		st.fields[name], st.orig[name] = f, f
		return nil
	})
	return st, err
}

// fieldDef returns the definition of a field of st: from the kind for
// declared fields, from the field itself for added ones.
func fieldDef(kind *model.Kind, f *model.Field) *model.FieldDef {
	if f.Declared && kind != nil {
		if fd, ok := kind.Field(f.Name); ok {
			return fd
		}
	}
	return f.Def
}

// names returns the entity's field names plus the kind's declared names,
// for "did you mean" suggestions.
func (st *entState) names() []string {
	var out []string
	for n := range st.fields {
		out = append(out, n)
	}
	if st.kind != nil {
		for _, fd := range st.kind.Fields {
			out = append(out, fd.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// save writes the changes in st.
func (tx *Tx) save(st *entState) error {
	if err := tx.writable(); err != nil {
		return err
	}
	cat := tx.Catalog()
	id := st.e.ID
	if !st.deleted {
		if err := tx.emptyQueues(st); err != nil {
			return err
		}
		if err := checkRequired(st); err != nil {
			return err
		}
		if err := tx.checkEntity(st); err != nil {
			return err
		}
	}

	// What the changed fields' records hold: large values go to the blob
	// store first, and history versions hold the same references.
	stored := map[string]value.Value{}
	if !st.deleted {
		for name, f := range st.fields {
			if old, ok := st.orig[name]; (ok && old == f) || f.Value == nil {
				continue
			}
			v, err := tx.storeValue(id, f.Value)
			if err != nil {
				return err
			}
			stored[name] = v
		}
	}
	if err := tx.droppedBlobs(st); err != nil {
		return err
	}
	if err := tx.recordHistory(st, stored); err != nil {
		return err
	}
	if k := st.kind; k != nil || st.origKind != nil {
		if k == nil || st.deleted {
			k = st.origKind
		}
		if tx.changed == nil {
			tx.changed = map[value.ID]int{}
		}
		tx.changed[k.ID]++
	}

	// Side keys: compute before and after, then write the difference.
	var before, after map[string][]byte
	if !st.isNew {
		before = tx.sideKeys(st.origKind, st.e, st.orig)
	}
	if !st.deleted {
		after = tx.sideKeys(st.kind, st.e, st.fields)
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			tx.t.Delete([]byte(k))
		}
	}
	for k, v := range after {
		if old, ok := before[k]; ok && bytes.Equal(old, v) {
			continue
		}
		if k[0] == keys.PrefixUnique {
			tx.t.GuardKey([]byte(k))
			if cur, ok, err := tx.get([]byte(k)); err != nil {
				return err
			} else if ok && !bytes.Equal(cur, id[:]) {
				return errorf(CodeConstraint, "unique value already used by entity %s", value.ID(cur))
			}
		}
		tx.t.Set([]byte(k), v)
	}

	// Fields.
	for name, old := range st.orig {
		if f, ok := st.fields[name]; !ok || st.deleted {
			tx.t.Delete(keys.Field(id, name))
			tx.t.Delete(keys.Node(old.ID))
		} else if f.ID != old.ID {
			tx.t.Delete(keys.Node(old.ID))
		}
	}
	if !st.deleted {
		for name, f := range st.fields {
			if old, ok := st.orig[name]; ok && old == f {
				continue // unchanged
			}
			out := f
			if ref, ok := stored[name].(value.BlobRef); ok {
				cp := *f
				cp.Value = ref
				out = &cp
			}
			rec, err := keys.EncodeField(out, cat)
			if err != nil {
				return wrap(CodeInternal, err)
			}
			tx.t.Set(keys.Field(id, name), rec)
			if old, ok := st.orig[name]; !ok || old.ID != f.ID {
				tx.t.Set(keys.Node(f.ID), keys.EncodeNode(keys.NodeRef{Kind: model.NodeField, Entity: id, Name: name}))
			}
		}
	}

	// Header, membership and node record.
	if st.deleted {
		tx.t.Delete(keys.Entity(id))
		tx.t.Delete(keys.Node(id))
		if !st.e.Kind.IsZero() {
			tx.t.Delete(keys.Member(st.e.Kind, id))
		}
		return nil
	}
	st.e.Meta.Version++
	st.e.Meta.Updated = tx.timeNow()
	tx.t.Set(keys.Entity(id), keys.EncodeEntity(st.e))
	if st.isNew {
		tx.t.Set(keys.Node(id), keys.EncodeNode(keys.NodeRef{Kind: model.NodeEntity}))
		if !st.e.Kind.IsZero() {
			tx.t.Set(keys.Member(st.e.Kind, id), nil)
		}
	}
	return nil
}

// emptyQueues gives every absent declared queue field an empty queue, so
// queues never need a value on create (spec/types.md §8).
func (tx *Tx) emptyQueues(st *entState) error {
	if st.kind == nil {
		return nil
	}
	for i := range st.kind.Fields {
		fd := &st.kind.Fields[i]
		if !fd.Type.Code.IsQueue() {
			continue
		}
		f, ok := st.fields[fd.Name]
		if ok && f.Value != nil {
			continue
		}
		q, err := value.NewQueue(fd.Type)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		nf := &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: fd.Name, Declared: true}
		if ok {
			cp := *f
			nf = &cp
		}
		nf.Value = q
		st.fields[fd.Name] = nf
	}
	return nil
}

// checkRequired checks that every required declared field has a value.
func checkRequired(st *entState) error {
	if st.kind == nil {
		return nil
	}
	for _, fd := range st.kind.Fields {
		if fd.Type.Nullable() {
			continue
		}
		if f, ok := st.fields[fd.Name]; !ok || f.Value == nil {
			return errorf(CodeConstraint, "%s.%s is required", st.kind.Name, fd.Name)
		}
	}
	return nil
}

// sideKeys returns the unique and back-ref keys an entity's fields need,
// with their values.
func (tx *Tx) sideKeys(kind *model.Kind, e *model.Entity, fields map[string]*model.Field) map[string][]byte {
	out := map[string][]byte{}
	for _, f := range fields {
		fd := fieldDef(kind, f)
		if fd == nil || f.Value == nil {
			continue
		}
		if fd.OnDelete == model.RefRestrict {
			if r, ok := f.Value.(value.Ref); ok {
				out[string(keys.BackRef(r.Target, f.ID))] = nil
			}
		}
		if fd.Unique && f.Declared && kind != nil && !value.IsNull(f.Value) {
			if k, err := value.Key(f.Value); err == nil {
				out[string(keys.Unique(kind.ID, f.Name, k))] = e.ID[:]
			}
		}
	}
	if kind != nil {
		for _, ix := range tx.Catalog().IndexesOn(kind.ID) {
			if k, ok := uniqueIndexKey(ix, fields); ok {
				out[string(k)] = e.ID[:]
			}
			for k, v := range tx.indexEntries(ix, e.ID, fields) {
				out[k] = v
			}
		}
	}
	return out
}

// uniqueIndexKey returns the unique key of a unique index for the given
// fields, or false if the index isn't unique or a field is absent or null.
func uniqueIndexKey(ix *model.Index, fields map[string]*model.Field) ([]byte, bool) {
	if !ix.Unique {
		return nil, false
	}
	var vals []byte
	for _, k := range ix.Keys {
		f, ok := fields[k.Field]
		if !ok || f.Value == nil || value.IsNull(f.Value) || len(k.Path) > 0 {
			return nil, false
		}
		var err error
		if vals, err = value.AppendKey(vals, f.Value); err != nil {
			return nil, false
		}
	}
	return keys.Unique(ix.ID, "", vals), true
}
