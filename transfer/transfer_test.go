package transfer_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/session"
	"github.com/sandgorgon/polydb/transfer"
	"github.com/sandgorgon/polydb/value"
)

func open(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1, PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng
}

func exec(t *testing.T, s *session.Session, src string, params map[string]value.Value) {
	t.Helper()
	if _, err := s.Exec(context.Background(), src, params); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
}

// fill makes database name with a bit of everything.
func fill(t *testing.T, eng *engine.Engine, name string) {
	s := session.New(eng, security.AdminUser)
	defer s.Close()
	exec(t, s, `create database `+name+`; use `+name+`;
		define format invoice : json "Customer invoice" { number: text, total: decimal(12,2) };
		define kind Company { name: text };
		define kind Person history "A person" {
			name: text unique "Full name"
			age: int? check age >= 0
			joined: date default today()
			boss: ref<Company>? on delete restrict
			bio: text?
			photo: bytes?
			inbox: queue.priority<text>
			bill: doc<invoice>?
		};
		alter kind Company add owner : ref<Person>?;
		define relation knows from Person to Person "Acquaintance" max out 100 { since: int };
		define index person_age on Person(age);
		define text index person_bio on Person(bio)`, nil)
	big := map[string]value.Value{
		"bio":   value.MustText(strings.Repeat("long biography words ", 3000)),
		"photo": value.BytesOf(bytes.Repeat([]byte{0, 1, 2, 3}, 300<<10), "image/x-test"),
	}
	exec(t, s, `create Company @acme { name: "Acme" };
		create Person @ann { name: "Ann", age: 41, boss: @acme, bio: $bio, photo: $photo,
			bill: "{\"number\": \"7\", \"total\": 12.5}" };
		create Person @bob { name: "Bob", bio: $bio };
		update @acme set owner = @ann;
		update @ann set ::labels ++= {"vip"};
		update @ann add hobby : text = "chess" doc "Main hobby", scores : list<ref<Person>> = [@bob];
		push @ann.inbox "hi" priority 5; push @ann.inbox "yo" priority 9;
		update @ann set age = 42;
		create { title: "kindless", n: 3 } ;
		relate @ann ~knows{since: 2019}~> @bob;
		relate @bob ~likes~> @ann;
		relate @ann.name ~mentions~> @bob`, big)
}

// snapshot renders a database's contents, for comparing.
func snapshot(t *testing.T, db *engine.DB) string {
	t.Helper()
	var lines []string
	err := db.View(func(tx *engine.Tx) error {
		if err := tx.AllEntities(func(id value.ID) bool {
			e, err := tx.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			kind := ""
			if e.Kind != nil {
				kind = e.Kind.Name
			}
			m := e.Meta
			s := fmt.Sprintf("entity %s %s created=%s updated=%s v%d owner=%s labels=%s props=%s:", id, kind,
				m.Created, m.Updated, m.Version, m.Owner, m.Labels, m.Props)
			for _, f := range e.Fields {
				v, err := value.Load(f.Value)
				if err != nil {
					t.Fatal(err)
				}
				def := ""
				if f.Def != nil {
					def = f.Def.Type.String() + " " + f.Def.Doc
				}
				s += fmt.Sprintf(" %s#%s=%.60s[%d](%s)", f.Name, f.ID, v, len(v.String()), def)
			}
			lines = append(lines, s)
			return true
		}); err != nil {
			return err
		}
		return tx.AllRelations(func(r *model.Relation) bool {
			rt, _ := tx.Catalog().RelationTypeByID(r.Type)
			lines = append(lines, fmt.Sprintf("relation %s %s %s→%s %s", r.ID, rt.Name, r.From, r.To, r.Props))
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, format := range []transfer.Format{transfer.JSONL, transfer.Native} {
		t.Run(string(format), func(t *testing.T) {
			eng := open(t)
			fill(t, eng, "src")
			src, _ := eng.Database("src")
			var buf bytes.Buffer
			st, err := transfer.ExportDatabase(ctx, src, &buf, format)
			if err != nil {
				t.Fatal(err)
			}
			if st.Entities != 4 || st.Relations != 3 || st.Blobs != 2 || st.Bytes != int64(buf.Len()) {
				t.Fatalf("export stats %+v", st)
			}
			dst, err := eng.CreateDatabase("dst")
			if err != nil {
				t.Fatal(err)
			}
			data := buf.Bytes()
			ist, err := transfer.ImportDatabase(ctx, dst, bytes.NewReader(data), transfer.ImportOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if ist.Entities != 4 || ist.Relations != 3 || ist.Blobs != 2 || ist.Definitions != 6 {
				t.Fatalf("import stats %+v", ist)
			}
			if a, b := snapshot(t, src), snapshot(t, dst); a != b {
				t.Fatalf("contents differ:\n%s\n---\n%s", a, b)
			}
			// The imported database works: its indexes are built, its
			// constraints hold.
			if err := dst.VerifyIndexes(); err != nil {
				t.Fatal(err)
			}
			s := session.New(eng, security.AdminUser)
			defer s.Close()
			res, err := s.Exec(ctx, `use dst; find Person p | where search(p.bio, "biography") | sort p.name | show p.name, p.bill->total`, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(res[1].Rows); got != `[["Ann" 12.50d] ["Bob" null]]` {
				t.Errorf("query on the import: %s", got)
			}
			if _, err := s.Exec(ctx, `create Person { name: "Ann" }`, nil); err == nil {
				t.Error("unique name not enforced after import")
			}

			// Importing again keeps IDs, which exist now.
			if _, err := transfer.ImportDatabase(ctx, dst, bytes.NewReader(data), transfer.ImportOptions{}); err == nil ||
				!strings.Contains(err.Error(), "already exists") {
				t.Fatalf("second import with the same IDs: %v", err)
			}
			// With new IDs it copies; unique names clash, so into a fresh
			// database whose definitions differ only in being absent.
			cp, _ := eng.CreateDatabase("copy")
			transfer.ImportDatabase(ctx, cp, bytes.NewReader(data), transfer.ImportOptions{})
			if _, err := transfer.ImportDatabase(ctx, cp, bytes.NewReader(data), transfer.ImportOptions{NewIDs: true}); err == nil {
				t.Fatal("a copy with clashing unique names imported")
			}
			fresh, _ := eng.CreateDatabase("fresh")
			if _, err := transfer.ImportDatabase(ctx, fresh, bytes.NewReader(data), transfer.ImportOptions{NewIDs: true}); err != nil {
				t.Fatal(err)
			}
			res, err = s.Exec(ctx, `use fresh; find Person p | where p.name = "Ann" | show p::id, p.boss.name, p.boss.owner.name, len(p.scores), p.scores[0].name`, nil)
			if err != nil {
				t.Fatal(err)
			}
			row := res[1].Rows[0]
			if row[0].(value.ID) == must(snapshotID(src, "Ann")) || fmt.Sprint(row[1:]) != `["Acme" "Ann" 1 "Bob"]` {
				t.Errorf("copy with new IDs: %v", row)
			}
			if res, err = s.Exec(ctx, `find Person a | follow a ~knows~> Person b | show a.name, b.name`, nil); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(res[0].Rows) != `[["Ann" "Bob"]]` {
				t.Errorf("relations in the copy: %v", res[0].Rows)
			}
		})
	}
}

func snapshotID(db *engine.DB, name string) (value.ID, error) {
	var id value.ID
	err := db.View(func(tx *engine.Tx) error {
		return tx.Entities("Person", func(e value.ID) bool {
			_, v, _, _ := tx.EntityField(e, "name")
			if v.(value.Text).Go() == name {
				id = e
				return false
			}
			return true
		})
	})
	return id, err
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Definitions the target has must match; an export may only define.
func TestImportChecks(t *testing.T) {
	ctx := context.Background()
	eng := open(t)
	fill(t, eng, "src")
	src, _ := eng.Database("src")
	var buf bytes.Buffer
	if _, err := transfer.ExportDatabase(ctx, src, &buf, transfer.JSONL); err != nil {
		t.Fatal(err)
	}
	s := session.New(eng, security.AdminUser)
	defer s.Close()
	exec(t, s, `create database other; use other; define kind Company { name: text, size: int? }`, nil)
	other, _ := eng.Database("other")
	_, err := transfer.ImportDatabase(ctx, other, bytes.NewReader(buf.Bytes()), transfer.ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "kind Company differs") {
		t.Fatalf("differing definition: %v", err)
	}
	if n := len(other.Catalog().Kinds()); n != 1 {
		t.Fatalf("a failed import created %d kinds", n)
	}
	// A planted statement.
	evil := strings.Replace(buf.String(), `{"define":"define format`, `{"define":"drop kind Company"}`+"\n"+`{"define":"define format`, 1)
	if _, err := transfer.ImportDatabase(ctx, other, strings.NewReader(evil), transfer.ImportOptions{}); err == nil ||
		!strings.Contains(err.Error(), "only hold definitions") {
		t.Fatalf("planted statement: %v", err)
	}
	// Truncated.
	empty, _ := eng.CreateDatabase("empty")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	cut := strings.Join(lines[:len(lines)-1], "\n")
	if _, err := transfer.ImportDatabase(ctx, empty, strings.NewReader(cut), transfer.ImportOptions{}); err == nil ||
		!strings.Contains(err.Error(), "ends early") {
		t.Fatalf("truncated export: %v", err)
	}
}
