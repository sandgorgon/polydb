package session

import (
	"context"
	"time"

	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/value"
)

// backupPath evaluates a backup or restore statement's path (constants
// and parameters) and resolves it under the engine's backup directory.
func (s *Session) backupPath(st lang.Stmt, e lang.Expr, params map[string]value.Value) (string, error) {
	// Check the path alone: the statement may hold a query, which needs
	// a database.
	info, err := lang.Check(&lang.Env{Params: params}, &lang.Backup{Path: e})
	if err != nil {
		return "", err
	}
	v, err := (&lang.Evaluator{Info: info, Params: params, Now: time.Now()}).Eval(e, nil)
	if err != nil {
		return "", err
	}
	t, ok := v.(value.Text)
	if !ok {
		return "", sessionErr(st, "type", "a backup path is text, not %s", v.Code())
	}
	p, err := s.eng.BackupPath(t.Go())
	if err != nil {
		return "", fail(st, err)
	}
	return p, nil
}

func intCol(name string) query.Column {
	return query.Column{Name: name, Type: lang.Static{Type: value.TypeInt}}
}

// backup runs "backup database d to 'path'". It needs admin permission
// on the database: a backup holds all of it.
func (r *Request) backup(ctx context.Context, st *lang.Backup, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	if err := r.noTx(st, "backup database"); err != nil {
		return nil, err
	}
	db, err := s.eng.Database(st.Name)
	if err != nil {
		return nil, fail(st, err)
	}
	if !s.eng.Security().Allowed(s.user, security.Admin, st.Name, value.ID{}) {
		return nil, denied(st, "backup database needs admin permission on database %s", st.Name)
	}
	path, err := s.backupPath(st, st.Path, params)
	if err != nil {
		return nil, err
	}
	info, err := db.Backup(ctx, nil, path)
	if err != nil {
		return nil, fail(st, err)
	}
	return &query.Result{
		Columns: []query.Column{textCol("database"), textCol("path"), intCol("keys"), intCol("files"), intCol("bytes")},
		Rows: [][]any{{value.MustText(st.Name), value.MustText(path), value.Int(info.Keys),
			value.Int(len(info.Files)), value.Int(info.Bytes())}},
	}, nil
}

// restore runs "restore database d from 'path'", which creates d: it
// needs admin permission on the server, like create database.
func (r *Request) restore(ctx context.Context, st *lang.Restore, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	if err := r.noTx(st, "restore database"); err != nil {
		return nil, err
	}
	if err := s.needAdmin(st, "restore database"); err != nil {
		return nil, err
	}
	path, err := s.backupPath(st, st.Path, params)
	if err != nil {
		return nil, err
	}
	_, info, err := s.eng.RestoreDatabase(ctx, st.Name, nil, path)
	if err != nil {
		return nil, fail(st, err)
	}
	created, _ := value.NewTime(info.Created)
	return &query.Result{
		Columns: []query.Column{textCol("database"), textCol("from"), {Name: "backed_up", Type: lang.Static{Type: value.TypeTime}},
			intCol("keys"), intCol("files"), intCol("bytes")},
		Rows: [][]any{{value.MustText(st.Name), value.MustText(info.Database), created, value.Int(info.Keys),
			value.Int(len(info.Files)), value.Int(info.Bytes())}},
	}, nil
}
