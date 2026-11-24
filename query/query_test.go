package query

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/value"
)

// session runs statements, each in its own transaction, keeping handles.
type session struct {
	t       testing.TB
	db      *engine.DB
	handles map[string]Handle
	params  map[string]value.Value
}

func newSession(t testing.TB) *session {
	t.Helper()
	clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	eng, err := engine.Open(engine.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	db, err := eng.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	return &session{t: t, db: db, handles: map[string]Handle{}, params: map[string]value.Value{}}
}

// exec runs one statement and returns its result or error.
func (s *session) exec(src string) (*Result, error) {
	s.t.Helper()
	st, err := lang.ParseStatement(src)
	if err != nil {
		s.t.Fatalf("%s: %v", src, err)
	}
	tx := s.db.Begin(engine.TxOptions{User: "tester"})
	res, err := Exec(&Context{Tx: tx, Handles: s.handles, Params: s.params}, st)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// run runs a script and fails on any error.
func (s *session) run(script string) {
	s.t.Helper()
	stmts, err := lang.ParseScript(script)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, st := range stmts {
		if _, err := s.exec(lang.Format(st)); err != nil {
			s.t.Fatalf("%s: %v", lang.Format(st), err)
		}
	}
}

// want runs a query and compares its rendered rows.
func (s *session) want(src, want string) {
	s.t.Helper()
	res, err := s.exec(src)
	if err != nil {
		s.t.Errorf("%s: %v", src, err)
		return
	}
	if got := render(res); got != want {
		s.t.Errorf("%s:\n got %s\nwant %s", src, got, want)
	}
}

// wantErr runs a statement and checks its error.
func (s *session) wantErr(src, code, msg string) {
	s.t.Helper()
	_, err := s.exec(src)
	e, ok := err.(*lang.Error)
	if !ok || e.Code != code || !strings.Contains(e.Msg, msg) {
		s.t.Errorf("%s: got %v, want %s containing %q", src, err, code, msg)
	}
}

// render shows rows as "a | b; c | d", with entities as Kind{fields}.
func render(r *Result) string {
	var rows []string
	for _, row := range r.Rows {
		var cells []string
		for _, c := range row {
			cells = append(cells, renderCell(c))
		}
		rows = append(rows, strings.Join(cells, " | "))
	}
	return strings.Join(rows, "; ")
}

func renderCell(c any) string {
	switch c := c.(type) {
	case *Entity:
		var fs []string
		for _, f := range c.Fields {
			v := "absent"
			if f.Value != nil {
				v = f.Value.String()
			}
			fs = append(fs, f.Name+": "+v)
		}
		return c.Kind + "{" + strings.Join(fs, ", ") + "}"
	case *Relation:
		return c.Type + c.Props.String()
	case *Path:
		var parts []string
		for i, n := range c.Nodes {
			if i > 0 {
				parts = append(parts, "~"+c.Rels[i-1].Type+"~>")
			}
			if e, ok := n.(*Entity); ok {
				name := ""
				for _, f := range e.Fields {
					if f.Name == "name" {
						name = f.Value.String()
					}
				}
				parts = append(parts, name)
			} else {
				parts = append(parts, fmt.Sprint(n))
			}
		}
		return strings.Join(parts, " ")
	case value.Value:
		return c.String()
	}
	return fmt.Sprint(c)
}

const people = `
define kind Person "A human being" {
  name   : text "Full name"
  email  : text unique
  age    : int? check age >= 0
  joined : date default today()
  tags   : set<text> default {}
  inbox  : queue.priority<text>
};
define kind Company { name: text };
define relation knows from Person to Person { since: int? };
define relation works_at from Person to Company max out 1;
create Person @ann { name: "Ann", email: "ann@x.io", age: 34, tags: {"eng"} };
create Person @bob { name: "Bob", email: "bob@x.io", age: 41 };
create Person @cy { name: "Cy", email: "cy@x.io" };
create Person @di { name: "Di", email: "di@x.io", age: 29, tags: {"eng", "ops"} };
create Company @acme { name: "Acme" };
relate @ann ~knows{since: 2019}~> @bob;
relate @bob ~knows{since: 1985}~> @cy;
relate @cy ~knows~> @di;
relate @ann ~works_at~> @acme;
relate @di ~works_at~> @acme;
`

func TestQueries(t *testing.T) {
	s := newSession(t)
	s.run(people)

	s.want(`find Person p | sort p.name | show p.name, p.age`, `"Ann" | 34; "Bob" | 41; "Cy" | null; "Di" | 29`)
	s.want(`find Person p | where p.age > 30 and "eng" in p.tags | show p.name`, `"Ann"`)
	s.want(`find Person p | where p.age > 30.5 | sort p.age desc | show p.name`, `"Bob"; "Ann"`)
	s.want(`find @ann as p | show p.joined, p.tags, p::kind, p::owner`, `d"2026-11-22" | {"eng"} | "Person" | "tester"`)
	s.want(`find Person p in [@bob, @ann] | sort p.name | show p.name`, `"Ann"; "Bob"`)
	s.want(`find Company c`, `Company{name: "Acme"}`)

	// Graph patterns.
	s.want(`find @ann as p | follow p ~knows r~> Person f | show f.name, r.since`, `"Bob" | 2019`)
	s.want(`find @cy as p | follow p <~knows~ Person q | show q.name`, `"Bob"`)
	s.want(`find @bob as p | follow p ~knows~ f | sort f.name | show f.name`, `"Ann"; "Cy"`)
	s.want(`find @ann as p | follow p ~knows*1..3~> Person f | show f.name`, `"Bob"; "Cy"; "Di"`)
	s.want(`find @ann as p | follow p ~knows*2~> f | show f.name`, `"Cy"`)
	s.want(`find @ann as p | follow p ~knows{since: 2019}~> f | show f.name`, `"Bob"`)
	s.want(`find @ann as p | follow p ~knows{since: 1}~> f | show f.name`, ``)
	s.want(`find @ann as p | follow path pth = p ~knows*~> f | where f.name = "Di" | show len(pth), pth`, `3 | "Ann" ~knows~> "Bob" ~knows~> "Cy" ~knows~> "Di"`)
	s.want(`find Person p | follow p ~works_at~> Company c | match Person q ~works_at~> c | where p != q | sort p.name | show p.name, q.name`, `"Ann" | "Di"; "Di" | "Ann"`)
	s.want(`find Person p | follow p ~*~> x | where p.name = "Ann" | sort x::kind | show x::kind`, `"Company"; "Person"`)
	s.want(`find @ann as p | follow p ~knows r~> f | show r`, `knows{"since": 2019}`)
	s.want(`find Person p | show degree(p, "knows") as d | sort d desc | limit 1`, `2`)

	// Grouping and aggregates.
	s.want(`find Person p | show count(), sum(p.age), avg(p.age), min(p.name), max(p.age)`, `4 | 104 | 34.6666666666666667d | "Ann" | 41`)
	s.want(`find Person p | where p.name = "nobody" | show count(), sum(p.age), collect(p.name)`, `0 | null | []`)
	s.want(`find Person p | group by "eng" in p.tags as eng | sort eng | show eng, count() as n, collect(p.name) as names`,
		`false | 2 | ["Bob", "Cy"]; true | 2 | ["Ann", "Di"]`)
	s.want(`find Person p | follow p ~works_at~> Company c | group by c as company | show company.name, count()`, `"Acme" | 2`)
	s.want(`find Person p | group by p.age > 30 as old | having count() > 2 | show old`, ``)
	s.want(`find Person p | let n = count() | show n`, `4`)
	s.want(`find Person p | group by p.age is null as missing | sort count() | show missing`, `true; false`)

	// Sorting, paging, distinct, columns, union.
	s.want(`find Person p | sort p.age desc, p.name | skip 1 | limit 2 | show p.name`, `"Ann"; "Di"`)
	s.want(`find Person p | follow p ~works_at~> c | distinct c | show c.name`, `"Acme"`)
	s.want(`find Person p | show p.name | sort p.name desc | limit 1`, `"Di"`)
	s.want(`find Person p | show p.name as nm | where nm starts with "B"`, `"Bob"`)
	s.want(`find Company c | show c.name | union (find Person p | where p.age > 40 | show p.name)`, `"Acme"; "Bob"`)

	// Document paths on stored values.
	s.run(`alter kind Person add cfg : doc<json>? ; update @ann set cfg = "{\"a\": {\"b\": [1, 2]}}"`)
	s.want(`find @ann as p | show p.cfg->a->b->[1]`, `2`)

	// bind saves a handle for later statements.
	s.run(`find Person p | where p.email = "cy@x.io" | bind p as @found`)
	s.want(`find @found as p | show p.name`, `"Cy"`)
	s.wantErr(`find Person p | bind p as @many`, "constraint", "bind needs exactly one row, and there are 4")
}

func TestDataStatements(t *testing.T) {
	s := newSession(t)
	s.run(people)

	s.run(`update @ann set age = 35, email = "ann@y.io"`)
	s.run(`update @ann set age += 1, tags ++= {"ops"}, tags --= {"eng"}, name ++= " Jr."`)
	s.want(`find @ann as p | show p.name, p.age, p.email, p.tags`, `"Ann Jr." | 36 | "ann@y.io" | {"ops"}`)
	s.run(`update @ann set ::labels ++= {"vip"}, ::props = {"source": "import"}`)
	s.run(`update @ann set ::props.level = 3`)
	s.want(`find @ann as p | show p::labels, p::props`, `{"vip"} | {"level": 3, "source": "import"}`)
	s.run(`update @ann unset age`)
	s.want(`find @ann as p | show p.age, exists(p.age)`, `null | false`)

	// Per-entity fields and nested elements.
	s.run(`update @ann add hobby : text = "chess" doc "Main hobby", scores : list<int> = [1, 2], attrs = {"k": "v"}`)
	s.run(`update @ann set scores[0] = 10, attrs["k"] = "w", hobby = "go"`)
	s.want(`find @ann as p | show p.hobby, p.scores, p.attrs, p.hobby::declared, p.hobby::doc, p.name::declared`,
		`"go" | [10, 2] | {"k": "w"} | false | "Main hobby" | true`)
	s.run(`update @ann drop hobby`)
	s.want(`find @ann as p | show exists(p.hobby)`, `false`)

	// Kindless entities.
	s.run(`create @n { title: "Loose note", room: "B2" doc "Building and room" }`)
	s.want(`find @n as x | show x.title, x.room::doc, x::kind`, `"Loose note" | "Building and room" | null`)

	// Create with add clauses, and the returned ID.
	res, err := s.exec(`create Person @eve { name: "Eve", email: "eve@x.io" } add hobby : text = "chess"`)
	if err != nil || len(res.Rows) != 1 || res.Affected != 1 {
		t.Fatalf("create: %v %v", res, err)
	}
	s.want(`find @eve as p | show p.hobby, p.joined, p.tags`, `"chess" | d"2026-11-22" | {}`)

	// Queues.
	s.run(`push @ann.inbox "low" priority 1; push @ann.inbox "high" priority 9; push @ann.inbox "mid" priority 5`)
	s.want(`find @ann as p | show peek(p.inbox), len(p.inbox), items(p.inbox)`, `"high" | 3 | ["high", "mid", "low"]`)
	s.want(`pop @ann.inbox`, `"high"`)
	s.want(`pop 5 from @ann.inbox`, `["mid", "low"]`)
	s.want(`pop @ann.inbox`, `null`)
	s.wantErr(`pop @ann.inbox wait 1s`, "constraint", "isn't allowed inside a transaction")

	// Relations.
	s.run(`relate @ann ~knows~> @cy as @k`)
	s.want(`find @k as r | show r::type, r.since`, `"knows" | null`)
	s.run(`unrelate @k`)
	s.want(`find @ann as p | follow p ~knows~> f | sort f.name | show f.name`, `"Bob"`)
	s.run(`unrelate @ann ~knows~> @bob`)
	s.want(`find @ann as p | follow p ~knows~> f | show f.name`, ``)

	// Data steps in pipelines.
	res, err = s.exec(`find Person p | where p.age > 30 | update p set ::labels ++= {"senior"}`)
	if err != nil || res.Affected != 1 || len(res.Rows) != 0 {
		t.Errorf("update step: %+v, %v", res, err)
	}
	s.want(`find Person p | where p::labels has "senior" | show p.name`, `"Bob"`)
	s.run(`find Person p | update p set age = coalesce(age, 0) + 1`)
	s.want(`find Person p | sort p.name | show p.age`, `1; 42; 1; 30; 1`)
	s.run(`find Person p | where p.name = "Di" | push p.inbox p.name ++ "!" priority 1`)
	s.want(`find Person w | where len(w.inbox) > 0 | pop w.inbox as job | show w.name, job`, `"Di" | "Di!"`)
	s.run(`find Person p | follow p ~knows r~> f | where r.since < 1990 | unrelate r`)
	s.want(`find @bob as p | follow p ~knows~> f | show f.name`, ``)
	res, _ = s.exec(`find Person p | where p.name = "Eve" | delete p`)
	if res.Affected != 1 {
		t.Errorf("delete step affected %d", res.Affected)
	}
	s.want(`find Person p | show count()`, `4`)
}

func TestDefinitionsAndErrors(t *testing.T) {
	s := newSession(t)
	s.run(people)

	// Constraint and check errors carry the statement's position.
	s.wantErr(`create Person { name: "X", email: "ann@x.io" }`, "constraint", "unique")
	s.wantErr(`update @ann set age = -1`, "constraint", "check failed for age: age >= 0")
	s.wantErr(`relate @ann ~works_at~> @acme`, "constraint", "max out")

	// Checker errors come first, with suggestions.
	s.wantErr(`find Persn p`, "not_found", "did you mean Person?")
	s.wantErr(`update @ann set agee = 1`, "unknown_field", "agee")

	// alter kind and defaults.
	s.run(`alter kind Person add level : int default 1`)
	s.want(`find @bob as p | show p.level`, `1`)
	s.run(`alter kind Person alter level : int check level < 5 default 1`)
	s.wantErr(`update @bob set level = 7`, "constraint", "check failed for level")
	s.run(`alter kind Person drop level; alter kind Company rename to Firm`)
	s.want(`find Firm f | show f.name`, `"Acme"`)
	s.run(`alter kind Firm set closed`)
	s.wantErr(`update @acme add x = 1`, "constraint", "closed")

	// Relations: defaults, options and renames.
	s.run(`alter relation knows add weight : float? check weight is null or weight >= 0.0`)
	s.wantErr(`relate @cy ~knows{weight: -1.0}~> @ann`, "constraint", "check failed for weight")
	s.run(`alter relation knows set max out 5; alter relation knows rename to friend_of`)
	s.want(`find @ann as p | follow p ~friend_of~> f | show f.name`, `"Bob"`)

	// Formats, including a table format with column defaults.
	s.run(`define format staff : csv header { name: text, age: int?, status: text default "active" }`)
	s.run(`alter kind Person add team : doc<staff>?`)
	s.run(`update @ann set team = "name,status\nZed,\nYo,away\n"`)
	s.want(`find @ann as p | show p.team->[0]->status, p.team->[1]->status, len(parse(p.team))`, `"active" | "away" | 2`)
	s.wantErr(`update @ann set team = "name,age\nZed,old\n"`, "type", `"old" isn't a int`)
	s.run(`define index person_age on Person(age); drop index person_age`)
	s.run(`define kind Tmp { a: int }; drop kind Tmp`)
	s.wantErr(`drop kind Person`, "constraint", "")

	// Things later phases add.
	s.wantErr(`find Person p as of t"2026-01-01T00:00:00Z"`, "unsupported", "shows fields")
	s.wantErr(`create user ramon password "x"`, "unsupported", "security")

	// search(): a constant query is checked before running; others when
	// they are used.
	s.want(`find Person p | where search(p.name, "ANN OR bo*") | sort p.name | show p.name`, `"Ann"; "Bob"`)
	s.wantErr(`find Person p | show search(p.name, "\"open")`, "syntax", "phrase is missing")
	s.wantErr(`find Person p | let q = "-x" | show search(p.name, q)`, "syntax", "no words to find")
}

func TestErrorPositions(t *testing.T) {
	s := newSession(t)
	s.run(people)
	_, err := s.exec("find Person p\n  | where p.age / 0 > 1")
	e, ok := err.(*lang.Error)
	if !ok || e.Code != "type" || e.Pos.Line != 2 {
		t.Errorf("division by zero: %#v", err)
	}
	_, err = s.exec("create Person {\n name: \"X\", email: \"ann@x.io\" }")
	if e, ok := err.(*lang.Error); !ok || e.Pos.Line != 1 || e.Code != "constraint" {
		t.Errorf("unique: %#v", err)
	}
}

func TestMaxRows(t *testing.T) {
	s := newSession(t)
	s.run(people)
	st, _ := lang.ParseStatement(`find Person p`)
	tx := s.db.Begin(engine.TxOptions{ReadOnly: true})
	defer tx.Rollback()
	_, err := Exec(&Context{Tx: tx, Handles: s.handles, Settings: Settings{MaxRows: 2}}, st)
	if e, ok := err.(*lang.Error); !ok || e.Code != "limit" {
		t.Errorf("max_rows: %v", err)
	}
}

func TestFieldNodesAndSources(t *testing.T) {
	s := newSession(t)
	s.run(`
define kind City { name: text };
define kind Place { label: text, address: text, owner: ref<Place>? };
define relation located_in from field to City;
create City @spr { name: "Springfield" };
create Place @home { label: "home", address: "742 Evergreen" };
create Place @shop { label: "shop", address: "Main St", owner: @home };
create @note { text: "kindless" };
relate @home.address ~located_in~> @spr;
`)
	s.want(`find Place p | follow p.address ~located_in~> City c | show p.label, c.name`, `"home" | "Springfield"`)
	s.want(`find @spr as c | follow c <~located_in~ x.address | show x.label`, `"home"`)
	s.want(`find @shop as p | show p.owner.label, p.owner.owner`, `"home" | null`)
	s.want(`find Place a, City c | sort a.label | show a.label, c.name`, `"home" | "Springfield"; "shop" | "Springfield"`)
	s.want(`find p | where p::kind is null | show p.text`, `"kindless"`)
	s.want(`find p | show count()`, `4`)
	s.wantErr(`relate @home ~located_in~> @spr`, "constraint", "")
}

// FuzzExec runs random statements against a seeded database, rolling each
// back: execution must not panic, and errors must be Rhizome errors with
// a code.
func FuzzExec(f *testing.F) {
	for _, src := range []string{
		`find Person p | follow p ~knows*1..3~> Person f | group by f as g | show g.name, count()`,
		`find Person p | update p set tags ++= {"x"}, age += 1`,
		`find Person w | pop w.inbox as job | show job`,
		`find Person p | follow path pth = p ~knows*~> f | show pth, nodes(pth)`,
		`update @ann add a : list<int> = [1] set a[0] = 2`,
		`find Person p | match p ~knows~> q <~knows~ r | show p, q, r`,
		`find Person p | show p.name | union (find Company c | show c.name) | sort p.name`,
		`define kind K { a: int default 1 check a > 0 }`,
	} {
		f.Add(src)
	}
	s := newSession(&testing.T{})
	s.run(people)
	f.Fuzz(func(t *testing.T, src string) {
		st, err := lang.ParseStatement(src)
		if err != nil {
			return
		}
		tx := s.db.Begin(engine.TxOptions{})
		defer tx.Rollback()
		handles := map[string]Handle{}
		for k, v := range s.handles {
			handles[k] = v
		}
		_, err = Exec(&Context{Tx: tx, Handles: handles, Settings: Settings{MaxDepth: 4}}, st)
		if err != nil {
			if e, ok := err.(*lang.Error); !ok || e.Code == "" || e.Code == "internal" {
				t.Fatalf("%s: bad error %#v", src, err)
			}
		}
	})
}

// A statement looks up only the handles it names, so its cost doesn't
// grow with the session's handles (a script that binds one per entity
// would otherwise slow down quadratically).
func TestManyHandles(t *testing.T) {
	s := newSession(t)
	s.run(`define kind K { a: int }; create K @k { a: 1 };`)
	timeRun := func() time.Duration {
		began := time.Now()
		for range 50 {
			if _, err := s.exec(`find @k as k | show k.a`); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(began)
	}
	few := timeRun()
	k := s.handles["k"]
	for i := range 200000 {
		s.handles[fmt.Sprint("h", i)] = k
	}
	many := timeRun()
	if many > 5*few+50*time.Millisecond {
		t.Errorf("50 statements took %v with 200000 handles, %v with one", many, few)
	}
	if _, err := s.exec(`find @kk as k | show k.a`); err == nil || !strings.Contains(err.Error(), "did you mean @k") {
		t.Errorf("unknown handle: %v", err)
	}
}
