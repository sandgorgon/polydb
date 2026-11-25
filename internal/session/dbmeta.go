package session

import (
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/internal/query"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/value"
)

// alterDatabase runs "alter database d set …", which changes d's doc,
// labels or props (spec/rhizome.md §4, Database metadata). It needs
// admin permission on d, and runs outside transactions, like the other
// statements that act on a whole database.
func (r *Request) alterDatabase(st *lang.AlterDatabase, params map[string]value.Value) (*query.Result, error) {
	s := r.s
	if err := r.noTx(st, "alter database"); err != nil {
		return nil, err
	}
	db, err := s.eng.Database(st.Name)
	if err != nil {
		return nil, fail(st, err)
	}
	if !s.eng.Security().Allowed(s.user, security.Admin, st.Name, value.ID{}) {
		return nil, denied(st, "alter database needs admin permission on database %s", st.Name)
	}
	info, err := lang.Check(&lang.Env{Params: params}, st)
	if err != nil {
		return nil, err
	}
	ev := &lang.Evaluator{Info: info, Params: params, Now: time.Now()}
	err = db.Update(func(tx *engine.Tx) error {
		m, err := tx.DatabaseMeta()
		if err != nil {
			return err
		}
		for _, a := range st.Assigns {
			v, err := ev.Eval(a.Value, nil)
			if err != nil {
				return err
			}
			if err := applyDBMeta(&m, a, v); err != nil {
				return err
			}
		}
		return tx.SetDatabaseMeta(m)
	})
	if err != nil {
		return nil, fail(st, err)
	}
	return &query.Result{Affected: 1}, nil
}

// applyDBMeta applies one checked assignment to m.
func applyDBMeta(m *engine.DatabaseMeta, a *lang.Assign, v value.Value) error {
	t := a.Target
	switch {
	case t.Name == "doc":
		m.Doc = ""
		if d, ok := v.(value.Text); ok {
			m.Doc = d.Go()
		}
	case t.Name == "labels":
		s, _ := v.(value.Set)
		switch a.Op {
		case "=":
			m.Labels = s
		case "++=":
			u, err := value.NewSet(append(m.Labels.Values(), s.Values()...)...)
			if err != nil {
				return err
			}
			m.Labels = u
		case "--=":
			var keep []value.Value
			for _, l := range m.Labels.Values() {
				if !s.Has(l) {
					keep = append(keep, l)
				}
			}
			u, err := value.NewSet(keep...)
			if err != nil {
				return err
			}
			m.Labels = u
		}
	case t.Name == "props":
		cur := map[string]value.Value{}
		for k, x := range m.Props.All() {
			cur[k.(value.Text).Go()] = x
		}
		switch {
		case len(t.Steps) == 1:
			cur[t.Steps[0].Field] = v
		case a.Op == "=":
			cur = map[string]value.Value{}
			fallthrough
		case a.Op == "++=":
			for k, x := range v.(value.Map).All() {
				cur[k.(value.Text).Go()] = x
			}
		case a.Op == "--=":
			for _, k := range v.(value.Set).Values() {
				delete(cur, k.(value.Text).Go())
			}
		}
		p, err := value.MapOfText(cur)
		if err != nil {
			return err
		}
		m.Props = p
	}
	return nil
}

// dbMetaRow reads a database's metadata for show and describe.
func dbMetaOf(db *engine.DB) (engine.DatabaseMeta, error) {
	var m engine.DatabaseMeta
	err := db.View(func(tx *engine.Tx) error {
		var err error
		m, err = tx.DatabaseMeta()
		return err
	})
	return m, err
}

func textOrNull(s string) any {
	if s == "" {
		return value.Null
	}
	return value.MustText(s)
}

// showDatabases runs "show databases": the databases this user may use,
// with their metadata.
func (s *Session) showDatabases(st lang.Stmt) (*query.Result, error) {
	r := table("name", "owner", "created", "labels", "doc")
	for _, n := range s.eng.Databases() {
		if !s.eng.Security().CanSee(s.user, n) {
			continue
		}
		db, err := s.eng.Database(n)
		if err != nil {
			continue // dropped meanwhile
		}
		m, err := dbMetaOf(db)
		if err != nil {
			return nil, fail(st, err)
		}
		add(r, n, textOrNull(m.Owner), m.Created, m.Labels, textOrNull(m.Doc))
	}
	return r, nil
}

// describeDatabase runs "describe database d".
func (s *Session) describeDatabase(st *lang.Describe) (*query.Result, error) {
	db, err := s.eng.Database(st.Name)
	if err != nil {
		return nil, fail(st, err)
	}
	if !s.eng.Security().CanSee(s.user, st.Name) {
		return nil, denied(st, "you don't have permission on database %s", st.Name)
	}
	m, err := dbMetaOf(db)
	if err != nil {
		return nil, fail(st, err)
	}
	r := table("item", "value")
	add(r, "name", st.Name)
	add(r, "owner", textOrNull(m.Owner))
	add(r, "created", m.Created)
	add(r, "doc", textOrNull(m.Doc))
	add(r, "labels", m.Labels)
	add(r, "props", m.Props)
	return r, nil
}
