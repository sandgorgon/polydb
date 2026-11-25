package session

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/transfer"
	"github.com/sandgorgon/polydb/value"
)

// exportTTL is how long an export returned as a value waits in the
// blob store for its download to start; the download's lease then keeps
// it.
const exportTTL = 10 * time.Minute

// mediaTypes are the media types of exports returned as values.
var mediaTypes = map[transfer.Format]string{
	transfer.JSONL: "application/x-ndjson", transfer.Native: "application/x-polydb-export",
	transfer.CSV: "text/csv", transfer.DOT: "text/vnd.graphviz", transfer.GraphJSON: "application/json",
}

// evalLazy evaluates a statement's constant expression, keeping a large
// value (an uploaded export) unread.
func (s *Session) evalLazy(st lang.Stmt, e lang.Expr, params map[string]value.Value) (value.Value, error) {
	info, err := lang.Check(&lang.Env{Params: params}, st)
	if err != nil {
		return nil, err
	}
	return (&lang.Evaluator{Info: info, Params: params, Now: time.Now()}).EvalLazy(e, nil)
}

// export runs export database, export security and export (query).
func (r *Request) export(ctx context.Context, st *lang.Export, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	if err := r.noTx(st, "export"); err != nil {
		return nil, err
	}
	format := transfer.Format(st.Format)
	if format == "" {
		format = transfer.JSONL
	}
	if _, ok := mediaTypes[format]; !ok || (st.What != "query" && format != transfer.JSONL && format != transfer.Native) {
		return nil, sessionErr(st, "type", "export %s as %s: the formats are jsonl and native (and for a query csv, dot and graph)", st.What, format)
	}

	// What to write, and where the result lives when there's no path.
	var write func(w io.Writer) (*transfer.Stats, error)
	var home *engine.DB
	switch st.What {
	case "database":
		db, err := s.eng.Database(st.Name)
		if err != nil {
			return nil, fail(st, err)
		}
		if !s.eng.Security().Allowed(s.user, security.Read, st.Name, value.ID{}) {
			return nil, denied(st, "export database needs read permission on all of database %s", st.Name)
		}
		home = db
		write = func(w io.Writer) (*transfer.Stats, error) { return transfer.ExportDatabase(ctx, db, w, format) }
	case "security":
		if err := s.needAdmin(st, "export security"); err != nil {
			return nil, err
		}
		write = func(w io.Writer) (*transfer.Stats, error) {
			ss, err := transfer.ExportSecurity(ctx, s.eng, w, format, !st.NoPasswords)
			if err != nil {
				return nil, err
			}
			return &transfer.Stats{Entities: int64(ss.Users), Relations: int64(ss.Grants), Bytes: ss.Bytes}, nil
		}
	default:
		db, err := r.db(st)
		if err != nil {
			return nil, err
		}
		home = db
		lazy := r.LazyResults
		r.LazyResults = true // large values stream into the export
		res, err := r.inTx(ctx, st.Query, params)
		r.LazyResults = lazy
		if err != nil {
			return nil, err
		}
		write = func(w io.Writer) (*transfer.Stats, error) {
			tx := db.Begin(engine.TxOptions{ReadOnly: true})
			defer tx.Rollback()
			return transfer.ExportResult(ctx, tx, res, w, format)
		}
	}
	if home == nil {
		home = s.currentDB()
	}

	if st.Path != nil {
		path, err := s.backupPath(st, st.Path, params)
		if err != nil {
			return nil, err
		}
		f, err := s.eng.CreateExportFile(path)
		if err != nil {
			return nil, fail(st, err)
		}
		stats, err := write(f)
		if err != nil {
			f.Abort()
			return nil, fail(st, err)
		}
		if err := f.Commit(); err != nil {
			return nil, fail(st, err)
		}
		return exportResult(textCell(path), "path", stats), nil
	}
	if home == nil {
		// Only a security export has no database; it is small, so it is
		// returned from memory.
		var buf bytes.Buffer
		stats, err := write(&buf)
		if err != nil {
			return nil, fail(st, err)
		}
		return exportResult(value.BytesOf(buf.Bytes(), mediaTypes[format]), "export", stats), nil
	}
	pr, pw := io.Pipe()
	var stats *transfer.Stats
	go func() {
		var err error
		stats, err = write(pw)
		pw.CloseWithError(err)
	}()
	u, err := home.StoreUpload(pr, value.CodeBytes, value.MaxBytesLen, exportTTL)
	pr.CloseWithError(io.ErrClosedPipe) // stop the writer if storing failed
	if err != nil {
		return nil, fail(st, err)
	}
	v, err := home.UploadValue(u, mediaTypes[format])
	if err != nil {
		return nil, fail(st, err)
	}
	if u.Len <= int64(16<<10) {
		if v, err = value.Load(v); err != nil {
			return nil, fail(st, err)
		}
	}
	return exportResult(v, "export", stats), nil
}

func textCell(s string) value.Value { return value.MustText(s) }

// exportResult is an export's one row: the export or its path, and
// counts.
func exportResult(first value.Value, name string, st *transfer.Stats) *query.Result {
	col := query.Column{Name: name, Type: lang.Static{Type: value.TypeBytes}}
	if name == "path" {
		col = textCol(name)
	}
	if st == nil {
		st = &transfer.Stats{}
	}
	return &query.Result{
		Columns: []query.Column{col, intCol("entities"), intCol("relations"), intCol("blobs"), intCol("bytes")},
		Rows:    [][]any{{first, value.Int(st.Entities), value.Int(st.Relations), value.Int(st.Blobs), value.Int(st.Bytes)}},
	}
}

// currentDB returns the database in use, or nil.
func (s *Session) currentDB() *engine.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db
}

// importStmt runs import into database and import security.
func (r *Request) importStmt(ctx context.Context, st *lang.Import, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	if err := r.noTx(st, "import"); err != nil {
		return nil, err
	}
	src, err := s.evalLazy(st, st.Source, params)
	if err != nil {
		return nil, err
	}
	var in io.Reader
	switch v := src.(type) {
	case value.Text:
		path, err := s.eng.BackupPath(v.Go())
		if err != nil {
			return nil, fail(st, err)
		}
		f, err := s.eng.OpenExportFile(path)
		if err != nil {
			return nil, fail(st, err)
		}
		defer f.Close()
		in = f
	case value.Lazy:
		in = io.NewSectionReader(v.Src, 0, v.Len())
	case value.Bytes:
		raw, _, _ := value.Large(v)
		in = bytes.NewReader(raw)
	default:
		return nil, sessionErr(st, "type", "import reads a path (text) or an export (bytes), not %s", src.Code())
	}

	if st.What == "security" {
		if err := s.needAdmin(st, "import security"); err != nil {
			return nil, err
		}
		ss, err := transfer.ImportSecurity(ctx, s.eng, in, st.Replace)
		if err != nil {
			return nil, fail(st, err)
		}
		return &query.Result{
			Columns: []query.Column{intCol("users"), intCol("roles"), intCol("grants"), intCol("kept"), intCol("skipped")},
			Rows:    [][]any{{value.Int(ss.Users), value.Int(ss.Roles), value.Int(ss.Grants), value.Int(ss.Kept), value.Int(ss.Skipped)}},
		}, nil
	}
	db, err := s.eng.Database(st.Name)
	if err != nil {
		return nil, fail(st, err)
	}
	if !s.eng.Security().Allowed(s.user, security.Admin, st.Name, value.ID{}) {
		return nil, denied(st, "import needs admin permission on database %s", st.Name)
	}
	is, err := transfer.ImportDatabase(ctx, db, in, transfer.ImportOptions{NewIDs: st.NewIDs})
	if err != nil {
		msg := err.Error()
		code := "constraint"
		if strings.Contains(msg, "isn't a database export") || strings.Contains(msg, "format") {
			code = "type"
		}
		if ee := engine.CodeOf(err); ee != "" && ee != engine.CodeInternal {
			code = string(ee)
		}
		return nil, sessionErr(st, code, "%s", msg)
	}
	return &query.Result{
		Columns: []query.Column{intCol("definitions"), intCol("entities"), intCol("relations"), intCol("blobs")},
		Rows:    [][]any{{value.Int(is.Definitions), value.Int(is.Entities), value.Int(is.Relations), value.Int(is.Blobs)}},
	}, nil
}
