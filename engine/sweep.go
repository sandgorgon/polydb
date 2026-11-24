package engine

import (
	"context"
	"time"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/value"
)

// The history sweep (spec/rhizome.md §10) removes stored history that
// nothing can read any more. Writing a field prunes that field's old
// versions, but a field that is no longer written, a deleted entity, or
// a field whose history was turned off keeps what it had. The sweep
// finds those:
//
//   - versions a field replaced longer ago than it keeps history (a
//     field's current version stays, however old);
//   - every version of a field whose history is off;
//   - entities deleted longer ago than their kind keeps history for any
//     of its fields: their versions, life record and kind entry.
//
// It never changes what a read within the retention finds. It runs in
// the background (Options.HistorySweep), one transaction per batch of
// entities. A batch that conflicts with a writer is retried at half the
// size, like index builds, so busy kinds still get swept; one that keeps
// conflicting at the smallest size is left for the next sweep.

// sweepBatch is the most entities' history one sweep transaction
// handles, and minSweepBatch the least it shrinks to after conflicts.
const (
	sweepBatch    = 256
	minSweepBatch = 8
)

// SweepResult counts what a sweep removed.
type SweepResult struct {
	Versions int // field versions
	Entities int // deleted entities forgotten entirely
}

// SweepHistory removes history past its retention (see above) and
// reports what it removed.
func (db *DB) SweepHistory(ctx context.Context) (SweepResult, error) {
	var total SweepResult
	for _, prefix := range []byte{keys.PrefixHistory, keys.PrefixLife} {
		var after []byte
		batch := sweepBatch
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			var r SweepResult
			var next []byte
			err := db.Update(func(tx *Tx) error {
				var err error
				next, err = tx.sweepBatch(prefix, after, batch, &r)
				return err
			})
			switch {
			case IsConflict(err) && batch > minSweepBatch:
				batch = max(batch/2, minSweepBatch) // redo it, smaller
				continue
			case IsConflict(err):
				// Writers keep changing these few entities: leave them
				// for the next sweep.
			case err != nil:
				return total, err
			default:
				total.Versions += r.Versions
				total.Entities += r.Entities
				batch = min(batch*2, sweepBatch)
			}
			if next == nil {
				break
			}
			after = next
		}
	}
	return total, nil
}

// sweepBatch sweeps up to batch entities whose keys under prefix
// (history or life records) come after the entity after. It returns the
// last entity it handled, or nil when there are no more.
func (tx *Tx) sweepBatch(prefix byte, after []byte, batch int, r *SweepResult) ([]byte, error) {
	opts := keys.Range([]byte{prefix})
	if after != nil {
		opts.Lower = storage.PrefixEnd(append([]byte{prefix}, after...))
	}
	var ids []value.ID
	it := tx.t.Iter(opts)
	for it.Next() && len(ids) < batch {
		k := it.Key()
		if len(k) < 17 {
			continue
		}
		var id value.ID
		copy(id[:], k[1:17])
		if len(ids) == 0 || ids[len(ids)-1] != id {
			ids = append(ids, id)
		}
		if prefix == keys.PrefixHistory {
			// Skip the rest of this entity's versions.
			it.Close()
			opts.Lower = storage.PrefixEnd(keys.HistoryEntity(id))
			it = tx.t.Iter(opts)
		}
	}
	err := it.Err()
	it.Close()
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	// What history a field keeps comes from the catalog, which every
	// data change also guards.
	tx.t.GuardKey(keys.CatalogVersion)
	now := tx.Now()
	for _, id := range ids {
		// Any save of the entity writes its header, and a delete its
		// life record: a writer changing it conflicts with this batch.
		tx.t.GuardKey(keys.Entity(id))
		tx.t.GuardKey(keys.Life(id))
		var err error
		if prefix == keys.PrefixHistory {
			err = tx.sweepVersions(id, now, r)
		} else {
			err = tx.sweepDeleted(id, now, r)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	last := ids[len(ids)-1]
	return last[:], nil
}

// sweepVersions removes an entity's versions that are past retention.
func (tx *Tx) sweepVersions(id value.ID, now time.Time, r *SweepResult) error {
	var kind *model.Kind
	var cur map[string]*model.Field
	if n, ok, err := tx.Exists(id); err != nil {
		return err
	} else if ok && n == model.NodeEntity {
		st, err := tx.loadEntity(id)
		if err != nil {
			return err
		}
		kind, cur = st.kind, st.fields
	} else {
		life, err := tx.life(id)
		if err != nil {
			return err
		}
		if life != nil {
			kind, _ = tx.Catalog().KindByID(life.Kind)
		}
	}
	type version struct {
		key []byte
		at  time.Time
	}
	byField := map[string][]version{}
	var names []string
	err := tx.scan(keys.HistoryEntity(id), false, func(k, _ []byte) error {
		_, name, err := keys.ParseHistory(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		t, err := keys.HistoryTime(k)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if _, ok := byField[name]; !ok {
			names = append(names, name)
		}
		byField[name] = append(byField[name], version{append([]byte(nil), k...), t.Go()})
		return nil
	})
	if err != nil {
		return err
	}
	for _, name := range names {
		vs := byField[name]
		h, err := tx.fieldHistory(id, kind, cur[name], name)
		if err != nil {
			return err
		}
		drop := 0
		switch {
		case !h.On:
			drop = len(vs) // nothing can read them
		case h.Keep > 0:
			cutoff := now.Add(-h.Keep)
			for drop+1 < len(vs) && !vs[drop+1].at.After(cutoff) {
				drop++ // replaced at or before the cutoff
			}
		}
		for _, v := range vs[:drop] {
			tx.t.Delete(v.key)
		}
		r.Versions += drop
	}
	return nil
}

// sweepDeleted forgets an entity deleted longer ago than its kind keeps
// history for any field.
func (tx *Tx) sweepDeleted(id value.ID, now time.Time, r *SweepResult) error {
	life, err := tx.life(id)
	if err != nil || life == nil || life.Deleted == nil {
		return err
	}
	kind, ok := tx.Catalog().KindByID(life.Kind)
	if ok && kind.History.On {
		keep, forever := retention(kind)
		if forever || life.Deleted.Go().After(now.Add(-keep)) {
			return nil
		}
	}
	// Past retention, or the kind no longer keeps history (or is gone):
	// no read can find the entity.
	n := 0
	err = tx.scan(keys.HistoryEntity(id), false, func(k, _ []byte) error {
		n++
		tx.t.Delete(append([]byte(nil), k...))
		return nil
	})
	if err != nil {
		return err
	}
	tx.t.Delete(keys.Life(id))
	tx.t.Delete(keys.HistKind(life.Kind, id))
	r.Versions += n
	r.Entities++
	return nil
}

// retention is the longest any field of a kind with history keeps it.
func retention(k *model.Kind) (keep time.Duration, forever bool) {
	if k.History.Keep <= 0 {
		return 0, true // fields without their own keep keep everything
	}
	keep = k.History.Keep
	for _, fd := range k.Fields {
		if fd.History.On && fd.History.Keep > keep {
			keep = fd.History.Keep
		}
	}
	return keep, false
}

// startSweeper runs the history sweep every interval until the database
// closes.
func (db *DB) startSweeper(interval time.Duration) {
	db.sweepQuit, db.sweepDone = make(chan struct{}), make(chan struct{})
	every(interval, db.sweepQuit, db.sweepDone, func(ctx context.Context) {
		db.SweepHistory(ctx) // errors are retried on the next round
	})
}

func (db *DB) stopSweeper() {
	close(db.sweepQuit)
	<-db.sweepDone
}

// startStats refreshes statistics every interval until the database
// closes.
func (db *DB) startStats(interval time.Duration) {
	db.statsQuit, db.statsDone = make(chan struct{}), make(chan struct{})
	every(interval, db.statsQuit, db.statsDone, db.refreshStats)
}

func (db *DB) stopStats() {
	close(db.statsQuit)
	<-db.statsDone
}

// CountHistory counts the stored field versions and the life records of
// entities of kinds with history (deleted ones included).
func (tx *Tx) CountHistory() (versions, entities int, err error) {
	if versions, err = tx.count(keys.Prefix(keys.PrefixHistory)); err != nil {
		return 0, 0, err
	}
	entities, err = tx.count(keys.Prefix(keys.PrefixLife))
	return versions, entities, err
}
