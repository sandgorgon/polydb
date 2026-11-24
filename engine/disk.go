package engine

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/lsm"
	"github.com/sandgorgon/polydb/storage/vfs"
)

// An engine with a Dir keeps:
//
//	<dir>/system/          the system store
//	<dir>/databases/<n>/   one store per database, numbered, with its
//	                       blob segments in blobs/
//
// The system store holds:
//
//	format       the layout version, "1"
//	next-db      the next database number
//	db/<name>    the database's number
//	security     users, roles, grants and keys (security.Store.Marshal)
//
// The system store is the truth: a database directory it doesn't list
// is left over from an interrupted create or drop, and is deleted at
// startup. Databases are numbered rather than named on disk, so names
// that differ only in case work on file systems that ignore case.

const diskFormat = "1"

func (e *Engine) storeOptions(dir string) lsm.Options {
	o := e.opts.Storage
	o.FS, o.Dir, o.Durability = e.opts.FS, dir, e.opts.Durability
	return o
}

func (e *Engine) databasesDir() string { return vfs.Join(e.opts.Dir, "databases") }

func (e *Engine) sysGet(key string) (string, bool, error) {
	v, ok, err := e.sys.Get([]byte(key))
	return string(v), ok, err
}

func (e *Engine) sysApply(fn func(b *storage.Batch)) error {
	var b storage.Batch
	fn(&b)
	_, err := e.sys.Apply(&b)
	return err
}

// load opens the system store and every database.
func (e *Engine) load() (err error) {
	fs := e.opts.FS
	if err := fs.MkdirAll(e.databasesDir()); err != nil {
		return wrap(CodeInternal, err)
	}
	if e.sys, err = lsm.Open(e.storeOptions(vfs.Join(e.opts.Dir, "system"))); err != nil {
		return wrap(CodeInternal, fmt.Errorf("opening %s: %w", e.opts.Dir, err))
	}
	defer func() {
		if err != nil {
			for _, db := range e.dbs {
				db.close()
			}
			e.dbs = map[string]*DB{}
			e.sys.Close()
			e.sys = nil
		}
	}()
	if err := fs.SyncDir(e.opts.Dir); err != nil {
		return wrap(CodeInternal, err)
	}

	switch format, ok, err := e.sysGet("format"); {
	case err != nil:
		return wrap(CodeInternal, err)
	case !ok:
		if err := e.sysApply(func(b *storage.Batch) { b.Set([]byte("format"), []byte(diskFormat)) }); err != nil {
			return wrap(CodeInternal, err)
		}
	case format != diskFormat:
		return errorf(CodeInternal, "%s holds data in format %s, which this version doesn't read", e.opts.Dir, format)
	}

	if data, ok, err := e.sysGet("security"); err != nil {
		return wrap(CodeInternal, err)
	} else if ok {
		if err := e.sec.Restore([]byte(data)); err != nil {
			return wrap(CodeInternal, err)
		}
	}
	e.sec.PersistTo(func(data []byte) error {
		return e.sysApply(func(b *storage.Batch) { b.Set([]byte("security"), data) })
	})

	live := map[string]bool{}
	it := e.sys.Iter(storage.IterOptions{Lower: []byte("db/"), Upper: []byte("db0")})
	type entry struct{ name, num string }
	var entries []entry
	for it.Next() {
		entries = append(entries, entry{strings.TrimPrefix(string(it.Key()), "db/"), string(it.Value())})
	}
	if err := it.Err(); err != nil {
		it.Close()
		return wrap(CodeInternal, err)
	}
	it.Close()
	for _, en := range entries {
		s, err := lsm.Open(e.storeOptions(vfs.Join(e.databasesDir(), en.num)))
		if err != nil {
			return wrap(CodeInternal, fmt.Errorf("opening database %s: %w", en.name, err))
		}
		db, err := openDB(e, en.name, s, vfs.Join(e.databasesDir(), en.num, "blobs"))
		if err != nil {
			s.Close()
			return err
		}
		db.dir = en.num
		e.dbs[en.name] = db
		live[en.num] = true
	}

	// Directories left by an interrupted create or drop.
	names, err := fs.List(e.databasesDir())
	if err != nil {
		return wrap(CodeInternal, err)
	}
	removed := false
	for _, n := range names {
		if !live[n] {
			if err := fs.RemoveAll(vfs.Join(e.databasesDir(), n)); err != nil {
				return wrap(CodeInternal, err)
			}
			removed = true
		}
	}
	if removed {
		if err := fs.SyncDir(e.databasesDir()); err != nil {
			return wrap(CodeInternal, err)
		}
	}
	return nil
}

// createOnDisk creates a database's store and records it. The caller
// holds e.mu.
func (e *Engine) createOnDisk(name string) (*DB, error) {
	next := 1
	if v, ok, err := e.sysGet("next-db"); err != nil {
		return nil, wrap(CodeInternal, err)
	} else if ok {
		if next, err = strconv.Atoi(v); err != nil {
			return nil, errorf(CodeInternal, "the system store's next-db is %q", v)
		}
	}
	num := strconv.Itoa(next)
	dir := vfs.Join(e.databasesDir(), num)
	fs := e.opts.FS
	fs.RemoveAll(dir) // left by an earlier attempt
	s, err := lsm.Open(e.storeOptions(dir))
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	db, err := openDB(e, name, s, vfs.Join(dir, "blobs"))
	if err != nil {
		s.Close()
		fs.RemoveAll(dir)
		return nil, err
	}
	db.dir = num
	// The directory must be durable before the system store names it.
	err = fs.SyncDir(e.databasesDir())
	if err == nil {
		err = e.sysApply(func(b *storage.Batch) {
			b.Set([]byte("db/"+name), []byte(num))
			b.Set([]byte("next-db"), []byte(strconv.Itoa(next+1)))
		})
	}
	if err != nil {
		db.close()
		fs.RemoveAll(dir)
		return nil, wrap(CodeInternal, err)
	}
	return db, nil
}

// dropOnDisk removes a database from the system store, then deletes its
// files. The caller holds e.mu.
func (e *Engine) dropOnDisk(name string, db *DB) error {
	if err := e.sysApply(func(b *storage.Batch) { b.Delete([]byte("db/" + name)) }); err != nil {
		return wrap(CodeInternal, err)
	}
	db.close()
	// If this fails, the next startup finishes the job.
	if e.opts.FS.RemoveAll(vfs.Join(e.databasesDir(), db.dir)) == nil {
		e.opts.FS.SyncDir(e.databasesDir())
	}
	return nil
}
