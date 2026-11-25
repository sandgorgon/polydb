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
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/lsm"
	"github.com/sandgorgon/polydb/storage/mem"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/storage/wal"
	"github.com/sandgorgon/polydb/txn"
	"github.com/sandgorgon/polydb/value"
)

// Options configure an Engine.
type Options struct {
	// Node is this machine's node ID, embedded in generated IDs (0 on a
	// single machine).
	Node uint16
	// Dir is where the engine keeps its data: a system store (the list of
	// databases, and users, roles, grants and keys) and one store per
	// database, all reopened by the next Open. "" keeps everything in
	// memory, as NewStore says.
	Dir string
	// FS is the file system Dir is on; the default is the OS's.
	FS vfs.FS
	// Durability is when a commit is acknowledged: once it is on stable
	// storage (Durable, the default), or once it is written, with the
	// log synced in the background (Relaxed, for bulk loads; a crash can
	// lose the last moments of commits, never part of one).
	Durability wal.Durability
	// Storage tunes the on-disk stores (memtable and table sizes, cache);
	// the engine sets their FS, Dir and Durability.
	Storage lsm.Options
	// NewStore creates the store for a new database when Dir is "". The
	// default is an in-memory store.
	NewStore func(database string) (storage.Store, error)
	// Now returns the current time; the default is time.Now. Tests set it.
	Now func() time.Time
	// MaxPopWait caps how long PopWait may wait (default 15 minutes).
	MaxPopWait time.Duration
	// HistorySweep is how often each database removes history past its
	// retention (DB.SweepHistory); default an hour, negative never.
	HistorySweep time.Duration
	// StatsRefresh is how often each database samples the statistics of
	// kinds that have none or have changed by about a tenth (DB.Analyze);
	// default a minute, negative never.
	StatsRefresh time.Duration
	// BlobThreshold is the length in bytes above which text, bytes and
	// image field values are kept in the blob store (default 16 KiB, at
	// least 4 KiB).
	BlobThreshold int
	// BlobSegmentSize is the size of the blob store's segment files
	// (default blob.DefaultSegmentSize).
	BlobSegmentSize int64
	// BackupDir is where backup and restore statements read and write:
	// their paths are relative to it (default <Dir>/backups; with no
	// Dir, the statements are refused). DB.Backup and RestoreDatabase
	// take any path.
	BackupDir string
	// MaxLoad is how many bytes of large values one statement may load
	// into memory, for functions and operators that need their content:
	// the default of sessions' max_load setting (default DefaultMaxLoad,
	// negative no limit). Passing large values along (showing, storing,
	// len, =) loads nothing.
	MaxLoad int64
	// PasswordIterations is the PBKDF2 iteration count for new passwords
	// (default security.DefaultIterations, at least security.MinIterations).
	PasswordIterations int
}

// Engine hosts databases. It is safe for concurrent use.
type Engine struct {
	opts Options
	ids  *value.IDGenerator

	sec *security.Store
	sys *lsm.Store // the system store, with a Dir

	mu     sync.RWMutex
	dbs    map[string]*DB
	closed bool
}

// Security returns the engine's system store: users, roles, grants and
// keys.
func (e *Engine) Security() *security.Store { return e.sec }

// MaxLoad returns how many bytes of large values a statement may load,
// or a negative number for no limit (Options.MaxLoad).
func (e *Engine) MaxLoad() int64 { return e.opts.MaxLoad }

// Open returns an engine: a new, empty one in memory, or the one kept in
// opts.Dir (created if needed).
func Open(opts Options) (*Engine, error) {
	if opts.NewStore == nil {
		opts.NewStore = func(string) (storage.Store, error) { return mem.New(), nil }
	}
	if opts.FS == nil {
		opts.FS = vfs.OS
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxPopWait <= 0 {
		opts.MaxPopWait = 15 * time.Minute
	}
	if opts.HistorySweep == 0 {
		opts.HistorySweep = time.Hour
	}
	if opts.StatsRefresh == 0 {
		opts.StatsRefresh = time.Minute
	}
	if opts.BlobThreshold == 0 {
		opts.BlobThreshold = DefaultBlobThreshold
	}
	opts.BlobThreshold = max(opts.BlobThreshold, minBlobThreshold)
	if opts.BackupDir == "" && opts.Dir != "" {
		opts.BackupDir = vfs.Join(opts.Dir, "backups")
	}
	if opts.MaxLoad == 0 {
		opts.MaxLoad = DefaultMaxLoad
	}
	e := &Engine{opts: opts, ids: value.NewIDGenerator(opts.Node), dbs: map[string]*DB{},
		sec: security.New(opts.PasswordIterations)}
	if opts.Dir != "" {
		if err := e.load(); err != nil {
			return nil, err
		}
	}
	return e, nil
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
	if e.sys != nil {
		db, err := e.createOnDisk(name, nil)
		if err != nil {
			return nil, err
		}
		e.dbs[name] = db
		return db, nil
	}
	s, err := e.opts.NewStore(name)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	db, err := openDB(e, name, s, blobPlace{})
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
	defer e.mu.Unlock()
	db, ok := e.dbs[name]
	if !ok {
		return errorf(CodeNotFound, "no database %s", name)
	}
	// Grants go first: if a crash interrupts the drop, the database may
	// survive without them, but a database created later with the same
	// name never inherits them.
	if err := e.sec.DropDatabase(name); err != nil {
		return wrap(CodeInternal, err)
	}
	if e.sys != nil {
		if err := e.dropOnDisk(name, db); err != nil {
			return err
		}
		delete(e.dbs, name)
		return nil
	}
	delete(e.dbs, name)
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
	if e.sys != nil {
		if err := e.sys.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openDB opens a database on store s, with its blob store in blobDir
// ("" for one in memory).
func openDB(e *Engine, name string, s storage.Store, bp blobPlace) (*DB, error) {
	cat, err := catalog.Load(s, 0)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	blobs, err := openBlobs(e, bp)
	if err != nil {
		return nil, err
	}
	db := &DB{name: name, eng: e, mgr: txn.NewManager(s), waiters: map[string]*waitList{}, blobs: blobs}
	db.cat.Store(cat)
	db.mgr.OnCommit(db.notify)
	db.startBuilder()
	if err := db.loadAlters(); err != nil {
		db.stopBuilder()
		return nil, err
	}
	db.startAlterer()
	db.startSweeper(e.opts.HistorySweep)
	db.startStats(e.opts.StatsRefresh)
	return db, nil
}
