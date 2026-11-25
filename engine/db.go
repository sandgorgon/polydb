package engine

import (
	"sync"
	"sync/atomic"

	"github.com/sandgorgon/polydb/blob"
	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/storage/lsm"
	"github.com/sandgorgon/polydb/txn"
)

// DB is one database. It is safe for concurrent use.
type DB struct {
	name string
	dir  string // its directory's number, with an engine Dir
	eng  *Engine
	mgr  *txn.Manager
	cat  atomic.Pointer[catalog.Catalog]

	wmu     sync.Mutex
	waiters map[string]*waitList // PopWait waiters by field key

	exprs sync.Map // exprKey → *lang.Compiled, for the current catalog

	build   *builder // background index builds
	buildMu sync.Mutex

	sweepQuit, sweepDone chan struct{} // the background history sweep
	statsQuit, statsDone chan struct{} // the background statistics refresh
	statChanges          sync.Map      // kind ID → *atomic.Int64: changes since its last sample

	alter          *alterer     // background alters (alter.go)
	alters         atomic.Int64 // alters running, or about to
	rewritingKinds sync.Map     // kind ID → *model.Kind: kinds whose entities an alter is rewriting

	blobs   *blob.Store // large values (blob.go)
	touchMu sync.Mutex
	touched map[uint32]uint64 // blob segment → store seq of its last append
}

// Name returns the database's name.
func (db *DB) Name() string { return db.name }

// Catalog returns the current committed catalog.
func (db *DB) Catalog() *catalog.Catalog { return db.cat.Load() }

// TxOptions configure a transaction.
type TxOptions struct {
	ReadOnly bool
	// User is recorded as the owner of entities the transaction creates.
	User string
}

// Begin starts a transaction. It must end with Commit or Rollback.
func (db *DB) Begin(opts TxOptions) *Tx {
	return &Tx{
		db:   db,
		t:    db.mgr.Begin(opts.ReadOnly),
		base: db.cat.Load(),
		user: opts.User,
		now:  db.eng.opts.Now(),
	}
}

// Update runs fn in a read-write transaction and commits it if fn returns
// nil. It doesn't retry on conflict; see Retry.
func (db *DB) Update(fn func(*Tx) error) error {
	return db.run(TxOptions{}, fn)
}

// View runs fn in a read-only transaction.
func (db *DB) View(fn func(*Tx) error) error {
	return db.run(TxOptions{ReadOnly: true}, fn)
}

func (db *DB) run(opts TxOptions, fn func(*Tx) error) error {
	tx := db.Begin(opts)
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Retry runs fn in read-write transactions until it commits without a
// conflict, at most attempts times.
func (db *DB) Retry(attempts int, fn func(*Tx) error) error {
	var err error
	for range max(attempts, 1) {
		if err = db.Update(fn); !IsConflict(err) {
			return err
		}
	}
	return err
}

// setCatalog installs c if it is newer than the current catalog.
func (db *DB) setCatalog(c *catalog.Catalog) {
	for {
		cur := db.cat.Load()
		if c.Version() <= cur.Version() {
			return
		}
		if db.cat.CompareAndSwap(cur, c) {
			db.exprs.Clear()
			return
		}
	}
}

func (db *DB) close() error {
	db.stopStats()
	db.stopSweeper()
	db.stopBuilder()
	db.stopAlterer()
	db.wmu.Lock()
	for _, wl := range db.waiters {
		for _, w := range wl.list {
			w.closed = true
			w.signal()
		}
	}
	db.wmu.Unlock()
	err := db.mgr.Store().Close()
	if berr := db.blobs.Close(); err == nil {
		err = berr
	}
	return err
}

// StoreMetrics returns the on-disk store's shape and work since it
// opened; ok is false for a database kept in memory.
func (db *DB) StoreMetrics() (m lsm.Metrics, ok bool) {
	s, ok := db.mgr.Store().(*lsm.Store)
	if !ok {
		return m, false
	}
	return s.Metrics(), true
}
