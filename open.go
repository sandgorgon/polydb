package polydb

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/storage/lsm"
	"github.com/sandgorgon/polydb/internal/storage/wal"
	"github.com/sandgorgon/polydb/value"
)

// Options configure Open and OpenMemory. The zero value is ready to use.
type Options struct {
	// Database is the database statements use, created if it doesn't
	// exist (default "main"). A script may still `use` another.
	Database string
	// Relaxed acknowledges a commit once it is written rather than once
	// it is on stable storage, which is much faster for bulk loads; a
	// crash can lose the last moments of commits, never part of one.
	Relaxed bool
	// CacheSize is the bytes of each database's block cache (default
	// 64 MiB).
	CacheSize int64
	// StatementTimeout stops a statement that runs longer (default no
	// limit).
	StatementTimeout time.Duration
	// MaxMemory is how much one statement may hold, its rows and the
	// large values it loads (default 1 GiB; negative for no limit).
	MaxMemory int64
	// BackupDir is where backup, restore, export and import statements
	// read and write files (default <dir>/backups; none in memory).
	BackupDir string
}

// Embedded is a PolyDB database running inside this program: no server,
// no network, the data in a directory or in memory. It is a DB, so code
// written against DB works the same against a server (package client).
// Statements run as the database's administrator.
type Embedded struct {
	embedded
	eng *engine.Engine
}

// Open opens the database in directory dir, creating it if it's new.
// Only one program may have a directory open at a time. Close it when
// done:
//
//	db, err := polydb.Open("./data", nil)
//	if err != nil { … }
//	defer db.Close()
func Open(dir string, opts *Options) (*Embedded, error) {
	if dir == "" {
		return nil, errors.New("polydb: Open needs a directory; use OpenMemory for a database in memory")
	}
	return open(dir, opts)
}

// OpenMemory opens a new database kept only in memory, gone when it is
// closed: for tests, caches and trying things out.
func OpenMemory(opts *Options) (*Embedded, error) { return open("", opts) }

func open(dir string, opts *Options) (*Embedded, error) {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.Database == "" {
		o.Database = "main"
	}
	eo := engine.Options{Dir: dir, BackupDir: o.BackupDir, Storage: lsm.Options{CacheSize: o.CacheSize},
		Limits: engine.Limits{StatementTimeout: o.StatementTimeout, MaxMemory: o.MaxMemory}}
	if o.Relaxed {
		eo.Durability = wal.Relaxed
	}
	eng, err := engine.Open(eo)
	if err != nil {
		return nil, convertErr(err, -1)
	}
	if _, err := eng.Database(o.Database); err != nil {
		if _, err := eng.CreateDatabaseAs(o.Database, security.AdminUser); err != nil {
			eng.Close()
			return nil, convertErr(err, -1)
		}
	}
	db, err := embed(context.Background(), eng, security.AdminUser, o.Database)
	if err != nil {
		eng.Close()
		return nil, err
	}
	return &Embedded{embedded: *db.(*embedded), eng: eng}, nil
}

// Close closes the database, rolling back open transactions.
func (db *Embedded) Close() error {
	db.embedded.Close()
	if err := db.eng.Close(); err != nil {
		return convertErr(err, -1)
	}
	return nil
}

// uploadTTL is how long an uploaded value's content waits for a
// statement to store it before the collector may remove it.
const uploadTTL = time.Hour

// Upload streams r's content into the current database's large-value
// store and returns a Blob to pass as a parameter, so a value bigger
// than memory can be stored without ever being held whole:
//
//	f, _ := os.Open("scan.png")
//	img, err := db.Upload(ctx, f, "image", "")
//	_, err = db.Exec(ctx, `create Scan { file: $img }`, polydb.Params{"img": img})
//
// typ is "text", "bytes" or "image"; mediaType is a bytes value's media
// type. Use it in a statement within an hour.
func (db *Embedded) Upload(ctx context.Context, r io.Reader, typ, mediaType string) (*Blob, error) {
	limit := int64(value.MaxBytesLen)
	var of value.Code
	switch typ {
	case "text":
		of, limit = value.CodeText, value.MaxTextBytes
	case "bytes":
		of = value.CodeBytes
	case "image":
		of = value.CodeImage
	default:
		return nil, &Error{Code: "type", Message: `an upload is "text", "bytes" or "image", not "` + typ + `"`, StatementIndex: -1}
	}
	name := db.s.Database()
	if name == "" {
		return nil, &Error{Code: "not_found", Message: "no database in use: run use <database> first", StatementIndex: -1}
	}
	edb, err := db.eng.Database(name)
	if err != nil {
		return nil, convertErr(err, -1)
	}
	if m := db.eng.Limits().MaxUpload; m > 0 {
		limit = min(limit, m)
	}
	u, err := edb.StoreUpload(ctxReader{ctx, r}, of, limit, uploadTTL)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Code: "cancelled", Message: ctx.Err().Error(), StatementIndex: -1}
		}
		code := "internal"
		if engine.CodeOf(err) == engine.CodeConstraint {
			code = "limit"
		}
		return nil, &Error{Code: code, Message: err.Error(), StatementIndex: -1}
	}
	v, err := edb.UploadValue(u, mediaType)
	if err != nil {
		return nil, &Error{Code: "type", Message: err.Error(), StatementIndex: -1}
	}
	return lazyBlob(v.(value.Lazy)), nil
}

// ctxReader stops reading when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Uploader is a DB that can stream a large value in ahead of the
// statement that stores it: *Embedded and *client.Conn.
type Uploader interface {
	DB
	Upload(ctx context.Context, r io.Reader, typ, mediaType string) (*Blob, error)
}

var _ Uploader = (*Embedded)(nil)
