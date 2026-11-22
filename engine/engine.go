// Package engine is PolyDB's embeddable database engine: databases,
// transactions, definitions and data operations, as a Go API. The Rhizome
// language (Phase 2) and the server (Phase 3) are built on it.
//
// An Engine hosts databases. Each database has its own store, catalog and
// transaction manager. Work happens in transactions:
//
//	db, _ := eng.CreateDatabase("sales")
//	err := db.Update(func(tx *engine.Tx) error {
//		id, err := tx.Create("Person", engine.Fields{"name": value.MustText("Ann")})
//		...
//	})
package engine

import (
	"slices"
	"sync"
	"time"

	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/mem"
	"github.com/sandgorgon/polydb/txn"
	"github.com/sandgorgon/polydb/value"
)

// Options configure an Engine.
type Options struct {
	// Node is this machine's node ID, embedded in generated IDs (0 on a
	// single machine).
	Node uint16
	// NewStore creates the store for a new database. The default is an
	// in-memory store.
	NewStore func(database string) (storage.Store, error)
	// Now returns the current time; the default is time.Now. Tests set it.
	Now func() time.Time
	// MaxPopWait caps how long PopWait may wait (default 15 minutes).
	MaxPopWait time.Duration
}

// Engine hosts databases. It is safe for concurrent use.
type Engine struct {
	opts Options
	ids  *value.IDGenerator

	mu     sync.RWMutex
	dbs    map[string]*DB
	closed bool
}

// Open returns a new engine.
func Open(opts Options) (*Engine, error) {
	if opts.NewStore == nil {
		opts.NewStore = func(string) (storage.Store, error) { return mem.New(), nil }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxPopWait <= 0 {
		opts.MaxPopWait = 15 * time.Minute
	}
	return &Engine{opts: opts, ids: value.NewIDGenerator(opts.Node), dbs: map[string]*DB{}}, nil
}

// CreateDatabase creates a new, empty database.
func (e *Engine) CreateDatabase(name string) (*DB, error) {
	if err := value.ValidateName("database", name); err != nil {
		return nil, wrap(CodeType, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errorf(CodeInternal, "engine is closed")
	}
	if _, ok := e.dbs[name]; ok {
		return nil, errorf(CodeConstraint, "database %s already exists", name)
	}
	s, err := e.opts.NewStore(name)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	db, err := openDB(e, name, s)
	if err != nil {
		s.Close()
		return nil, err
	}
	e.dbs[name] = db
	return db, nil
}

// Database returns the named database.
func (e *Engine) Database(name string) (*DB, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	db, ok := e.dbs[name]
	if !ok {
		return nil, errorf(CodeNotFound, "no database %s", name)
	}
	return db, nil
}

// Databases returns the database names, sorted.
func (e *Engine) Databases() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.dbs))
	for n := range e.dbs {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// DropDatabase closes and removes a database and all its data.
func (e *Engine) DropDatabase(name string) error {
	e.mu.Lock()
	db, ok := e.dbs[name]
	delete(e.dbs, name)
	e.mu.Unlock()
	if !ok {
		return errorf(CodeNotFound, "no database %s", name)
	}
	return db.close()
}

// Close closes every database.
func (e *Engine) Close() error {
	e.mu.Lock()
	dbs := e.dbs
	e.dbs, e.closed = map[string]*DB{}, true
	e.mu.Unlock()
	var first error
	for _, db := range dbs {
		if err := db.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func openDB(e *Engine, name string, s storage.Store) (*DB, error) {
	cat, err := catalog.Load(s, 0)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	db := &DB{name: name, eng: e, mgr: txn.NewManager(s), waiters: map[string]*waitList{}}
	db.cat.Store(cat)
	db.mgr.OnCommit(db.notify)
	return db, nil
}
