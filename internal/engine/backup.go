package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/internal/storage/lsm"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// Backups (spec/rhizome.md §4). A backup of a database
// is a directory:
//
//	BACKUP     what the backup holds, as JSON: written last, so a
//	           directory without it is an unfinished backup
//	store/     the key-value store as of one snapshot, written compacted
//	           (lsm.Build): the latest version of every key, no history
//	           of deletions
//	blobs/     the blob segments, copied
//
// A backup runs while the database takes writes: it reads one snapshot,
// which also keeps the blob collector from deleting segments the
// snapshot can read, and copies the segments as they are when it starts.
// The result is the database as of one commit. Restoring checks every
// file against the SHA-256 that BACKUP records.
//
// Users, roles and grants live in the system store, not in a database,
// so a backup doesn't hold them.

// backupFormat is the version of the backup layout.
const backupFormat = 1

// BackupInfo describes a backup: the BACKUP file.
type BackupInfo struct {
	Format   int          `json:"format"`
	Database string       `json:"database"`
	Created  time.Time    `json:"created"`
	Seq      uint64       `json:"seq"` // the snapshot, in the database's store
	Keys     int64        `json:"keys"`
	Files    []BackupFile `json:"files"`
}

// BackupFile is one file of a backup, with its path relative to the
// backup's directory.
type BackupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Bytes returns the backup's total size.
func (b *BackupInfo) Bytes() int64 {
	var n int64
	for _, f := range b.Files {
		n += f.Size
	}
	return n
}

// Backup writes a consistent copy of the database to dir on fs (nil:
// the engine's file system), which must not exist or be empty. It runs
// alongside other transactions. A failed or cancelled backup removes
// what it wrote.
func (db *DB) Backup(ctx context.Context, fs vfs.FS, dir string) (info *BackupInfo, err error) {
	if fs == nil {
		fs = db.eng.opts.FS
	}
	if err := fs.MkdirAll(dir); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if names, err := fs.List(dir); err != nil {
		return nil, wrap(CodeInternal, err)
	} else if len(names) > 0 {
		return nil, errorf(CodeConstraint, "backup directory %s isn't empty", dir)
	}
	defer func() {
		if err != nil {
			fs.RemoveAll(dir)
		}
	}()
	t := db.mgr.Begin(true)
	defer t.Rollback()
	info = &BackupInfo{Format: backupFormat, Database: db.name, Created: db.eng.opts.Now().UTC(), Seq: t.Seq()}

	// The store: every key of the snapshot, written as compacted tables.
	it := t.Iter(storage.IterOptions{})
	defer it.Close()
	o := db.eng.opts.Storage
	o.FS, o.Dir = fs, vfs.Join(dir, "store")
	err = lsm.Build(o, func() ([]byte, []byte, bool, error) {
		if info.Keys%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, false, cancelled(err)
			}
		}
		if !it.Next() {
			return nil, nil, false, it.Err()
		}
		info.Keys++
		return it.Key(), it.Value(), true, nil
	})
	if err != nil {
		return nil, wrapBackup(err)
	}
	if info.Files, err = hashTree(ctx, fs, dir, "store"); err != nil {
		return nil, err
	}

	// The blob segments, as long as they are now.
	blobDir := vfs.Join(dir, "blobs")
	if err := fs.MkdirAll(blobDir); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	err = db.blobs.Copy(func(name string, size int64, r io.Reader) error {
		f, err := copyFile(ctx, fs, vfs.Join(blobDir, name), r)
		if err == nil {
			f.Path = "blobs/" + name
			info.Files = append(info.Files, f)
		}
		return err
	})
	if err != nil {
		return nil, wrapBackup(err)
	}
	if err := fs.SyncDir(blobDir); err != nil {
		return nil, wrap(CodeInternal, err)
	}

	// BACKUP last: its presence says the backup is complete.
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if _, err := copyFile(ctx, fs, vfs.Join(dir, "BACKUP.tmp"), strings.NewReader(string(data)+"\n")); err != nil {
		return nil, wrapBackup(err)
	}
	if err := fs.Rename(vfs.Join(dir, "BACKUP.tmp"), vfs.Join(dir, "BACKUP")); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if err := fs.SyncDir(dir); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	return info, nil
}

// BackupPath resolves a path that a backup or restore statement names:
// relative to Options.BackupDir, and not leaving it.
func (e *Engine) BackupPath(p string) (string, error) {
	if e.opts.BackupDir == "" {
		return "", errorf(CodeUnsupported, "backup and restore need a backup directory (polydb serve --backup-dir)")
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errorf(CodeConstraint, "a backup path is a directory name relative to the server's backup directory, not %q", p)
	}
	return vfs.Join(e.opts.BackupDir, clean), nil
}

// ExportFile is a file being written in the backup directory: it gets
// its name only when Commit succeeds.
type ExportFile struct {
	fs         vfs.FS
	f          vfs.File
	tmp, final string
	done       bool
}

// CreateExportFile starts a file for an export at path (from
// BackupPath), which must not exist yet.
func (e *Engine) CreateExportFile(p string) (*ExportFile, error) {
	fs := e.opts.FS
	if f, err := fs.Open(p); err == nil {
		f.Close()
		return nil, errorf(CodeConstraint, "%s already exists", p)
	}
	if err := fs.MkdirAll(path.Dir(p)); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	tmp := p + ".partial"
	f, err := fs.Create(tmp)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	return &ExportFile{fs: fs, f: f, tmp: tmp, final: p}, nil
}

func (x *ExportFile) Write(p []byte) (int, error) { return x.f.Write(p) }

// Commit makes the file durable under its name.
func (x *ExportFile) Commit() error {
	x.done = true
	if err := x.f.Sync(); err != nil {
		x.Abort()
		return wrap(CodeInternal, err)
	}
	if err := x.f.Close(); err != nil {
		x.fs.Remove(x.tmp)
		return wrap(CodeInternal, err)
	}
	if err := x.fs.Rename(x.tmp, x.final); err != nil {
		x.fs.Remove(x.tmp)
		return wrap(CodeInternal, err)
	}
	return wrap(CodeInternal, x.fs.SyncDir(path.Dir(x.final)))
}

// Abort removes an unfinished file.
func (x *ExportFile) Abort() {
	x.done = true
	x.f.Close()
	x.fs.Remove(x.tmp)
}

// OpenExportFile opens a file in the backup directory (from BackupPath).
func (e *Engine) OpenExportFile(p string) (io.ReadCloser, error) {
	f, err := e.opts.FS.Open(p)
	if err != nil {
		return nil, errorf(CodeNotFound, "no file %s", p)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, 0, 1<<62), f}, nil
}

// ReadBackup reads the description of the backup in dir. It doesn't
// check the files; restoring does.
func ReadBackup(fs vfs.FS, dir string) (*BackupInfo, error) {
	f, err := fs.Open(vfs.Join(dir, "BACKUP"))
	if err != nil {
		return nil, errorf(CodeNotFound, "%s holds no finished backup (no BACKUP file)", dir)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	var info BackupInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, errorf(CodeInternal, "%s/BACKUP: %v", dir, err)
	}
	if info.Format != backupFormat {
		return nil, errorf(CodeUnsupported, "%s is a backup in format %d, which this version doesn't read", dir, info.Format)
	}
	for _, bf := range info.Files {
		if !validBackupPath(bf.Path) {
			return nil, errorf(CodeInternal, "%s/BACKUP lists a bad file %q", dir, bf.Path)
		}
	}
	return &info, nil
}

// validBackupPath accepts the paths a backup's files may have: store/…
// and blobs/…, without climbing out of the directory.
func validBackupPath(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." {
		return false
	}
	return strings.HasPrefix(p, "store/") || strings.HasPrefix(p, "blobs/")
}

// RestoreDatabase creates database name from the backup in dir on fs
// (nil: the engine's file system). Every file is checked against the
// backup's description as it is copied. The engine keeps serving other
// databases meanwhile.
func (e *Engine) RestoreDatabase(ctx context.Context, name string, fs vfs.FS, dir string) (*DB, *BackupInfo, error) {
	if err := value.ValidateName("database", name); err != nil {
		return nil, nil, wrap(CodeType, err)
	}
	if fs == nil {
		fs = e.opts.FS
	}
	info, err := ReadBackup(fs, dir)
	if err != nil {
		return nil, nil, err
	}
	if _, err := e.Database(name); err == nil {
		return nil, nil, errorf(CodeConstraint, "database %s already exists", name)
	}
	var db *DB
	if e.sys != nil {
		db, err = e.restoreOnDisk(ctx, name, fs, dir, info)
	} else {
		db, err = e.restoreInMemory(ctx, name, fs, dir, info)
	}
	if err != nil {
		return nil, nil, err
	}
	return db, info, nil
}

// copyBackup copies the backup's files whose paths start with from+"/"
// to the same paths under to+"/" on dst, without the prefix, checking
// each against info.
func copyBackup(ctx context.Context, fs vfs.FS, dir string, info *BackupInfo, from string, dst vfs.FS, to string) error {
	made := map[string]bool{}
	for _, bf := range info.Files {
		rest, ok := strings.CutPrefix(bf.Path, from+"/")
		if !ok {
			continue
		}
		target := vfs.Join(to, rest)
		parent := path.Dir(target)
		if !made[parent] {
			if err := dst.MkdirAll(parent); err != nil {
				return wrap(CodeInternal, err)
			}
			made[parent] = true
		}
		src, err := fs.Open(vfs.Join(dir, bf.Path))
		if err != nil {
			return errorf(CodeInternal, "backup file %s: %v", bf.Path, err)
		}
		got, err := copyFile(ctx, dst, target, src)
		src.Close()
		if err != nil {
			return wrapBackup(err)
		}
		if got.Size != bf.Size || got.SHA256 != bf.SHA256 {
			return errorf(CodeInternal, "backup file %s is damaged: it doesn't match the backup's description", bf.Path)
		}
	}
	for d := range made {
		if err := dst.SyncDir(d); err != nil {
			return wrap(CodeInternal, err)
		}
	}
	return nil
}

// restoreOnDisk reserves a database number, copies the backup into its
// directory, and only then registers the name: the slow part runs
// without the engine's lock. An interrupted restore leaves a directory
// the system store doesn't list, which the next start deletes.
func (e *Engine) restoreOnDisk(ctx context.Context, name string, fs vfs.FS, dir string, info *BackupInfo) (*DB, error) {
	num, err := e.reserveNumber()
	if err != nil {
		return nil, err
	}
	target := vfs.Join(e.databasesDir(), num)
	ok := false
	defer func() {
		if !ok {
			e.opts.FS.RemoveAll(target)
		}
	}()
	if err := copyBackup(ctx, fs, dir, info, "store", e.opts.FS, target); err != nil {
		return nil, err
	}
	if err := e.opts.FS.MkdirAll(vfs.Join(target, "blobs")); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if err := copyBackup(ctx, fs, dir, info, "blobs", e.opts.FS, vfs.Join(target, "blobs")); err != nil {
		return nil, err
	}
	if err := e.opts.FS.SyncDir(e.databasesDir()); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	s, err := lsm.Open(e.storeOptions(target))
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	db, err := openDB(e, name, s, blobPlace{e.opts.FS, vfs.Join(target, "blobs")})
	if err != nil {
		s.Close()
		return nil, err
	}
	db.dir = num
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
		err = errorf(CodeInternal, "engine is closed")
	case e.dbs[name] != nil:
		err = errorf(CodeConstraint, "database %s already exists", name)
	default:
		err = wrap(CodeInternal, e.sysApply(func(b *storage.Batch) { b.Set([]byte("db/"+name), []byte(num)) }))
	}
	if err != nil {
		db.close()
		return nil, err
	}
	e.dbs[name] = db
	ok = true
	return db, nil
}

// reserveNumber takes the next database number for a database that
// isn't registered yet.
func (e *Engine) reserveNumber() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := 1
	if v, ok, err := e.sysGet("next-db"); err != nil {
		return "", wrap(CodeInternal, err)
	} else if ok {
		if next, err = strconv.Atoi(v); err != nil {
			return "", errorf(CodeInternal, "the system store's next-db is %q", v)
		}
	}
	err := e.sysApply(func(b *storage.Batch) { b.Set([]byte("next-db"), []byte(strconv.Itoa(next+1))) })
	return strconv.Itoa(next), wrap(CodeInternal, err)
}

// restoreInMemory loads a backup into an in-memory database: the store's
// keys into a new store, the segments into a blob store in memory.
func (e *Engine) restoreInMemory(ctx context.Context, name string, fs vfs.FS, dir string, info *BackupInfo) (*DB, error) {
	tmp := vfs.NewMem()
	if err := copyBackup(ctx, fs, dir, info, "store", tmp, "/store"); err != nil {
		return nil, err
	}
	src, err := lsm.Open(lsm.Options{FS: tmp, Dir: "/store"})
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	defer src.Close()
	s, err := e.opts.NewStore(name)
	if err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if err := copyKeys(ctx, src, s); err != nil {
		s.Close()
		return nil, err
	}
	blobFS := vfs.NewMem()
	if err := blobFS.MkdirAll("/blobs"); err != nil {
		return nil, wrap(CodeInternal, err)
	}
	if err := copyBackup(ctx, fs, dir, info, "blobs", blobFS, "/blobs"); err != nil {
		s.Close()
		return nil, err
	}
	db, err := openDB(e, name, s, blobPlace{blobFS, "/blobs"})
	if err != nil {
		s.Close()
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dbs[name] != nil || e.closed {
		db.close()
		return nil, errorf(CodeConstraint, "database %s already exists", name)
	}
	e.dbs[name] = db
	return db, nil
}

// copyKeys copies every key of src into dst, in batches.
func copyKeys(ctx context.Context, src storage.Reader, dst storage.Store) error {
	it := src.Iter(storage.IterOptions{})
	defer it.Close()
	var b storage.Batch
	size := 0
	flush := func() error {
		if size == 0 {
			return nil
		}
		_, err := dst.Apply(&b)
		b, size = storage.Batch{}, 0
		return wrap(CodeInternal, err)
	}
	for it.Next() {
		b.Set(it.Key(), it.Value())
		if size += len(it.Key()) + len(it.Value()); size >= 4<<20 {
			if err := ctx.Err(); err != nil {
				return cancelled(err)
			}
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := it.Err(); err != nil {
		return wrap(CodeInternal, err)
	}
	return flush()
}

// copyFile writes r to name on fs, durably, and describes it.
func copyFile(ctx context.Context, fs vfs.FS, name string, r io.Reader) (BackupFile, error) {
	f, err := fs.Create(name)
	if err != nil {
		return BackupFile{}, err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return BackupFile{}, cancelled(err)
		}
		k, rerr := io.ReadFull(r, buf)
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				f.Close()
				return BackupFile{}, err
			}
			h.Write(buf[:k])
			n += int64(k)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			f.Close()
			return BackupFile{}, rerr
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return BackupFile{}, err
	}
	if err := f.Close(); err != nil {
		return BackupFile{}, err
	}
	return BackupFile{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// hashTree describes the files of a store directory that lsm.Build
// wrote: its tables, and its manifest's segments.
func hashTree(ctx context.Context, fs vfs.FS, dir, sub string) ([]BackupFile, error) {
	var out []BackupFile
	var walk func(rel string) error
	walk = func(rel string) error {
		names, err := fs.List(vfs.Join(dir, rel))
		if err != nil {
			return wrap(CodeInternal, err)
		}
		for _, n := range names {
			p := rel + "/" + n
			if n == "manifest" {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			f, err := fs.Open(vfs.Join(dir, p))
			if err != nil {
				return wrap(CodeInternal, err)
			}
			h := sha256.New()
			k, err := io.Copy(h, f)
			f.Close()
			if err != nil {
				return wrap(CodeInternal, err)
			}
			if err := ctx.Err(); err != nil {
				return cancelled(err)
			}
			out = append(out, BackupFile{Path: p, Size: k, SHA256: hex.EncodeToString(h.Sum(nil))})
		}
		return nil
	}
	return out, walk(sub)
}

// cancelled reports a cancelled or timed-out backup or restore.
func cancelled(err error) error {
	return &Error{Code: CodeLimit, Msg: fmt.Sprintf("stopped: %v", err), Err: err}
}

// wrapBackup keeps engine errors and wraps others as internal.
func wrapBackup(err error) error {
	var ee *Error
	if errors.As(err, &ee) {
		return err
	}
	return wrap(CodeInternal, err)
}
