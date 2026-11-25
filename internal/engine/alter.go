package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/internal/keys"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

// Altering kinds (spec/rhizome.md §5.1). A change that leaves every
// entity's stored data valid applies at once. Otherwise every entity is
// checked, and rewritten if the change needs it: in the altering
// transaction for a kind of up to syncBuildLimit entities, and in the
// background for a larger one, in batches:
//
//  1. Writes to the kind stop: every write guards the kind's alter key,
//     which the alter sets, so one that started before conflicts.
//  2. Check: each entity is changed in memory and checked against the
//     new definition; nothing is written but the keys of fields that
//     become unique, whose duplicates fail the check. Reads see the old
//     definition. If any entity doesn't fit, the alter fails, those keys
//     are removed and writes resume.
//  3. Switch: the new definition becomes current.
//  4. Rewrite: each entity is written in its new form. Until it is,
//     reads see it through the new definition: fields the kind dropped
//     are hidden, values are converted to the new types, fields the
//     kind now declares count as declared. The kind's indexes aren't
//     used meanwhile.
//  5. Writes resume.
//
// The job's state, kept under the kind's alter key, survives a restart,
// and a job resumes where it was. show alters reports it.

// alterNeed is what a kind change needs from the stored data.
type alterNeed int

const (
	needNothing alterNeed = iota // stored data stays valid
	needCheck                    // every entity must be checked
	needRewrite                  // every entity must be checked and rewritten
)

// alterNeeds works out what changing old to nk needs, and which fields
// stop being unique (their keys go, at once).
func alterNeeds(old, nk *model.Kind) (need alterNeed, unUnique []string) {
	raise := func(n alterNeed) { need = max(need, n) }
	if !old.Closed && nk.Closed {
		raise(needCheck)
	}
	for _, of := range old.Fields {
		nf, kept := nk.Field(of.Name)
		if !kept {
			raise(needRewrite) // its records go
			if of.Unique {
				unUnique = append(unUnique, of.Name)
			}
			continue
		}
		if of.Unique && !nf.Unique {
			unUnique = append(unUnique, of.Name)
		}
	}
	for i := range nk.Fields {
		nf := &nk.Fields[i]
		of, had := old.Field(nf.Name)
		if !had {
			// Entities may hold their own field of that name, which now
			// becomes declared; a default or a required field needs a
			// value in every entity.
			if !old.Closed || nf.Default != "" || !nf.Type.Nullable() {
				raise(needRewrite)
			}
			continue
		}
		switch {
		case of.Type.Equal(nf.Type):
		case value.Required(of.Type).Equal(value.Required(nf.Type)):
			if !nf.Type.Nullable() {
				raise(needCheck) // optional to required: no nulls allowed
			}
		default:
			raise(needRewrite) // values convert
		}
		if nf.Unique && !of.Unique {
			raise(needCheck)
		}
		if nf.Check != "" && nf.Check != of.Check {
			raise(needCheck)
		}
		if nf.OnDelete != of.OnDelete {
			raise(needRewrite) // restrict refs keep back-references
		}
	}
	return need, unUnique
}

// --- the job ---

const (
	alterChecking  = "checking"
	alterRewriting = "rewriting"
	alterDone      = "done"
	alterFailed    = "failed"

	alterBatch     = 1024
	alterMaxErrors = 10
)

// alterKey holds a kind's alter job.
func alterKey(kind value.ID) []byte { return append(keys.Meta("alter/"), kind[:]...) }

func alterKeys() []byte { return keys.Meta("alter/") }

// AlterStatus is how a background alter is doing (show alters).
type AlterStatus struct {
	Kind              string
	Phase             string // checking, rewriting, done or failed
	Rewrite           bool   // whether the change rewrites entities
	Done, Total       int64  // entities through the current phase
	Failed            int64  // entities that didn't fit (failed)
	Errors            []string
	Started, Finished time.Time
	Err               string // an error that held the job up, retried
}

// alterJob is the stored state of a background alter.
type alterJob struct {
	AlterStatus
	KindID    value.ID
	Old, New  kindDef
	Need      alterNeed
	After     value.ID // the last entity done in this phase
	NewUnique []string // fields becoming unique: their keys, written while checking
	UnUnique  []string // fields no longer unique: their keys go at the switch
}

func (j *alterJob) active() bool { return j.Phase == alterChecking || j.Phase == alterRewriting }

// kindDef is a kind definition as JSON, with types as Rhizome text.
type kindDef struct {
	ID      value.ID
	Name    string
	Doc     string
	Closed  bool
	History model.History
	Fields  []storedField
}

type storedField struct {
	Name, Type, Doc string
	Unique          bool
	Default, Check  string
	OnDelete        model.RefAction
	History         model.History
}

func toDef(k *model.Kind) kindDef {
	d := kindDef{ID: k.ID, Name: k.Name, Doc: k.Doc, Closed: k.Closed, History: k.History}
	for _, f := range k.Fields {
		d.Fields = append(d.Fields, storedField{Name: f.Name, Type: f.Type.String(), Doc: f.Doc, Unique: f.Unique,
			Default: f.Default, Check: f.Check, OnDelete: f.OnDelete, History: f.History})
	}
	return d
}

func (d kindDef) kind() (*model.Kind, error) {
	k := &model.Kind{ID: d.ID, Name: d.Name, Doc: d.Doc, Closed: d.Closed, History: d.History}
	for _, f := range d.Fields {
		t, err := value.ParseType(f.Type)
		if err != nil {
			return nil, err
		}
		k.Fields = append(k.Fields, model.FieldDef{Name: f.Name, Type: t, Doc: f.Doc, Unique: f.Unique,
			Default: f.Default, Check: f.Check, OnDelete: f.OnDelete, History: f.History})
	}
	return k, nil
}

func (tx *Tx) loadAlter(kind value.ID) (*alterJob, bool, error) {
	b, ok, err := tx.get(alterKey(kind))
	if err != nil || !ok {
		return nil, false, err
	}
	var j alterJob
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, false, wrap(CodeInternal, err)
	}
	return &j, true, nil
}

func (tx *Tx) storeAlter(j *alterJob) error {
	b, err := json.Marshal(j)
	if err != nil {
		return wrap(CodeInternal, err)
	}
	tx.t.Set(alterKey(j.KindID), b)
	return nil
}

// checkNotAltering refuses a write to an entity of a kind a background
// alter is working on. Every write guards the kind's alter key, so a
// write that began before an alter conflicts with it.
func (tx *Tx) checkNotAltering(k *model.Kind) error {
	if k == nil || tx.alterJob {
		return nil
	}
	tx.t.GuardKey(alterKey(k.ID))
	if tx.db.alters.Load() == 0 {
		return nil
	}
	j, ok, err := tx.loadAlter(k.ID)
	if err != nil || !ok || !j.active() {
		return err
	}
	return errorf(CodeConstraint, "kind %s is being altered (%s, %d of %d; show alters): writes to it resume when that finishes",
		k.Name, j.Phase, j.Done, j.Total)
}

// startAlter records a background alter of old to nk.
func (tx *Tx) startAlter(old, nk *model.Kind, need alterNeed, total int64, unUnique []string) error {
	if j, ok, err := tx.loadAlter(old.ID); err != nil {
		return err
	} else if ok && j.active() {
		return errorf(CodeConstraint, "kind %s is already being altered", old.Name)
	}
	j := &alterJob{KindID: old.ID, Old: toDef(old), New: toDef(nk), Need: need, UnUnique: unUnique}
	j.Kind, j.Phase, j.Total, j.Rewrite, j.Started = old.Name, alterChecking, total, need == needRewrite, tx.timeNow().Go()
	for _, f := range nk.Fields {
		if of, had := old.Field(f.Name); f.Unique && (!had || !of.Unique) {
			j.NewUnique = append(j.NewUnique, f.Name)
		}
	}
	if err := tx.storeAlter(j); err != nil {
		return err
	}
	// Counted before the commit, so no write misses the check; the
	// runner corrects the count.
	tx.db.alters.Add(1)
	tx.startAlterJob = true
	return nil
}

// Alters returns the database's background alters, current and past.
func (tx *Tx) Alters() ([]AlterStatus, error) {
	var out []AlterStatus
	err := tx.scan(alterKeys(), false, func(_, v []byte) error {
		var j alterJob
		if err := json.Unmarshal(v, &j); err != nil {
			return wrap(CodeInternal, err)
		}
		out = append(out, j.AlterStatus)
		return nil
	})
	slices.SortFunc(out, func(a, b AlterStatus) int { return strings.Compare(a.Kind, b.Kind) })
	return out, err
}

// AlterStarted reports whether this transaction started a background
// alter, which runs once it commits.
func (tx *Tx) AlterStarted() bool { return tx.startAlterJob }

// rewriting returns the definition of a kind whose entities a
// background alter is rewriting, which reads go through.
func (db *DB) rewriting(kind value.ID) (*model.Kind, bool) {
	if db.alters.Load() == 0 {
		return nil, false
	}
	v, ok := db.rewritingKinds.Load(kind)
	if !ok {
		return nil, false
	}
	return v.(*model.Kind), true
}

// normalize shows a loaded entity of a kind being rewritten through its
// new definition k.
func normalize(st *entState, k *model.Kind) {
	for name, f := range st.fields {
		fd, declared := k.Field(name)
		switch {
		case f.Declared && !declared:
			delete(st.fields, name) // the kind dropped it
		case f.Value == nil || value.IsNull(f.Value):
			if !f.Declared && declared {
				cp := *f
				cp.Declared, cp.Def = true, nil
				st.fields[name] = &cp
			}
		case declared:
			c, err := value.Conform(f.Value, fd.Type)
			if err != nil {
				continue // the check let it through; show it as it is
			}
			if c != f.Value || !f.Declared {
				cp := *f
				cp.Value, cp.Declared = c, true
				if !f.Declared {
					cp.Def = nil
				}
				st.fields[name] = &cp
			}
		}
	}
}

// loadAlters notes the alters a restart interrupted, before anything
// reads or writes, so their kinds stay closed to writes and read
// through their new definitions.
func (db *DB) loadAlters() error {
	return db.View(func(tx *Tx) error {
		var active int64
		err := tx.scan(alterKeys(), false, func(_, v []byte) error {
			var j alterJob
			if err := json.Unmarshal(v, &j); err != nil {
				return wrap(CodeInternal, err)
			}
			if j.active() {
				active++
			}
			if j.Phase == alterRewriting {
				if k, ok := tx.Catalog().KindByID(j.KindID); ok {
					db.rewritingKinds.Store(j.KindID, k)
				}
			}
			return nil
		})
		db.alters.Store(active)
		return err
	})
}

// --- the runner ---

// alterer runs a database's background alters.
type alterer struct {
	kick, quit, done chan struct{}
}

func (db *DB) startAlterer() {
	db.alter = &alterer{kick: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go db.alterLoop()
	db.kickAlterer() // resume alters a restart interrupted
}

func (db *DB) kickAlterer() {
	select {
	case db.alter.kick <- struct{}{}:
	default:
	}
}

func (db *DB) stopAlterer() {
	close(db.alter.quit)
	<-db.alter.done
}

// alterHook, if set, runs before each batch with the job's phase and
// progress; an error holds the job up (tests).
var alterHook func(phase string, done int64) error

// alterRetry is how long a job held up by an error waits to try again.
var alterRetry = 5 * time.Second

func (db *DB) alterLoop() {
	defer close(db.alter.done)
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	for {
		select {
		case <-db.alter.quit:
			return
		case <-db.alter.kick:
		case <-retry.C:
		}
		for {
			select {
			case <-db.alter.quit:
				return
			default:
			}
			more, err := db.alterStep()
			if IsConflict(err) {
				continue
			}
			if err != nil {
				db.noteAlterError(err)
				retry.Reset(alterRetry)
				break
			}
			if !more {
				break
			}
		}
	}
}

// noteAlterError records an error that held a job up.
func (db *DB) noteAlterError(cause error) {
	db.Update(func(tx *Tx) error {
		tx.alterJob = true
		var target *alterJob
		err := tx.scan(alterKeys(), false, func(_, v []byte) error {
			var j alterJob
			if json.Unmarshal(v, &j) == nil && j.active() && target == nil {
				target = &j
			}
			return nil
		})
		if err != nil || target == nil {
			return err
		}
		target.Err = cause.Error()
		return tx.storeAlter(target)
	})
}

// alterStep does one batch of the first active alter, and reports
// whether there may be more to do.
func (db *DB) alterStep() (more bool, err error) {
	tx := db.Begin(TxOptions{})
	defer tx.Rollback()
	tx.alterJob = true
	var job *alterJob
	active := int64(0)
	err = tx.scan(alterKeys(), false, func(_, v []byte) error {
		var j alterJob
		if err := json.Unmarshal(v, &j); err != nil {
			return wrap(CodeInternal, err)
		}
		if j.active() {
			active++
			if job == nil {
				job = &j
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	db.alters.Store(active)
	if job == nil {
		return false, nil
	}
	if alterHook != nil {
		if err := alterHook(job.Phase, job.Done); err != nil {
			return false, err
		}
	}
	old, err := job.Old.kind()
	if err != nil {
		return false, wrap(CodeInternal, err)
	}
	nk, err := job.New.kind()
	if err != nil {
		return false, wrap(CodeInternal, err)
	}
	if job.Phase == alterRewriting {
		cur, ok := tx.Catalog().KindByID(job.KindID)
		if !ok {
			return false, errorf(CodeInternal, "kind %s went away while being altered", job.Kind)
		}
		nk = cur
		db.rewritingKinds.Store(job.KindID, cur)
	}

	// The next batch of entities.
	opts := keys.Range(keys.Members(job.KindID))
	if !job.After.IsZero() {
		opts.Lower = append(keys.Member(job.KindID, job.After), 0)
	}
	it := tx.t.Iter(opts)
	var ids []value.ID
	for len(ids) < alterBatch && it.Next() {
		ids = append(ids, value.ID(it.Key()[len(it.Key())-16:]))
	}
	err = it.Err()
	it.Close()
	if err != nil {
		return false, wrap(CodeInternal, err)
	}

	switch job.Phase {
	case alterChecking:
		for _, id := range ids {
			if err := tx.checkOne(job, id, old, nk); err != nil {
				if CodeOf(err) == CodeInternal || CodeOf(err) == CodeConflict {
					return false, err
				}
				job.Failed++
				if len(job.Errors) < alterMaxErrors {
					job.Errors = append(job.Errors, err.Error())
				}
			}
		}
	case alterRewriting:
		for _, id := range ids {
			if err := tx.migrate(id, old, nk); err != nil {
				return false, err
			}
		}
	}
	job.Done += int64(len(ids))
	job.Err = ""
	if len(ids) > 0 {
		job.After = ids[len(ids)-1]
	}
	if len(ids) == alterBatch {
		if err := tx.storeAlter(job); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}

	// The phase is through.
	now := tx.timeNow().Go()
	switch {
	case job.Phase == alterChecking && job.Failed > 0:
		for _, name := range job.NewUnique {
			p := keys.UniquePrefix(job.KindID, name)
			tx.t.DeleteRange(p, prefixEnd(p))
		}
		job.Phase, job.Finished = alterFailed, now
	case job.Phase == alterChecking:
		e, err := tx.editing()
		if err != nil {
			return false, err
		}
		cur, _ := tx.Catalog().KindByID(job.KindID)
		if err := e.ReplaceKind(*nk); err != nil {
			return false, wrap(CodeConstraint, err)
		}
		switched, _ := tx.Catalog().KindByID(job.KindID)
		if err := tx.historyBaseline(cur, switched); err != nil {
			return false, err
		}
		for _, f := range job.UnUnique {
			p := keys.UniquePrefix(job.KindID, f)
			tx.t.DeleteRange(p, prefixEnd(p))
		}
		if job.Need == needRewrite {
			job.Phase, job.After, job.Done = alterRewriting, value.ID{}, 0
			// Reads convert from now; set before the commit, which is
			// harmless while the old definition is current.
			db.rewritingKinds.Store(job.KindID, switched)
		} else {
			job.Phase, job.Finished = alterDone, now
		}
	default:
		job.Phase, job.Finished = alterDone, now
	}
	if err := tx.storeAlter(job); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if !job.active() {
		db.rewritingKinds.Delete(job.KindID)
	}
	return true, nil
}

// checkOne checks that one entity fits the new definition, and writes
// the keys of its fields that become unique.
func (tx *Tx) checkOne(job *alterJob, id value.ID, old, nk *model.Kind) error {
	st, _, err := tx.transform(id, old, nk)
	if err != nil {
		return err
	}
	if err := checkRequired(st); err != nil {
		return fmt.Errorf("entity %s: %w", id, err)
	}
	if err := tx.checkEntity(st); err != nil {
		return fmt.Errorf("entity %s: %w", id, err)
	}
	for _, name := range job.NewUnique {
		f, ok := st.fields[name]
		if !ok || f.Value == nil || value.IsNull(f.Value) {
			continue
		}
		k, ok, err := valueKey(nil, f.Value)
		if err != nil || !ok {
			return err
		}
		uk := keys.Unique(job.KindID, name, k)
		if cur, found, err := tx.get(uk); err != nil {
			return err
		} else if found && value.ID(cur) != id {
			return errorf(CodeConstraint, "entity %s: %s has the same value as entity %s", id, name, value.ID(cur))
		}
		tx.t.Set(uk, id[:])
	}
	return nil
}

func prefixEnd(p []byte) []byte { return keys.Range(p).Upper }

// WaitAlters waits until no background alter is running.
func (db *DB) WaitAlters(ctx context.Context) error {
	for {
		var running bool
		err := db.View(func(tx *Tx) error {
			alters, err := tx.Alters()
			for _, a := range alters {
				if a.Phase == alterChecking || a.Phase == alterRewriting {
					running = true
				}
			}
			return err
		})
		if err != nil || !running {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
