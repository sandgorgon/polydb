package lang

import (
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/catalog"
	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/value"
)

func testEnv(t *testing.T) *Env {
	t.Helper()
	ed := catalog.New().Edit(value.NewIDGenerator(0))
	must := func(_ value.ID, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ty := value.MustParseType
	must(ed.DefineFormat(model.Format{Name: "invoice", Base: "json", Fields: []model.FieldDef{
		{Name: "number", Type: ty("text")},
		{Name: "total", Type: ty("decimal(12,2)")},
		{Name: "lines", Type: ty("list<record{sku: text, qty: int}>")},
	}}))
	four := 4
	must(ed.DefineFormat(model.Format{Name: "staff", Base: "csv", Header: true, Fields: []model.FieldDef{
		{Name: "name", Type: ty("text")},
		{Name: "email", Type: ty("text"), Column: &four},
		{Name: "age", Type: ty("int?")},
		{Name: "status", Type: ty("text"), Default: `"active"`, DefaultValue: value.MustText("active")},
	}}))
	must(ed.DefineFormat(model.Format{Name: "contacts", Base: "csv", Header: true}))
	must(ed.DefineKind(model.Kind{Name: "Company", Fields: []model.FieldDef{{Name: "name", Type: ty("text")}}}))
	must(ed.DefineKind(model.Kind{Name: "Person", Fields: []model.FieldDef{
		{Name: "name", Type: ty("text")},
		{Name: "email", Type: ty("text"), Unique: true},
		{Name: "age", Type: ty("int?")},
		{Name: "joined", Type: ty("date"), Default: "today()"},
		{Name: "tags", Type: ty("set<text>"), Default: "{}"},
		{Name: "manager", Type: ty("ref<Person>?")},
		{Name: "inbox", Type: ty("queue.priority<text>")},
		{Name: "jobs", Type: ty("queue.fifo<ref>")},
		{Name: "salary", Type: ty("decimal(12,2)?")},
		{Name: "score", Type: ty("float?")},
		{Name: "config", Type: ty("doc<json>?")},
		{Name: "inv", Type: ty("doc<invoice>?")},
		{Name: "page", Type: ty("doc<xml>?")},
		{Name: "addr", Type: ty("record{city: text, zip: text?}?")},
		{Name: "attrs", Type: ty("map<text, int>?")},
		{Name: "scores", Type: ty("list<int>?")},
		{Name: "data", Type: ty("bytes?")},
		{Name: "started", Type: ty("time?")},
		{Name: "team", Type: ty("doc<staff>?")},
		{Name: "book", Type: ty("doc<contacts>?")},
		{Name: "rows", Type: ty("doc<csv>?")},
	}}))
	must(ed.DefineKind(model.Kind{Name: "Invoice", Closed: true, Fields: []model.FieldDef{
		{Name: "number", Type: ty("text")},
		{Name: "total", Type: ty("decimal(12,2)")},
	}}))
	cat := ed.Catalog()
	person, _ := cat.Kind("Person")
	company, _ := cat.Kind("Company")
	must(ed.DefineRelationType(model.RelationType{Name: "knows", Defined: true,
		From:  model.Endpoint{Mode: model.EndpointKind, Kind: person.ID},
		To:    model.Endpoint{Mode: model.EndpointKind, Kind: person.ID},
		Props: []model.FieldDef{{Name: "since", Type: ty("int?")}}}))
	must(ed.DefineRelationType(model.RelationType{Name: "works_at", Defined: true,
		From: model.Endpoint{Mode: model.EndpointKind, Kind: person.ID},
		To:   model.Endpoint{Mode: model.EndpointKind, Kind: company.ID}}))
	must(ed.DefineRelationType(model.RelationType{Name: "located_in", Defined: true,
		From: model.Endpoint{Mode: model.EndpointField}}))
	must(ed.EnsureRelationType("likes"))
	cat = ed.Catalog()
	person, _ = cat.Kind("Person")
	knows, _ := cat.RelationType("knows")
	id, _ := value.ParseID("01JD8X3K7M0000Q2W5ZB9N4T6R")
	return &Env{
		Catalog: cat,
		Handles: map[string]Static{
			"alice": {Form: FormEntity, Kind: person},
			"bob":   {Form: FormEntity, Kind: person},
			"n":     {Form: FormEntity},
			"k":     {Form: FormRelation, Rel: knows},
		},
		Params: map[string]value.Value{"min": value.Int(3), "id": id, "name": value.MustText("Ann")},
	}
}

func checkSrc(t *testing.T, env *Env, src string) (*Info, Stmt, error) {
	t.Helper()
	s, err := ParseStatement(src)
	if err != nil {
		t.Fatalf("%q: parse: %v", src, err)
	}
	info, err := Check(env, s)
	return info, s, err
}

func TestCheckValid(t *testing.T) {
	env := testEnv(t)
	for _, src := range []string{
		// definitions
		`define kind Task { title: text, due: date? check due > d"2000-01-01", done: bool default false, parent: ref<Task>? }`,
		`define kind T2 { a: int check a >= 0 and a < 10, b: int? check b is null or b > a }`,
		`define kind T3 history keep 365h { t: time default now(), d: duration default 5m, s: set<text> default {} }`,
		`define kind T4 { at: time default "2026-01-01T00:00:00Z", cfg: doc<json>? default "{}" }`,
		`define relation manages from Person to Person max out 3 { since: date?, weight: float? check weight >= 0.0 }`,
		`define relation tagged from any to field`,
		`define format order : json { id: text, items: list<text> }`,
		`define format people : csv header "Export" { name: text, email: text at 4, age: int?, status: text default "active" }`,
		`define format pts : tsv { x: int, y: float?, day: date default "2026-01-01" }`,
		`define format raw : csv`,
		`find Person p | where p.team->[0]->age > 30 and p.team->[0]->status = "active"`,
		`find Person p | show p.book->[0]->email, p.rows->[2]->[0], len(parse(p.team))`,
		`define index cfg_port on Person(config->server->port)`,
		`define index inv_total on Person(inv->total)`,
		`define text index person_name on Person(name)`,
		`alter kind Person add nickname : text? "Informal name"`,
		`alter kind Person alter age : int? check age between 0 and 150`,
		`alter kind Person drop salary`,
		`alter relation knows add weight : float?`,
		`describe kind Person`, `describe relation knows`, `describe format invoice`, `describe format json`,
		`describe @alice`, `show stats kind Person`, `set timeout = 30s`, `set max_rows = 100`,
		// create
		`create Person @carol { name: "Carol", email: "c@x.io", age: 34, tags: {"eng"}, inbox: [5 => "hi"] }`,
		`create Person { name: "Bob", email: "b@x.io", joined: "2026-01-02", manager: @alice, salary: 10 }`,
		`create Person { name: "B", email: "b", started: "2026-01-02T03:04:05Z", config: "{\"a\": 1}", addr: {city: "X"} }`,
		`create Person { name: "B", email: "b", manager: #01JD8X3K7M0000Q2W5ZB9N4T6R, inv: "{\"number\":\"1\",\"total\":2,\"lines\":[]}" }`,
		`create Person @c { name: "C", email: "c" } add hobby : text = "chess" doc "Main hobby", n = 1`,
		`create @n2 { title: "Loose", body: "no kind" doc "Body text" }`,
		`create @m { x: 1 } add y : int = 2 check y > x`,
		`create Invoice { number: "1", total: 9.99d }`,
		// update
		`update @alice set age = 35, email = "alice@x.io"`,
		`update @alice unset age`,
		`update @alice set age += 1, salary += 1, salary *= 2`,
		`update @alice set tags ++= {"ops"}, tags --= {"eng"}, name ++= " Jr."`,
		`update @alice set ::labels ++= {"vip"}, ::props.source = "import"`,
		`update @alice set age = age + 1`,
		`update @alice add hobby : text = "chess" set hobby = "go"`,
		`update @n set title = "Renamed", scores[0] = 10`,
		`update @alice set attrs["k"] = 3, addr.city = "Y", scores[0] = 1`,
		`update @alice set attrs --= {"k"}`,
		`update @alice add x = 1 drop x`,
		`update #01JD8X3K7M0000Q2W5ZB9N4T6R set age = 36`,
		`update $id set age = $min`,
		`delete @alice`, `delete $id`,
		// relations and queues
		`relate @alice ~knows{since: 2019}~> @bob`,
		`relate @alice ~knows~> @bob as @k2`,
		`relate @bob <~knows~ @alice`,
		`relate @alice.addr ~located_in~> @n`,
		`relate @n ~whatever~> @alice`,
		`unrelate @alice ~knows~> @bob`, `unrelate @k`,
		`push @alice.inbox "hello" priority 5`, `push @alice.jobs @bob`,
		`pop @alice.inbox`, `pop 10 from @alice.inbox`, `pop @alice.jobs wait 30s`,
		// queries
		`find Person p`, `find p`, `find Person p, Company c`, `find @alice as p`,
		`find #01JD8X3K7M0000Q2W5ZB9N4T6R as x`, `find Person p in [@alice, @bob]`,
		`find Person p | where p.age > 30 and "eng" in p.tags`,
		`find Person p | where p.age > 30.5`,
		`find Person p | where p.salary > 100 and p.salary < 1000.00d`,
		`find Person p | where p.joined > d"2026-01-01" and p::created > now() - 24h`,
		`find Person p | where p.email matches ".*@x\\.io$" and p.name starts with "A"`,
		`find Person p | where p.name contains "nn" and p.tags has "eng" and p.tags contains "x"`,
		`find Person p | where p.manager = @alice or p.manager = #01JD8X3K7M0000Q2W5ZB9N4T6R`,
		`find Person p | where p.manager.name = "Ann"`,
		`find Person p | where p is Person and p.age is int and p.age is not null`,
		`find Person p | where p.age not between 1 and 5`,
		`find Person p | where p.config->server->port = 80`,
		`find Person p | where p.config->"key with space"->[0] = 1`,
		`find Person p | where p.inv->total > 10 and p.inv->lines->[0]->qty = 2`,
		`find Person p | where p.page->server->@port = "80" and p.page->@"x" = "1"`,
		`find Person p | where p.name::doc = "Full name" and p.hobby::declared`,
		`find Person p | where exists(p.age) and coalesce(p.age, 0) > 1`,
		`find Person p | follow p ~knows~> Person f | where f.name != p.name`,
		`find Person p | follow p ~knows r~> Person f | where r.since < 1990 | show p.name, f.name as friend, r::id`,
		`find Person p | follow p ~knows*1..3~> Person fof`,
		`find Person p | follow path pth = p ~knows*~> f | show len(pth), nodes(pth)`,
		`find Person p | follow p ~works_at~> Company c | follow p ~knows*2~> Person fof | match fof ~works_at~> c`,
		`find Person p | follow p ~knows|likes~> f | follow f ~*~> x`,
		`find Person p | follow p.addr ~located_in~> c`,
		`find Person p | follow p <~knows~ Person q | follow q ~knows{since: 2019}~> z`,
		`find Person p | let full = p.name ++ " " ++ p.email | sort full desc, p.age | skip 20 | limit $min`,
		`find Person p | group by p.age as age | having count() > 5 | show age, count() as n, collect(p.name) as names`,
		`find Person p | group by p.age as age | sort count() desc`,
		`find Person p | show count(), avg(p.age), sum(p.salary), min(p.joined), max(p.name)`,
		`find Person p | let n = count() | show n`,
		`find Person p | distinct p.age`, `find Person p | distinct`,
		`find Person p | show p.name | sort p.name | limit 3`,
		`find Person p | show p.name as nm | where nm != "x"`,
		`find Person p | where p.name = "x" | bind p as @x`,
		`find Person p | show p.name | union (find Company c | show c.name)`,
		`find Person p | where p.age > 120 | update p set ::labels ++= {"check_age"}`,
		`find Person p | update p set age = age + 1`,
		`find Person p | where p::labels has "spam" | delete p`,
		`find Person p | follow p ~knows r~> f | where r.since < 1990 | unrelate r`,
		`find Person w | pop w.jobs as job | show w.name, job`,
		`find Person p | push p.inbox p.name priority 1`,
		`find Person p | show if(p.age > 3, "old", "young"), cast(p.age as float) * 1.5, cast("3" as int)`,
		`find Person p | show typeof(p.age), lower(p.name), substr(p.name, 0, 2), len(p.tags), keys(p.attrs)`,
		`find Person p | show round(p.salary, 1), abs(p.age), pow(2, 10), sqrt(2.0), year(p.joined), add_months(p.joined, 1)`,
		`find Person p | show peek(p.inbox), items(p.jobs), degree(p, "knows", "out"), id(p), parse(p.inv).total`,
		`find Person p | show p.started + 1h, p.started - p.started, 2 * 1h, {"a", "b"} ++ p.tags, [1] ++ p.scores`,
		`find Person p | show p.name => p.age, {a: 1, b: p.age}, [p, @alice], valid("{}", "json"), max(1, p.age)`,
		`find Person p | show truncate(now(), 1h), today(), split(p.name, " "), join(split(p.name, " "), "-")`,
		`explain find Person p | where p.age > 30`, `history @alice.salary`,
		`begin`, `commit`, `rollback`, `use x`, `show databases`,
		`find Person p | show p.data ++ x"00", -p.age, not p.age > 3, inner_text(p.page->x)`,
	} {
		if _, _, err := checkSrc(t, env, src); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}

func TestCheckErrors(t *testing.T) {
	env := testEnv(t)
	cases := []struct{ src, code, want string }{
		// names
		{`find Persn p`, "not_found", "no kind Persn (did you mean Person?)"},
		{`find Person`, "syntax", "Person is a kind; name a variable"},
		{`find Person p | where q.age > 1`, "not_found", "no variable q"},
		{`find Person p | where p.age > $max`, "not_found", "parameter $max isn't set"},
		{`find @alise as p`, "not_found", "no handle @alise (did you mean @alice"},
		{`find Person p | show lowr(p.name)`, "not_found", "no function lowr (did you mean lower?)"},
		{`find Invoice i | where i.numbr = "1"`, "unknown_field", "kind Invoice has no field numbr (did you mean number?)"},
		{`find p | where p is Persn`, "not_found", "no kind Persn"},
		{`set timout = 3s`, "not_found", "no setting timout (did you mean timeout?)"},
		{`describe kind Persn`, "not_found", "no kind Persn"},
		{`describe relation knws`, "not_found", "no relation knws (did you mean knows?)"},
		// operators and types
		{`find Person p | where p.age + p.score > 1`, "type", "write cast(p.age as float)"},
		{`find Person p | where p.name + "x" = "y"`, "type", "use ++ to join"},
		{`find Person p | show 1 ++ 2`, "type", "use + to add numbers"},
		{`find Person p | where p.name > 3`, "type", "can't compare text with int"},
		{`find Person p | where p.joined > "2026-01-01"`, "type", `write a date as d"2026-01-31"`},
		{`find Person p | where p.age`, "type", "where needs a condition"},
		{`find Person p | where p.age = 1 and p.name`, "type", "and needs a condition"},
		{`find Person p | where 3 in p.tags`, "type", "the collection holds text"},
		{`find Person p | where p.email matches "("`, "type", "invalid regular expression"},
		{`find Person p | where p.name starts with 1`, "type", "starts with works on text"},
		{`find Person p | show p.tags[0]`, "type", "[ ] reads from lists, arrays and maps"},
		{`find Person p | show p.scores["a"]`, "type", "list positions are ints"},
		{`find Person p | show p.addr.cty`, "unknown_field", "the record has no field cty (did you mean city?)"},
		{`find Person p | show p.age.x`, "type", ".x reads fields"},
		{`find Person p | show p::creatd`, "not_found", "no metadata ::creatd (did you mean created?)"},
		{`find Person p | show p.name::doc2`, "not_found", "fields have no metadata ::doc2"},
		{`find Person p | show cast(p.joined as int)`, "type", "can't cast date to int"},
		{`find Person p | show sqrt(p.age)`, "type", "sqrt(cast(p.age as float))"},
		{`find Person p | show lower(p.age)`, "type", "lower needs text as argument 1, not int?"},
		{`find Person p | show substr(p.name)`, "type", "substr takes 2 to 3 arguments, not 1"},
		{`find Person p | show degree(p, "knows", "sideways")`, "type", "degree's direction"},
		{`find Person p | show search(p.name, "-x")`, "syntax", "no words to find"},
		{`find Person p | show search(p.age, "x")`, "type", "search needs text"},
		{`find Person p | show nodes(p)`, "type", "nodes needs a path"},
		{`find Person p | show valid("x", "yaml")`, "not_found", "no format yaml"},
		// docs
		{`find Person p | where p.inv->totl > 1`, "unknown_field", "format invoice has no field totl here (did you mean total?)"},
		{`find Person p | where p.inv->total = "x"`, "type", "can't compare decimal(12,2)? with text"},
		{`find Person p | where p.config->@x = 1`, "type", "@x is an XML attribute step"},
		{`find Person p | where p.page->["a"] = 1`, "type", "xml position steps take an int"},
		{`find Person p | where p.name->x = 1`, "type", "-> reads inside documents"},
		{`define index bad on Person(inv->nope)`, "unknown_field", "format invoice has no field nope"},
		{`define index bad on Person(nope)`, "unknown_field", "kind Person has no field nope"},
		{`define text index bad on Person(age)`, "type", "a text index needs text fields"},
		// aggregates and grouping
		{`find Person p | where count() > 1`, "syntax", "count() is an aggregate"},
		{`find Person p | show p.name, count()`, "syntax", "p isn't a group name"},
		{`find Person p | group by p.age as a | show p.name`, "syntax", "p isn't a group name"},
		{`find Person p | group by p.age as a | show count(count())`, "syntax", "aggregates can't be nested"},
		{`find Person p | having count() > 1`, "syntax", "having filters groups"},
		{`find Person p | group by p.age as a | group by a as b`, "syntax", "already grouped"},
		{`find Person p | limit p.age`, "syntax", "limit needs a constant value"},
		{`find Person p | show p.name, p.name`, "constraint", "two columns are named p.name"},
		{`find Person p | show p.name | where p.age > 1`, "not_found", "no variable p"},
		{`find Person p | show p.name | union (find Company c)`, "", ""},
		{`find Person p, Company c | show p.name | union (find Company c)`, "", ""},
		{`find Person p | union (find Company c, Person q)`, "type", "union needs the same number of columns"},
		// patterns
		{`find Person p | follow q ~knows~> f`, "syntax", "follow must start at a bound variable"},
		{`find Person p | follow p ~knows~> Company`, "syntax", "Company is a kind; name a variable"},
		{`find Person p | follow p ~knows~> f | follow f ~knows~> p`, "syntax", "use match to connect p"},
		{`find Person p | follow Person p ~knows~> f`, "syntax", "p is already bound"},
		{`find Person p | follow p ~knows r*2~> f`, "syntax", "can't bind relation variable r"},
		{`find Person p | follow p ~knows{sinc: 1}~> f`, "unknown_field", "relation knows has no property sinc"},
		{`find Person p | follow p ~knows{since: "x"}~> f`, "type", "can't compare"},
		{`find Person p | follow p ~knows r~> f | where r.sinc > 1`, "unknown_field", "relation knows has no property sinc"},
		{`find Person p | follow p ~knows*3..2~> f`, "syntax", "1 ≤ min ≤ max"},
		{`find Person p | follow p ~knows~> f | let f = 1`, "constraint", "f is already bound"},
		// data statements
		{`create Persn { }`, "not_found", "no kind Persn"},
		{`create Person { name: "A", emial: "x" }`, "unknown_field", "no field emial (did you mean email?)"},
		{`create Person { name: "A" }`, "constraint", "required field email"},
		{`create Person { name: "A", email: "e", age: 1.5 }`, "type", "1.5 (float) can't be stored as int?"},
		{`create Person { name: "A", email: "e", score: 3 }`, "type", "write a float like 3.0"},
		{`create Person { name: "A", email: "e", joined: "2026-13-01" }`, "type", `"2026-13-01" isn't a valid date`},
		{`create Person { name: "A", email: "e", inv: "{\"number\": 1}" }`, "type", "isn't a valid doc<invoice>"},
		{`create Person { name: "A", email: "e", tags: ["a"] }`, "type", `write a set as {"a", "b"}`},
		{`create Person { name: "A", email: "e", addr: {city: "x", zap: "1"} }`, "unknown_field", "no field zap (did you mean zip?)"},
		{`create Person { name: "A", email: "e", addr: {zip: "1"} }`, "type", "the record needs field city"},
		{`create Person { name: "A", email: "e", name: "B" }`, "constraint", "given twice"},
		{`create Person { name: "A" doc "x", email: "e" }`, "constraint", "sets its doc"},
		{`create Invoice { number: "1", total: 1 } add x = 1`, "constraint", "kind Invoice is closed"},
		{`create Person { name: "A", email: "e" } add name = "x"`, "constraint", "already has a field name"},
		{`create Person { name: "A", email: "e", manager: @k }`, "type", "can't store a knows relation"},
		{`update @alice set emial = "x"`, "", ""}, // extensible kind: checked at run time
		{`update @k set x = 1`, "type", "update needs an entity"},
		{`update @alice set age = "x"`, "type", `"x" (text) can't be stored as int?`},
		{`update @alice set age ++= 1`, "type", "use += to add numbers"},
		{`update @alice set name += 1`, "type", "+ adds numbers; use ++ to join text"},
		{`update @alice set joined += 1`, "type", "can't use + with date and int"},
		{`update @alice set ::created = now()`, "constraint", "::created is read-only"},
		{`update @alice unset name`, "constraint", "name is a required field of Person"},
		{`update @alice drop name`, "constraint", "can't be dropped from one entity"},
		{`update @alice set age = nme`, "", ""},
		{`find Person p | update p set age = age * 2`, "", ""},
		{`find Person p | update p set age = name`, "type", "text can't be stored as int?"},
		{`find Person p | let name = 1 | update p set age = name`, "syntax", "both a variable and a field"},
		{`update p set age = 1`, "syntax", "only names a target inside a pipeline"},
		{`update $name set age = 1`, "type", "$name must be an id or ref"},
		{`delete @k`, "type", "use unrelate"},
		{`relate @alice ~knows|likes~> @bob`, "syntax", "exactly one relation type"},
		{`relate @alice ~knows r~> @bob`, "syntax", "can't bind a relation variable"},
		{`relate @alice ~knows*2~> @bob`, "syntax", "can't have a depth"},
		{`relate @alice ~knows~ @bob`, "syntax", "needs a direction"},
		{`relate @alice ~knows{sinc: 1}~> @bob`, "unknown_field", "no property sinc (did you mean since?)"},
		{`relate @alice ~knows{since: "x"}~> @bob`, "type", "can't be stored as int?"},
		{`relate @alice ~located_in~> @bob`, "constraint", "located_in relations must start at a field"},
		{`unrelate @alice`, "type", "@alice is an entity"},
		{`push @alice.inbox "x"`, "constraint", "give a priority"},
		{`push @alice.jobs @bob priority 1`, "constraint", "has no priorities"},
		{`push @alice.inbox 1 priority 1`, "type", "can't be stored as text"},
		{`push @alice.name "x"`, "type", "name is text, not a queue"},
		{`pop @alice.inbox as x`, "syntax", "only works as a pipeline step"},
		{`pop "x" from @alice.inbox`, "type", "can't be stored as int"},
		{`find Person p | delete p | show p`, "syntax", "delete must be the last step"},
		{`find Person p | pop p.jobs as j | show j | show j`, "syntax", "must be the last step"},
		// definitions
		{`define kind K { a: int default "x" }`, "type", `"x" (text) can't be stored as int`},
		{`define kind K { a: int default b }`, "syntax", "a default needs a constant value"},
		{`define kind K { a: int check b > 1 }`, "unknown_field", "no field b (did you mean a?)"},
		{`define kind K { a: int check a + 1 }`, "type", "check needs a condition"},
		{`define kind K { a: ref<Nobody> }`, "not_found", "no kind Nobody"},
		{`define kind K { a: doc<yaml> }`, "not_found", "no format yaml"},
		{`define kind K { a: int, a: text }`, "constraint", "field a is defined twice"},
		{`define kind K { a: int on delete restrict }`, "type", "on delete restrict only applies to ref fields"},
		{`define kind K history keep 3 { a: int }`, "type", "can't be stored as duration"},
		{`define relation r from Persn to any`, "not_found", "no kind Persn"},
		{`define format f : yaml { a: int }`, "not_found", "no base format yaml"},
		{`define format f : json header { a: int }`, "syntax", "header only applies to csv and tsv formats"},
		{`define format f : json { a: int at 1 }`, "syntax", "at and default only apply to csv and tsv columns"},
		{`define format f : xml`, "syntax", "a xml format needs fields"},
		{`define format f : csv { a: list<int> }`, "type", "a column holds"},
		{`define format f : csv { a: int, b: int at 0 }`, "constraint", "columns a and b both read column 0"},
		{`define format f : csv { a: int default "x" }`, "type", `"x" (text) can't be stored as int`},
		{`define format f : csv header { a: int default b }`, "syntax", "a default needs a constant value"},
		{`find Person p | where p.team->[0]->agee > 1`, "unknown_field", "format staff has no field agee here (did you mean age?)"},
		{`find Person p | where p.team->[0]->age = "x"`, "type", "can't compare int? with text"},
		{`find Person p | where p.team->name = "x"`, "type", ".name reads fields"},
		{`find Person p | where p.rows->[0]->email = "x"`, "type", "->email reads a column by name, which needs a header"},
		{`alter kind Person add name : text`, "constraint", "already has a field name"},
		{`alter kind Person alter nme : text`, "unknown_field", "no field nme (did you mean name?)"},
		{`alter kind Person drop nme`, "unknown_field", "no field nme"},
		{`alter kind Persn drop x`, "not_found", "no kind Persn"},
		{`alter relation knows add since : int`, "constraint", "already has a property since"},
		{`alter relation nope drop x`, "not_found", "no relation type nope"},
	}
	for _, tc := range cases {
		_, _, err := checkSrc(t, env, tc.src)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.src, err)
			}
			continue
		}
		e, ok := err.(*Error)
		if !ok {
			t.Errorf("%q: got %v, want a %s error containing %q", tc.src, err, tc.code, tc.want)
			continue
		}
		if e.Code != tc.code || !strings.Contains(e.Msg, tc.want) {
			t.Errorf("%q: got %s: %s, want %s containing %q", tc.src, e.Code, e.Msg, tc.code, tc.want)
		}
		if e.Pos.Line == 0 {
			t.Errorf("%q: error has no position: %v", tc.src, e)
		}
	}
}

func TestCheckNoDatabase(t *testing.T) {
	_, _, err := checkSrc(t, &Env{}, `find Person p`)
	if e, ok := err.(*Error); !ok || e.Code != "not_found" || !strings.Contains(e.Msg, "no database in use") {
		t.Errorf("got %v", err)
	}
	if _, _, err := checkSrc(t, &Env{}, `create database x`); err != nil {
		t.Errorf("create database: %v", err)
	}
}

func TestCheckInfo(t *testing.T) {
	env := testEnv(t)

	// Literal conversions for field writes.
	info, s, err := checkSrc(t, env, `create Person { name: "A", email: "e", joined: "2026-01-02", manager: #01JD8X3K7M0000Q2W5ZB9N4T6R, config: "[1]" }`)
	if err != nil {
		t.Fatal(err)
	}
	inits := s.(*Create).Inits
	if v, ok := info.Consts[inits[2].Value].(value.Date); !ok || v.String() != `d"2026-01-02"` {
		t.Errorf("joined: got %v", info.Consts[inits[2].Value])
	}
	if _, ok := info.Consts[inits[3].Value].(value.Ref); !ok {
		t.Errorf("manager: got %T", info.Consts[inits[3].Value])
	}
	if d, ok := info.Consts[inits[4].Value].(value.Doc); !ok || d.Format() != "json" {
		t.Errorf("config: got %v", info.Consts[inits[4].Value])
	}
	if _, ok := info.Consts[inits[0].Value]; ok {
		t.Errorf("name: text written to text must not be converted")
	}

	// Columns and their types.
	info, s, err = checkSrc(t, env, `find Person p | follow p ~knows r~> Person f | show p.name, f.age as age, r.since, count() as n`)
	if err == nil {
		t.Fatal("mixing aggregates and row values should fail")
	}
	info, s, err = checkSrc(t, env, `find Person p | follow p ~knows r~> Person f | show p.name, f.age as age, r.since, p.inv->total`)
	if err != nil {
		t.Fatal(err)
	}
	pl := s.(*Pipeline)
	var got []string
	for _, c := range info.Columns[pl] {
		got = append(got, c.Name+":"+c.Type.String())
	}
	want := "p.name:text age:int? r.since:int? p.inv->total:decimal(12,2)?"
	if strings.Join(got, " ") != want {
		t.Errorf("columns: got %q, want %q", strings.Join(got, " "), want)
	}

	// Without show, the columns are the bound variables.
	info, s, _ = checkSrc(t, env, `find Person p | follow path pth = p ~knows r~> Person f`)
	got = nil
	for _, c := range info.Columns[s.(*Pipeline)] {
		got = append(got, c.Name+":"+c.Type.String())
	}
	if want := "p:a Person entity f:a Person entity r:a knows relation pth:a path"; strings.Join(got, " ") != want {
		t.Errorf("columns: got %q, want %q", strings.Join(got, " "), want)
	}

	// Implicit whole-stream grouping.
	info, s, err = checkSrc(t, env, `find Person p | show count() as n, avg(p.age) as a`)
	if err != nil {
		t.Fatal(err)
	}
	show := s.(*Pipeline).Steps[0]
	if !info.Grouped[show] {
		t.Error("show with aggregates should group the whole stream")
	}
	if cols := info.Columns[s.(*Pipeline)]; cols[1].Type.String() != "decimal?" {
		t.Errorf("avg(int) type: got %s", cols[1].Type)
	}

	// Name references.
	info, s, err = checkSrc(t, env, `find Person p | update p set age = age + 1`)
	if err != nil {
		t.Fatal(err)
	}
	upd := s.(*Pipeline).Steps[0].(*DataStep).Stmt.(*Update)
	add := upd.Actions[0].(*SetAction).Assigns[0].Value.(*Binary)
	if r := info.Refs[add.X]; r != (Ref{RefField, "age"}) {
		t.Errorf("age: got %+v", r)
	}
	if r := info.Refs[upd.Target]; r != (Ref{RefVar, "p"}) {
		t.Errorf("target: got %+v", r)
	}

	// Show columns can be named by their expression afterwards.
	info, s, err = checkSrc(t, env, `find Person p | show p.name | sort p.name`)
	if err != nil {
		t.Fatal(err)
	}
	key := s.(*Pipeline).Steps[1].(*Sort).Keys[0].Expr
	if r := info.Refs[key]; r != (Ref{RefColumn, "p.name"}) {
		t.Errorf("sort key: got %+v", r)
	}

	// Typed doc paths.
	_, ty, err := CheckExpr(env, mustExpr(t, `p.inv->lines->[0]->sku`), map[string]Static{"p": env.Handles["alice"]})
	if err != nil || ty.String() != "text?" {
		t.Errorf("doc path: got %v, %v", ty, err)
	}
}

func TestCheckWarnings(t *testing.T) {
	env := testEnv(t)
	info, _, err := checkSrc(t, env, `find Person p | follow p ~knws~> f`)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0].Msg, "no relation of type knws has been created yet (did you mean knows?)") {
		t.Errorf("got %v", info.Warnings)
	}
	info, _, err = checkSrc(t, env, `find Person p | match Company c ~works_at~> Person q`)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0].Msg, "cross product") {
		t.Errorf("got %v", info.Warnings)
	}
}

// FuzzCheck checks that the checker never panics, and that every error
// it reports has a code and a position.
func FuzzCheck(f *testing.F) {
	for _, s := range corpus {
		f.Add(s)
	}
	f.Add(`find Person p | follow p ~knows r~> Person f | group by f.age as a | show a, count()`)
	f.Add(`create Person { name: "A", email: "e", inv: "{}" } add x : list<int> = [1]`)
	f.Add(`find Person p | where p.inv->lines->[0]->qty > 1 | update p set attrs["k"] += 1`)
	env := testEnv(&testing.T{})
	f.Fuzz(func(t *testing.T, src string) {
		stmts, err := ParseScript(src)
		if err != nil {
			return
		}
		for _, s := range stmts {
			if _, err := Check(env, s); err != nil {
				e, ok := err.(*Error)
				if !ok || e.Code == "" || e.Code == "internal" {
					t.Fatalf("%q: bad error %#v", Format(s), err)
				}
			}
		}
	})
}
