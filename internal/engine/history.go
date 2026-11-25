package engine

import (
	"time"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/value"
)

// Opt-in field history (spec/rhizome.md §10). When a field with history
// changes, its new value is also written as a version keyed by the
// transaction's time (keys.History). Entities of a kind with history also
// get a life record, so that queries as of an earlier time find them
// after they are deleted. Two transactions can't both change one field
// (the second conflicts), so a field's versions are in commit order.

// historyOf is the history setting of a field: the kind's, overridden
// by the field's own.
func historyOf(kind *model.Kind, def *model.FieldDef) model.History {
	var h model.History
	if kind != nil && kind.History.On {
		h = kind.History
	}
	if def != nil && def.History.On {
		h.On = true
		if def.History.Keep > 0 {
			h.Keep = def.History.Keep
		}
	}
	return h
}

// recordHistory writes the versions and life record for the changes in
// st. save calls it before writing the entity.
// stored holds what the changed fields' records hold (blob references
// for large values).
func (tx *Tx) recordHistory(st *entState, stored map[string]value.Value) error {
	now := tx.timeNow()
	id := st.e.ID
	kind := st.kind
	if st.deleted {
		kind = st.origKind
	}
	tracked := kind != nil && kind.History.On

	if st.deleted {
		if !tracked {
			// Nothing can read the versions of a deleted entity whose
			// kind has no history.
			p := keys.HistoryEntity(id)
			tx.t.DeleteRange(p, storage.PrefixEnd(p))
			return nil
		}
		life, err := tx.life(id)
		if err != nil {
			return err
		}
		if life == nil {
			life = &keys.LifeRecord{Kind: kind.ID, Created: st.e.Meta.Created}
		}
		life.Deleted = &now
		tx.t.Set(keys.Life(id), keys.EncodeLife(*life))
		return nil
	}

	if st.isNew && tracked {
		tx.t.Set(keys.HistKind(kind.ID, id), nil)
		tx.t.Set(keys.Life(id), keys.EncodeLife(keys.LifeRecord{Kind: kind.ID, Created: now}))
	}
	for name, f := range st.fields {
		old, had := st.orig[name]
		if had && old == f {
			continue
		}
		h := historyOf(kind, fieldDef(kind, f))
		if !h.On || (had && sameValue(old.Value, f.Value)) {
			continue
		}
		v := f.Value
		if sv, ok := stored[name]; ok {
			v = sv
		}
		if err := tx.writeVersion(id, name, v, h, now); err != nil {
			return err
		}
	}
	for name, old := range st.orig {
		if _, ok := st.fields[name]; ok {
			continue
		}
		if h := historyOf(st.origKind, fieldDef(st.origKind, old)); h.On && old.Value != nil {
			if err := tx.writeVersion(id, name, nil, h, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameValue(a, b value.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return value.Equal(a, b) && a.Code() == b.Code()
}

// writeVersion records a field's value from now on, and drops versions
// that were replaced longer ago than the field keeps them.
func (tx *Tx) writeVersion(id value.ID, name string, v value.Value, h model.History, now value.Time) error {
	tx.t.Set(keys.History(id, name, now), keys.EncodeVersion(v))
	if h.Keep <= 0 {
		return nil
	}
	cutoff := now.Go().Add(-h.Keep)
	var stale [][]byte
	var prev []byte
	err := tx.scan(keys.HistoryPrefix(id, name), false, func(k, _ []byte) error {
		t, err := keys.HistoryTime(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if prev != nil && !t.Go().After(cutoff) {
			stale = append(stale, prev) // replaced at or before the cutoff
		}
		prev = append([]byte(nil), k...)
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range stale {
		tx.t.Delete(k)
	}
	if len(stale) > 0 {
		return tx.markBlobsDirty(id)
	}
	return nil
}

func (tx *Tx) life(id value.ID) (*keys.LifeRecord, error) {
	b, ok, err := tx.get(keys.Life(id))
	if err != nil || !ok {
		return nil, err
	}
	l, err := keys.DecodeLife(b)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	return &l, nil
}

// historyBaseline records the current value of every entity's field when
// alter turns its history on, so that the history starts complete.
func (tx *Tx) historyBaseline(old, nk *model.Kind) error {
	var on []string
	for _, fd := range nk.Fields {
		before := model.History{}
		if ofd, ok := old.Field(fd.Name); ok {
			before = historyOf(old, ofd)
		}
		if h := historyOf(nk, &fd); h.On && !before.On {
			on = append(on, fd.Name)
		}
	}
	if len(on) == 0 {
		return nil
	}
	now := tx.timeNow()
	return tx.scan(keys.Members(nk.ID), false, func(k, _ []byte) error {
		_, id, err := keys.ParseMember(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		for _, name := range on {
			var v value.Value
			if b, ok, err := tx.get(keys.Field(id, name)); err != nil {
				return err
			} else if ok {
				f, err := keys.DecodeField(id, name, b, tx.Catalog())
				if err != nil {
					return wrap(CodeInternal, err)
				}
				v = f.Value
			}
			tx.t.Set(keys.History(id, name, now), keys.EncodeVersion(v))
		}
		return nil
	})
}

// Version is one stored version of a field: its value from At until the
// next version. A nil Value means the field was absent.
type Version struct {
	At    value.Time
	Value value.Value
}

// versions reads a field's stored versions, oldest first.
func (tx *Tx) versions(id value.ID, name string) ([]Version, error) {
	var out []Version
	err := tx.scan(keys.HistoryPrefix(id, name), false, func(k, v []byte) error {
		t, err := keys.HistoryTime(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		val, err := keys.DecodeVersion(v)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if val, err = tx.lazy(val); err != nil {
			return err
		}
		out = append(out, Version{t, val})
		return nil
	})
	return out, err
}

// fieldHistory finds a field's history setting: from the entity's kind
// and field definition, or, for a field that no longer exists, from its
// stored versions.
func (tx *Tx) fieldHistory(id value.ID, kind *model.Kind, f *model.Field, name string) (model.History, error) {
	var def *model.FieldDef
	if f != nil {
		def = fieldDef(kind, f)
	} else if kind != nil {
		if fd, ok := kind.Field(name); ok {
			def = fd
		}
	}
	if h := historyOf(kind, def); h.On {
		return h, nil
	}
	if def == nil {
		// A dropped field may have left versions behind.
		found := false
		err := tx.scan(keys.HistoryPrefix(id, name), false, func(k, v []byte) error {
			found = true
			return errStopScan
		})
		if err != nil && err != errStopScan {
			return model.History{}, err
		}
		return model.History{On: found}, nil
	}
	return model.History{}, nil
}

var errStopScan = errorf(CodeInternal, "stop")

// FieldHistory returns a field's versions within its retention, oldest
// first (the history statement). The field must have history.
func (tx *Tx) FieldHistory(id value.ID, name string) ([]Version, error) {
	st, err := tx.loadEntity(id)
	if err != nil {
		return nil, err
	}
	h, err := tx.fieldHistory(id, st.kind, st.fields[name], name)
	if err != nil {
		return nil, err
	}
	if !h.On {
		return nil, noHistory(name)
	}
	vs, err := tx.versions(id, name)
	if err != nil {
		return nil, err
	}
	return keep(vs, h, tx.Now()), nil
}

// keep drops the versions replaced longer ago than h keeps them.
func keep(vs []Version, h model.History, now time.Time) []Version {
	if h.Keep <= 0 {
		return vs
	}
	cutoff := now.Add(-h.Keep)
	i := 0
	for i+1 < len(vs) && !vs[i+1].At.Go().After(cutoff) {
		i++
	}
	return vs[i:]
}

func noHistory(name string) error {
	return errorf(CodeNotFound, "%s has no history (turn it on with the field option history)", name)
}

// AsOf reads entities as they were at a past time (find … as of). Only
// fields with history can be read; entities that didn't exist then
// aren't seen. It caches what it reads, for one statement.
type AsOf struct {
	tx   *Tx
	at   value.Time
	ents map[value.ID]*pastEntity
}

type pastEntity struct {
	exists bool // at the time
	kind   *model.Kind
	cur    *entState // nil once deleted
}

// AsOf returns a reader of the database as it was at t.
func (tx *Tx) AsOf(t value.Time) *AsOf {
	return &AsOf{tx: tx, at: t, ents: map[value.ID]*pastEntity{}}
}

// At returns the time it reads at.
func (a *AsOf) At() value.Time { return a.at }

func (a *AsOf) entity(id value.ID) (*pastEntity, error) {
	if p, ok := a.ents[id]; ok {
		return p, nil
	}
	p := &pastEntity{}
	life, err := a.tx.life(id)
	if err != nil {
		return nil, err
	}
	if n, ok, err := a.tx.Exists(id); err != nil {
		return nil, err
	} else if ok && n == model.NodeEntity {
		if p.cur, err = a.tx.loadEntity(id); err != nil {
			return nil, err
		}
		p.kind = p.cur.kind
	}
	at := a.at.Go()
	switch {
	case life != nil:
		if k, ok := a.tx.Catalog().KindByID(life.Kind); ok {
			p.kind = k
		}
		p.exists = !life.Created.Go().After(at) && (life.Deleted == nil || life.Deleted.Go().After(at))
	case p.cur != nil:
		p.exists = !p.cur.e.Meta.Created.Go().After(at)
	}
	a.ents[id] = p
	return p, nil
}

// Entity reports whether id was an entity at the time, and its kind (nil
// for kindless).
func (a *AsOf) Entity(id value.ID) (*model.Kind, bool, error) {
	p, err := a.entity(id)
	if err != nil {
		return nil, false, err
	}
	return p.kind, p.exists, nil
}

// Created returns when an entity that existed at the time was created.
func (a *AsOf) Created(id value.ID) (value.Time, error) {
	p, err := a.entity(id)
	if err != nil {
		return value.Time{}, err
	}
	if p.cur != nil {
		return p.cur.e.Meta.Created, nil
	}
	life, err := a.tx.life(id)
	if err != nil || life == nil {
		return value.Time{}, err
	}
	return life.Created, nil
}

// Field reads a field's value at the time. ok is false if it was absent.
// A field without history can't be read, nor a time older than the
// history it keeps.
func (a *AsOf) Field(id value.ID, name string) (value.Value, bool, error) {
	p, err := a.entity(id)
	if err != nil || !p.exists {
		return value.Null, false, err
	}
	var f *model.Field
	if p.cur != nil {
		f = p.cur.fields[name]
	}
	h, err := a.tx.fieldHistory(id, p.kind, f, name)
	if err != nil {
		return nil, false, err
	}
	if !h.On {
		return nil, false, noHistory(name)
	}
	if h.Keep > 0 && a.at.Go().Before(a.tx.Now().Add(-h.Keep)) {
		return nil, false, errorf(CodeNotFound, "%s keeps history for %s, so it can't be read as of %s",
			name, value.Duration(h.Keep), a.at)
	}
	vs, err := a.tx.versions(id, name)
	if err != nil {
		return nil, false, err
	}
	var v value.Value
	for _, ver := range vs {
		if ver.At.Go().After(a.at.Go()) {
			break
		}
		v = ver.Value
	}
	if v == nil {
		return value.Null, false, nil
	}
	return v, true, nil
}

// Entities calls fn with every entity of the kind ("" for all) that
// existed at the time, until fn returns false. For a kind with history
// that includes entities deleted since.
func (a *AsOf) Entities(kind string, fn func(value.ID) bool) error {
	var k *model.Kind
	if kind != "" {
		var ok bool
		if k, ok = a.tx.Catalog().Kind(kind); !ok {
			return a.tx.unknownKind(kind)
		}
	}
	var visitErr error
	visit := func(id value.ID) bool {
		_, ok, err := a.Entity(id)
		if err != nil {
			visitErr = err
			return false
		}
		return !ok || fn(id)
	}
	if k != nil && k.History.On {
		err := a.tx.scan(keys.HistKinds(k.ID), false, func(key, _ []byte) error {
			if !visit(value.ID(key[len(key)-16:])) {
				return errStopScan
			}
			return nil
		})
		if err != nil && err != errStopScan {
			return err
		}
		return visitErr
	}
	if err := a.tx.Entities(kind, visit); err != nil {
		return err
	}
	return visitErr
}

// dropKindHistory deletes what a dropped kind's deleted entities left.
func (tx *Tx) dropKindHistory(kind value.ID) error {
	var ids []value.ID
	if err := tx.scan(keys.HistKinds(kind), false, func(k, _ []byte) error {
		ids = append(ids, value.ID(k[len(k)-16:]))
		return nil
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.markBlobsDirty(id); err != nil {
			return err
		}
		p := keys.HistoryEntity(id)
		tx.t.DeleteRange(p, storage.PrefixEnd(p))
		tx.t.Delete(keys.Life(id))
		tx.t.Delete(keys.HistKind(kind, id))
	}
	return nil
}
