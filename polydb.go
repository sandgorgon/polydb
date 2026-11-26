// Package polydb is PolyDB's application interface. A DB runs Rhizome
// statements either on a database inside this program (Open, OpenMemory)
// or on a server (package client); both implement the same interfaces,
// so an application can switch between the two (spec/protocol.md §13).
//
//	db, err := polydb.Open("./data", nil) // or polydb.OpenMemory(nil)
//	// or: db, err := client.Dial(ctx, "polydb://ana@db.example.com/sales", client.WithPassword(pw))
//	rows, err := db.Query(ctx, `find Person p | where p.age > $min | show p.name`, polydb.Params{"min": 30})
//	for rows.Next() {
//		var name string
//		err = rows.Scan(&name)
//	}
package polydb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/sandgorgon/polydb/internal/version"
	"github.com/sandgorgon/polydb/value"
)

// Version returns this PolyDB's version, for example "1.0.0".
func Version() string { return version.String() }

// DB is a connection to a PolyDB database: over the network
// (client.Dial), or to one inside this program (Open, OpenMemory). It is
// safe for concurrent use.
type DB interface {
	Querier
	// Prepare prepares one statement for repeated execution.
	Prepare(ctx context.Context, text string) (Stmt, error)
	// Begin starts an explicit transaction.
	Begin(ctx context.Context, opts TxOptions) (Tx, error)
	// Close releases the connection, rolling back open transactions.
	Close() error
}

// Querier runs Rhizome text. DB and Tx both are.
type Querier interface {
	// Query runs a script and returns its results, positioned at the
	// first statement's result set.
	Query(ctx context.Context, text string, params Params) (Rows, error)
	// Exec runs a script and returns what its statements changed.
	Exec(ctx context.Context, text string, params Params) (Result, error)
}

// Tx is an explicit transaction (spec/rhizome.md §9). After a statement in
// it fails, everything but Rollback fails with code txn_aborted.
type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// TxOptions configure Begin.
type TxOptions struct {
	ReadOnly bool
}

// Stmt is a prepared statement.
type Stmt interface {
	Query(ctx context.Context, params Params) (Rows, error)
	Exec(ctx context.Context, params Params) (Result, error)
	Close() error
}

// Rows iterates over the results of a script: one result set per
// statement, each with columns and rows.
type Rows interface {
	// Next advances to the next row of the current result set.
	Next() bool
	// Scan copies the current row's cells into dest (see Scan).
	Scan(dest ...any) error
	// Values returns the current row's cells: value.Value, *Entity,
	// *Relation, *Path or *Blob.
	Values() []any
	// Columns returns the current result set's columns.
	Columns() []Column
	// NextResultSet moves to the next statement's result set.
	NextResultSet() bool
	// Result returns what the current statement changed, once its rows
	// have been read.
	Result() Result
	// Err returns the error that stopped iteration, if any.
	Err() error
	// Close stops reading; the rest of the results are discarded.
	Close() error
}

// Column is a result column: its name and type expression ("any" for
// entities, relations and paths).
type Column struct {
	Name string
	Type string
}

// Result is what a statement changed.
type Result struct {
	RowsAffected int64
	CreatedIDs   []value.ID
	Warnings     []string
}

// Params are the values of $parameters. Go values are converted with
// ToValue.
type Params map[string]any

// Entity is a whole entity in a result.
type Entity struct {
	ID     value.ID
	Kind   string // "" for kindless
	Meta   Meta
	Fields []*Field
}

// Field returns the field with the given name.
func (e *Entity) Field(name string) (*Field, bool) {
	for _, f := range e.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return nil, false
}

// Value returns a field's value, or nil if it is absent or not there.
func (e *Entity) Value(name string) any {
	if f, ok := e.Field(name); ok {
		return f.Value
	}
	return nil
}

// Meta is an entity's metadata.
type Meta struct {
	Created, Updated time.Time
	Version          int64
	Labels           []string
	Owner            string
	Props            map[string]value.Value
}

// Field is one field of an entity.
type Field struct {
	ID       value.ID
	Name     string
	Type     string
	Doc      string
	Declared bool
	Value    any // value.Value, *Blob, or nil when absent
}

// Relation is a whole relation in a result.
type Relation struct {
	ID       value.ID
	Type     string
	From, To value.ID
	Props    map[string]value.Value
}

// Path is a path in a result: its nodes (entities, or field IDs) and the
// relations between them.
type Path struct {
	Nodes []any
	Rels  []*Relation
}

// Blob is a large text, bytes or image value: one a query returned,
// whose content is read only when the application reads it, or one
// uploaded (Uploader) or made with NewBlob to pass as a parameter.
type Blob struct {
	Type      string // "text", "bytes" or "image"
	Length    int64
	MediaType string
	src       BlobSource
	// lazy is the value an embedded database returned, which goes back
	// as a parameter without being read.
	lazy *value.Lazy
}

// BlobSource supplies a Blob's content.
type BlobSource interface {
	// ReadBlob reads length bytes at offset.
	ReadBlob(ctx context.Context, offset, length int64) ([]byte, error)
}

// BlobFunc is a function that is a BlobSource.
type BlobFunc func(ctx context.Context, offset, length int64) ([]byte, error)

// ReadBlob calls f.
func (f BlobFunc) ReadBlob(ctx context.Context, offset, length int64) ([]byte, error) {
	return f(ctx, offset, length)
}

// NewBlob returns a blob of length bytes whose content src reads; typ
// is "text", "bytes" or "image". Passed as a parameter, its content is
// read whole and sent with the statement; for content too large for
// that, use Uploader.Upload.
func NewBlob(typ string, length int64, mediaType string, src BlobSource) *Blob {
	return &Blob{Type: typ, Length: length, MediaType: mediaType, src: src}
}

// Source returns what reads the blob's content.
func (b *Blob) Source() BlobSource { return b.src }

func (b *Blob) fetch(ctx context.Context, offset, length int64) ([]byte, error) {
	return b.src.ReadBlob(ctx, offset, length)
}

// ReadAt implements io.ReaderAt.
func (b *Blob) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.Length {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.Length-off)
	data, err := b.fetch(context.Background(), off, n)
	copy(p, data)
	if err == nil && int64(len(data)) < int64(len(p)) {
		err = io.EOF
	}
	return len(data), err
}

// Reader returns a reader of the whole blob. It fetches the content in
// pieces of 1 MiB, however little each Read asks for, and io.Copy
// writes each piece as it arrives.
func (b *Blob) Reader() io.Reader { return &blobReader{b: b} }

// blobPiece is how much a blob's Reader fetches at a time.
const blobPiece = 1 << 20

type blobReader struct {
	b   *Blob
	off int64  // where buf starts in the blob
	buf []byte // fetched, not yet read
}

func (r *blobReader) fill() error {
	if len(r.buf) > 0 {
		return nil
	}
	if r.off >= r.b.Length {
		return io.EOF
	}
	n := min(blobPiece, r.b.Length-r.off)
	data, err := r.b.fetch(context.Background(), r.off, n)
	if err != nil {
		return err
	}
	if int64(len(data)) != n {
		return io.ErrUnexpectedEOF
	}
	r.buf = data
	return nil
}

func (r *blobReader) Read(p []byte) (int, error) {
	if err := r.fill(); err != nil {
		return 0, err
	}
	n := copy(p, r.buf)
	r.buf, r.off = r.buf[n:], r.off+int64(n)
	return n, nil
}

// WriteTo writes the rest of the blob to w, a piece at a time.
func (r *blobReader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for {
		if err := r.fill(); err == io.EOF {
			return total, nil
		} else if err != nil {
			return total, err
		}
		n, err := w.Write(r.buf)
		total += int64(n)
		r.buf, r.off = r.buf[n:], r.off+int64(n)
		if err != nil {
			return total, err
		}
	}
}

// Bytes reads the whole blob.
func (b *Blob) Bytes(ctx context.Context) ([]byte, error) { return b.fetch(ctx, 0, b.Length) }

// Value reads the whole blob as the value it stands for.
func (b *Blob) Value(ctx context.Context) (value.Value, error) {
	if b.lazy != nil {
		return b.lazy.Load()
	}
	data, err := b.Bytes(ctx)
	if err != nil {
		return nil, err
	}
	switch b.Type {
	case "text":
		return value.NewText(string(data))
	case "image":
		return value.NewImage(data)
	}
	return value.NewBytes(data, b.MediaType)
}

// Error is an error reported by PolyDB, with a Rhizome error code
// (spec/rhizome.md §14, spec/protocol.md §11.2).
type Error struct {
	Code           string
	Message        string
	Line, Column   int // 0 if not tied to a position
	StatementIndex int // -1 if not tied to a statement
	Retryable      bool
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%d:%d: %s: %s", e.Line, e.Column, e.Code, e.Message)
	}
	return e.Code + ": " + e.Message
}

// CodeOf returns the PolyDB error code of err, or "" for other errors.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsRetryable reports whether err is safe to retry from the start of the
// transaction: a conflict, or a transaction that was busy.
func IsRetryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}

// RetryTx runs fn in a transaction and commits it, retrying from the
// start when the transaction conflicts with another (up to 10 attempts,
// with a short random backoff). fn must not have effects outside the
// transaction that can't be repeated.
func RetryTx(ctx context.Context, db DB, fn func(Tx) error) error {
	var err error
	for attempt := range 10 {
		if attempt > 0 {
			d := time.Duration(rand.Int64N(int64(5*time.Millisecond<<min(attempt, 6)))) + time.Millisecond
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var tx Tx
		if tx, err = db.Begin(ctx, TxOptions{}); err != nil {
			return err
		}
		if err = fn(tx); err == nil {
			if err = tx.Commit(ctx); err == nil {
				return nil
			}
		} else {
			tx.Rollback(ctx)
		}
		if !IsRetryable(err) {
			return err
		}
	}
	return err
}
