package engine

import (
	"context"
	"time"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Secondary indexes (spec/rhizome.md §5.4). Every save writes the entries
// of the indexes on the entity's kind, in the same transaction as the
// data, so an index never disagrees with it. Entities that existed before
// the index was defined are indexed by a background builder, which walks
// the kind in ID order a batch at a time and records how far it got
// (keys.IndexBuild). An index is ready, and the planner may use it, once
// that record is gone. Small kinds are indexed at once instead.

// syncBuildLimit is the most entities a kind may have for a new index to
// be built in the defining transaction.
const syncBuildLimit = 1000

// buildBatch is the most entities the builder indexes per transaction.
// A batch conflicts when a writer changes any of its entities, so after a
// conflict the builder halves its batch (to minBuildBatch), and doubles
// it again after each success: busy kinds still make progress.
const (
	buildBatch    = 256
	minBuildBatch = 8
)

// indexEntryKey returns an entity's entry in a non-unique value index, or
// false when the entity has no entry: a key is absent or null, a path
// finds nothing, or a value can't be a key.
func (tx *Tx) indexEntryKey(ix *model.Index, id value.ID, fields map[string]*model.Field) ([]byte, bool) {
	if ix.Unique || ix.Text {
		return nil, false
	}
	var vals []byte
	for _, k := range ix.Keys {
		f, ok := fields[k.Field]
		if !ok || f.Value == nil || value.IsNull(f.Value) {
			return nil, false
		}
		v := f.Value
		if len(k.Path) > 0 {
			d, ok := v.(value.Doc)
			if !ok {
				return nil, false
			}
			format, ok := tx.Catalog().Format(d.Format())
			if !ok {
				return nil, false
			}
			if v, ok = lang.IndexValue(format, d, k.Path); !ok {
				return nil, false
			}
		}
		var err error
		if vals, err = value.AppendKey(vals, v); err != nil {
			return nil, false
		}
	}
	return keys.IndexEntry(ix.ID, vals, id), true
}

// buildIndex indexes a new non-unique index's existing entities: at once
// for a small kind, otherwise in the background after commit.
func (tx *Tx) buildIndex(ix *model.Index) error {
	var ids []value.ID
	stop := errorf(CodeInternal, "stop")
	err := tx.scan(keys.Members(ix.Kind), false, func(k, _ []byte) error {
		if len(ids) > syncBuildLimit {
			return stop
		}
		ids = append(ids, value.ID(k[len(k)-16:]))
		return nil
	})
	if err != nil && err != stop {
		return err
	}
	if len(ids) > syncBuildLimit {
		tx.t.Set(keys.IndexBuild(ix.ID), keys.EncodeBuildState(keys.BuildState{}))
		tx.startBuild = true
		return nil
	}
	for _, id := range ids {
		if err := tx.indexEntity(ix, id); err != nil {
			return err
		}
	}
	return nil
}

// indexEntity writes one entity's entry in an index.
func (tx *Tx) indexEntity(ix *model.Index, id value.ID) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	if k, ok := tx.indexEntryKey(ix, id, st.fields); ok {
		tx.t.Set(k, nil)
	}
	return nil
}

// IndexState is how far an index is built.
type IndexState struct {
	Ready bool  // complete and usable
	Done  int64 // while building: entities indexed so far
}

// IndexState reports whether an index is ready, as this transaction sees
// it.
func (tx *Tx) IndexState(ix *model.Index) (IndexState, error) {
	if ix.Unique {
		return IndexState{Ready: true}, nil
	}
	b, ok, err := tx.get(keys.IndexBuild(ix.ID))
	if err != nil || !ok {
		return IndexState{Ready: err == nil}, err
	}
	s, err := keys.DecodeBuildState(b)
	if err != nil {
		return IndexState{}, wrap(CodeInternal, err)
	}
	return IndexState{Done: s.Done}, nil
}

// builder runs a database's background index builds.
type builder struct {
	kick chan struct{}
	quit chan struct{}
	done chan struct{}
	err  error // the last error that stopped a build, other than a conflict
}

func (db *DB) startBuilder() {
	db.build = &builder{kick: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go db.buildLoop()
	db.kickBuilder() // resume builds a restart interrupted
}

func (db *DB) kickBuilder() {
	select {
	case db.build.kick <- struct{}{}:
	default:
	}
}

func (db *DB) stopBuilder() {
	close(db.build.quit)
	<-db.build.done
}

func (db *DB) buildLoop() {
	defer close(db.build.done)
	for {
		select {
		case <-db.build.quit:
			return
		case <-db.build.kick:
		}
		batch := buildBatch
		for {
			select {
			case <-db.build.quit:
				return
			default:
			}
			more, err := db.buildStep(batch)
			if IsConflict(err) {
				batch = max(batch/2, minBuildBatch) // a writer changed an entity in the batch: redo it, smaller
				continue
			}
			batch = min(batch*2, buildBatch)
			if err != nil {
				db.buildMu.Lock()
				db.build.err = err
				db.buildMu.Unlock()
				break
			}
			if !more {
				break
			}
		}
	}
}

// buildStep indexes one batch of one index still being built, and
// reports whether there may be more to do.
func (db *DB) buildStep(batch int) (more bool, err error) {
	tx := db.Begin(TxOptions{})
	defer tx.Rollback()
	var ixID value.ID
	found := false
	stop := errorf(CodeInternal, "stop")
	err = tx.scan(keys.IndexBuilds(), false, func(k, _ []byte) error {
		ixID, found = value.ID(k[len(k)-16:]), true
		return stop
	})
	if err != nil && err != stop {
		return false, err
	}
	if !found {
		return false, nil
	}
	if err := tx.writable(); err != nil {
		return false, err
	}
	ix, ok := tx.Catalog().IndexByID(ixID)
	if !ok { // dropped
		tx.t.Delete(keys.IndexBuild(ixID))
		return true, tx.Commit()
	}
	b, _, err := tx.get(keys.IndexBuild(ixID))
	if err != nil {
		return false, err
	}
	state, err := keys.DecodeBuildState(b)
	if err != nil {
		return false, wrap(CodeInternal, err)
	}
	opts := keys.Range(keys.Members(ix.Kind))
	if !state.After.IsZero() {
		opts.Lower = append(keys.Member(ix.Kind, state.After), 0)
	}
	it := tx.t.Iter(opts)
	var ids []value.ID
	for len(ids) < batch && it.Next() {
		ids = append(ids, value.ID(it.Key()[len(it.Key())-16:]))
	}
	err = it.Err()
	it.Close()
	if err != nil {
		return false, wrap(CodeInternal, err)
	}
	for _, id := range ids {
		// Every save writes the entity's header: if a writer changes the
		// entity before this commits, the batch conflicts and is redone.
		tx.t.GuardKey(keys.Entity(id))
		if err := tx.indexEntity(ix, id); err != nil {
			return false, err
		}
	}
	if len(ids) < batch {
		tx.t.Delete(keys.IndexBuild(ixID))
	} else {
		state.After, state.Done = ids[len(ids)-1], state.Done+int64(len(ids))
		tx.t.Set(keys.IndexBuild(ixID), keys.EncodeBuildState(state))
	}
	return true, tx.Commit()
}

// BuildError returns the error that stopped a background index build, if
// any. Builds start again with the next index definition or restart.
func (db *DB) BuildError() error {
	db.buildMu.Lock()
	defer db.buildMu.Unlock()
	return db.build.err
}

// WaitIndexes waits until every index is built.
func (db *DB) WaitIndexes(ctx context.Context) error {
	for {
		pending := false
		err := db.View(func(tx *Tx) error {
			return tx.scan(keys.IndexBuilds(), false, func(_, _ []byte) error {
				pending = true
				return errorf(CodeInternal, "stop")
			})
		})
		if !pending {
			return nil
		}
		if err != nil && !pending {
			return err
		}
		if err := db.BuildError(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ScanIndex calls fn with each entry of a non-unique index whose key
// encoding is in [lower, upper) (nil: unbounded), in key order, until fn
// returns false. vals is the entry's key encoding.
func (tx *Tx) ScanIndex(ix *model.Index, lower, upper []byte, fn func(id value.ID, vals []byte) bool) error {
	prefix := keys.IndexPrefix(ix.ID)
	opts := keys.Range(prefix)
	if lower != nil {
		opts.Lower = append(append([]byte(nil), prefix...), lower...)
	}
	if upper != nil {
		opts.Upper = append(append([]byte(nil), prefix...), upper...)
	}
	it := tx.t.Iter(opts)
	defer it.Close()
	for it.Next() {
		vals, id, err := keys.ParseIndexEntry(it.Key())
		if err != nil {
			return wrap(CodeInternal, err)
		}
		if !fn(id, vals) {
			return nil
		}
	}
	return wrap(CodeInternal, it.Err())
}

// VerifyIndexes checks every ready non-unique index against the data:
// each entity has exactly the entry its values call for, and no index
// that no longer exists has entries left. It reads the whole database.
func (db *DB) VerifyIndexes() error {
	return db.View(func(tx *Tx) error {
		cat := tx.Catalog()
		have := map[value.ID]map[string]bool{}
		err := tx.scan(keys.Prefix(keys.PrefixIndex), false, func(k, _ []byte) error {
			ixID := value.ID(k[1:17])
			if have[ixID] == nil {
				have[ixID] = map[string]bool{}
			}
			have[ixID][string(k)] = true
			return nil
		})
		if err != nil {
			return err
		}
		for ixID := range have {
			if _, ok := cat.IndexByID(ixID); !ok {
				return errorf(CodeInternal, "entries left for dropped index %s", ixID)
			}
		}
		for _, ix := range cat.Indexes() {
			if ix.Unique || ix.Text {
				continue
			}
			st, err := tx.IndexState(ix)
			if err != nil {
				return err
			}
			if !st.Ready {
				continue
			}
			want := map[string]bool{}
			err = tx.scan(keys.Members(ix.Kind), false, func(k, _ []byte) error {
				id := value.ID(k[len(k)-16:])
				e, err := tx.loadEntity(id)
				if err != nil {
					return err
				}
				if key, ok := tx.indexEntryKey(ix, id, e.fields); ok {
					want[string(key)] = true
				}
				return nil
			})
			if err != nil {
				return err
			}
			for k := range want {
				if !have[ix.ID][k] {
					_, id, _ := keys.ParseIndexEntry([]byte(k))
					return errorf(CodeInternal, "index %s: entity %s has no entry", ix.Name, id)
				}
			}
			for k := range have[ix.ID] {
				if !want[k] {
					_, id, _ := keys.ParseIndexEntry([]byte(k))
					return errorf(CodeInternal, "index %s: stale entry for entity %s", ix.Name, id)
				}
			}
		}
		return nil
	})
}
