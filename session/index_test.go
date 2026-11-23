package session

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/fulltext"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

const indexKind = `define kind P { name: text, age: int?, city: text?, cfg: doc<json>? }`

func mustExec(t *testing.T, eng *engine.Engine, db, src string) {
	t.Helper()
	if err := execAs(t, eng, security.AdminUser, db, src); err != nil {
		t.Fatalf("%v (in %.60q)", err, src)
	}
}

func verify(t *testing.T, eng *engine.Engine, what string) {
	t.Helper()
	db, err := eng.Database("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyIndexes(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// entries counts an index's entries.
func entries(t *testing.T, eng *engine.Engine, name string) int {
	t.Helper()
	db, _ := eng.Database("test")
	n := 0
	err := db.View(func(tx *engine.Tx) error {
		ix, ok := tx.Catalog().Index(name)
		if !ok {
			return fmt.Errorf("no index %s", name)
		}
		return tx.ScanIndex(ix, nil, nil, func(value.ID, []byte) bool { n++; return true })
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIndexMaintenance checks that every write keeps single-field,
// multi-field and document-path indexes in step with the data.
func TestIndexMaintenance(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint("disk=", disk), func(t *testing.T) {
			s := newScriptSession(t, disk)
			eng := s.eng
			mustExec(t, eng, "test", indexKind+`;
				create P @a { name: "a", age: 30, city: "Oslo", cfg: "{\"server\":{\"port\":80}}" };
				create P @b { name: "b", age: 30 };
				create P @c { name: "c", city: "Oslo", cfg: "{\"server\":{}}" };
				define index p_age on P(age);
				define index p_city_age on P(city, age);
				define index p_port on P(cfg->server->port);`)
			verify(t, eng, "after define")
			// Absent and null values aren't indexed; nor is a path that
			// finds nothing.
			if a, ca, port := entries(t, eng, "p_age"), entries(t, eng, "p_city_age"), entries(t, eng, "p_port"); a != 2 || ca != 1 || port != 1 {
				t.Errorf("entries: age %d, city+age %d, port %d", a, ca, port)
			}
			steps := []string{
				`create P { name: "d", age: 41, city: "Rome", cfg: "{\"server\":{\"port\":443}}" }`,
				`find P p | where p.name = "a" | update p set age = 31`,
				`find P p | where p.name = "b" | update p unset age`,
				`find P p | where p.name = "c" | update p set age = 5, cfg = "{\"server\":{\"port\":8080}}"`,
				`find P p | where p.name = "a" | update p set age = null`,
				`find P p | where p.name = "d" | delete p`,
			}
			for _, st := range steps {
				mustExec(t, eng, "test", st)
				verify(t, eng, st)
			}
			if a, port := entries(t, eng, "p_age"), entries(t, eng, "p_port"); a != 1 || port != 2 {
				t.Errorf("entries at the end: age %d, port %d", a, port)
			}
			mustExec(t, eng, "test", "drop index p_city_age")
			verify(t, eng, "after drop")
		})
	}
}

// bigKind creates n entities of P, enough for background builds.
func bigKind(t *testing.T, eng *engine.Engine, n int) {
	t.Helper()
	mustExec(t, eng, "test", indexKind)
	for start := 0; start < n; start += 500 {
		var b strings.Builder
		for i := start; i < min(start+500, n); i++ {
			fmt.Fprintf(&b, "create P { name: \"n%05d\", age: %d };\n", i, i%97)
		}
		mustExec(t, eng, "test", b.String())
	}
}

// TestBackgroundIndexBuild builds an index on a large kind in the
// background while writers change it, then checks it.
func TestBackgroundIndexBuild(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint("disk=", disk), func(t *testing.T) { backgroundBuild(t, disk) })
	}
}

func backgroundBuild(t *testing.T, disk bool) {
	s := newScriptSession(t, disk)
	eng := s.eng
	bigKind(t, eng, 3000)
	db, _ := eng.Database("test")
	var ids []value.ID
	db.View(func(tx *engine.Tx) error {
		return tx.Entities("P", func(id value.ID) bool { ids = append(ids, id); return true })
	})

	// Inside the defining transaction the index is recorded as building,
	// not built at once.
	got := rows(t, eng, security.AdminUser, "test", "begin; define index p_age on P(age); show indexes")
	if !strings.Contains(got, "building: 0 entities indexed") {
		t.Fatalf("show indexes in the defining transaction: %s", got)
	}
	execAs(t, eng, security.AdminUser, "test", "rollback")
	mustExec(t, eng, "test", "define index p_age on P(age)")

	var stop atomic.Bool
	var wg sync.WaitGroup
	var writes atomic.Int64
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 9))
			for !stop.Load() {
				id := ids[rng.IntN(len(ids))]
				err := db.Update(func(tx *engine.Tx) error {
					switch rng.IntN(10) {
					case 0:
						return tx.Unset(id, "age")
					case 1:
						_, err := tx.Create("P", engine.Fields{"name": value.MustText("new"), "age": value.Int(int64(rng.IntN(50)))})
						return err
					default:
						return tx.Set(id, engine.Fields{"age": value.Int(int64(rng.IntN(1000)))})
					}
				})
				if err == nil {
					writes.Add(1)
				} else if !engine.IsConflict(err) && engine.CodeOf(err) != engine.CodeNotFound {
					t.Error(err)
					return
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := db.WaitIndexes(ctx)
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d writes during the build", writes.Load())
	verify(t, eng, "after a build with concurrent writers")
	if got := rows(t, eng, security.AdminUser, "test", "show indexes"); !strings.Contains(got, "ready") {
		t.Errorf("show indexes: %s", got)
	}
}

// TestIndexBuildResumes stops a build by closing the engine, and by a
// crash; reopening finishes it.
func TestIndexBuildResumes(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint("crash=", crash), func(t *testing.T) {
			fs := vfs.NewMem()
			o := diskOptions(fs)
			eng := openDisk(t, o)
			mustExec(t, eng, "", "create database test")
			bigKind(t, eng, 2500)
			mustExec(t, eng, "test", "define index p_age on P(age)")
			if crash {
				crashed := fs.Crash(rand.New(rand.NewPCG(3, 3)))
				eng.Close()
				o.FS = crashed
			} else {
				eng.Close()
			}
			eng = openDisk(t, o)
			defer eng.Close()
			db, _ := eng.Database("test")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := db.WaitIndexes(ctx); err != nil {
				t.Fatal(err)
			}
			verify(t, eng, "after resuming")
			if n := entries(t, eng, "p_age"); n != 2500 {
				t.Errorf("%d entries, want 2500", n)
			}
		})
	}
}

// TestDropIndexDuringBuild drops an index while it is being built.
func TestDropIndexDuringBuild(t *testing.T) {
	s := newScriptSession(t, false)
	eng := s.eng
	bigKind(t, eng, 2500)
	mustExec(t, eng, "test", "define index p_age on P(age); define index p_name on P(name)")
	mustExec(t, eng, "test", "drop index p_age")
	db, _ := eng.Database("test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.WaitIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	verify(t, eng, "after dropping during a build") // no entries of p_age left
	if n := entries(t, eng, "p_name"); n != 2500 {
		t.Errorf("p_name has %d entries", n)
	}
}

var textWords = strings.Fields("alpha beta gamma delta Café cafe CAFE résumé resume run running runner 東京 京都 e-mail mail don't 2026")

func randomText(rng *rand.Rand) string {
	var ws []string
	for range 2 + rng.IntN(10) {
		ws = append(ws, textWords[rng.IntN(len(textWords))])
	}
	return strings.Join(ws, []string{" ", ", ", ". ", " - "}[rng.IntN(4)])
}

func randomQuery(rng *rand.Rand) string {
	word := func() string {
		w := textWords[rng.IntN(len(textWords))]
		switch rng.IntN(6) {
		case 0:
			if r := []rune(w); len(r) > 2 && !strings.ContainsAny(w, "-'") {
				return string(r[:1+rng.IntN(len(r)-1)]) + "*"
			}
		case 1:
			return `"` + w + " " + textWords[rng.IntN(len(textWords))] + `"`
		}
		return w
	}
	var cs []string
	for i := range 1 + rng.IntN(3) {
		c := word()
		if rng.IntN(3) == 0 {
			c += " OR " + word()
		} else if i > 0 && rng.IntN(4) == 0 {
			c = "-" + c
		}
		cs = append(cs, c)
	}
	return strings.Join(cs, " ")
}

// TestTextIndexMatchesScan checks, on random documents and queries, that
// answering from a text index gives what matching every document gives,
// through updates, unsets and deletes, and for an index built in the
// background.
func TestTextIndexMatchesScan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		disk  bool
		n     int
		after bool // define the index after the data: a background build
	}{{"memory", false, 300, false}, {"disk", true, 300, false}, {"background", true, 1500, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(4, 4))
			s := newScriptSession(t, tc.disk)
			eng := s.eng
			mustExec(t, eng, "test", `define kind Doc { title: text, body: text? }`)
			if !tc.after {
				mustExec(t, eng, "test", `define text index doc_title on Doc(title); define text index doc_body on Doc(body)`)
			}
			db, _ := eng.Database("test")
			var ids []value.ID
			err := db.Update(func(tx *engine.Tx) error {
				for range tc.n {
					f := engine.Fields{"title": value.MustText(randomText(rng))}
					if rng.IntN(5) > 0 {
						f["body"] = value.MustText(randomText(rng))
					}
					id, err := tx.Create("Doc", f)
					if err != nil {
						return err
					}
					ids = append(ids, id)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *engine.Tx) error {
				for i, id := range ids {
					switch {
					case i%7 == 0:
						if err := tx.Set(id, engine.Fields{"body": value.MustText(randomText(rng))}); err != nil {
							return err
						}
					case i%11 == 0:
						if err := tx.Unset(id, "body"); err != nil {
							return err
						}
					case i%13 == 0:
						if err := tx.Delete(id); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.after {
				mustExec(t, eng, "test", `define text index doc_title on Doc(title); define text index doc_body on Doc(body)`)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := db.WaitIndexes(ctx); err != nil {
					t.Fatal(err)
				}
			}
			verify(t, eng, "text index")

			matches := 0
			err = db.View(func(tx *engine.Tx) error {
				texts := map[string]map[value.ID]string{"title": {}, "body": {}}
				tx.Entities("Doc", func(id value.ID) bool {
					e, _ := tx.Get(id)
					for f := range texts {
						if v, ok := e.Value(f).(value.Text); ok {
							texts[f][id] = v.Go()
						}
					}
					return true
				})
				for range 400 {
					src := randomQuery(rng)
					q, err := fulltext.Parse(src)
					if err != nil {
						continue // e.g. only exclusions
					}
					for field, byID := range texts {
						ix, _ := tx.Catalog().Index("doc_" + field)
						got, err := tx.TextSearch(ix, field, q)
						if err != nil {
							return err
						}
						var want []value.ID
						for id, text := range byID {
							if q.Match(text) {
								want = append(want, id)
							}
						}
						slices.SortFunc(want, func(a, b value.ID) int { return value.Compare(a, b) })
						if !slices.Equal(got, want) {
							return fmt.Errorf("%s in %s: index found %d, matching finds %d", src, field, len(got), len(want))
						}
						matches += len(got)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d matches over 800 searches", matches)
			// And through Rhizome.
			want := rows(t, eng, security.AdminUser, "test", `find Doc d | where search(d.title, "cafe -mail") | show count()`)
			n := 0
			db.View(func(tx *engine.Tx) error {
				ix, _ := tx.Catalog().Index("doc_title")
				q, _ := fulltext.Parse("cafe -mail")
				got, err := tx.TextSearch(ix, "title", q)
				n = len(got)
				return err
			})
			if want != fmt.Sprint(n) {
				t.Errorf("search() counts %s, the index %d", want, n)
			}
		})
	}
}
