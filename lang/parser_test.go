package lang

import (
	"strings"
	"testing"
)

// corpus holds statements covering the grammar, most taken from the
// spec's examples.
var corpus = []string{
	`create database sales`,
	`drop database sales`,
	`use sales`,
	`set timeout = 30s`,
	`set max_rows = 10000`,
	`define kind Person "A human being" {
	  name     : text                         "Full name"
	  email    : text unique                  "Login email"
	  age      : int? check age >= 0
	  joined   : date default today()
	  photo    : image?
	  tags     : set<text> default {}
	  manager  : ref<Person>? on delete restrict
	  inbox    : queue.priority<text>
	  salary   : decimal(12,2)? history keep 365 * 24h
	}`,
	`define kind Invoice closed { n: int }`,
	`define kind Note history { body: text, check: int }`,
	`define relation knows from Person to Person "Personal acquaintance" { since: int? }`,
	`define relation located_in from any to City`,
	`define relation spouse from Person to Person max out 1 max in 1`,
	`define relation placed_by from Order to Customer max out 1 on delete restrict`,
	`define format invoice : json "Customer invoice" {
	  number : text
	  total  : decimal(12,2)
	  lines  : list<record{ sku: text, qty: int }>
	}`,
	`define index person_age on Person(age)`,
	`define index order_lookup on Order(customer, placed)`,
	`define unique index sku on Product(sku)`,
	`define text index person_bio on Person(bio)`,
	`define index cfg_port on Server(config->server->port)`,
	`define index x on Server(config->"odd key"->[0]->@id)`,
	`drop index person_age`,
	`alter kind Person add nickname : text? "Informal name"`,
	`alter kind Person drop nickname`,
	`alter kind Person alter age : int? check age between 0 and 150`,
	`alter kind Person rename to Human`,
	`alter kind Person set closed`,
	`alter kind Person set extensible`,
	`alter relation spouse set max out 2`,
	`alter relation knows add since : int?`,
	`drop kind Person`,
	`create Person @alice { name: "Alice", email: "a@x.io", age: 34, tags: {"eng"} }`,
	`create Person { name: "Bob", email: "b@x.io" }`,
	`create @n { title: "Loose note", body: "no kind" }`,
	`create @m { title: "Standup", room: "B2" doc "Building and room" }`,
	`create Person @carol { name: "Carol", email: "c@x.io" } add hobby : text = "chess" doc "Main hobby"`,
	`create { "key with space": 1, x: [1, 2] }`,
	`update @alice set age = 35, email = "alice@x.io"`,
	`update @alice unset age`,
	`update @alice set age += 1`,
	`update @alice set tags ++= {"ops"}, tags --= {"eng"}`,
	`update @alice set name ++= " Jr."`,
	`update @alice set ::labels ++= {"vip"}`,
	`update @alice add hobby : text = "chess" doc "Main hobby"`,
	`update @n add pinned : bool = true, scores : list<int> = [1, 2]`,
	`update @n add attrs = {k: "v"}`,
	`update @n set scores[0] = 10, attrs["k"] = "w"`,
	`update @alice drop hobby`,
	`update #01JD8X3K7M0000Q2W5ZB9N4T6R set age = 36`,
	`update $who set a.b = 1 unset c add d = 2 drop e`,
	`delete @alice`,
	`relate @alice ~knows{since: 2019}~> @bob`,
	`relate @alice ~knows~> @bob as @k`,
	`relate @alice.address ~located_in~> @springfield`,
	`unrelate @alice ~knows~> @bob`,
	`unrelate @k`,
	`push @alice.inbox "hello" priority 5`,
	`push @worker.jobs @task1`,
	`pop @alice.inbox`,
	`pop 10 from @alice.inbox`,
	`pop @worker.jobs wait 30s`,
	`pop @worker.jobs as job`,
	`find Person p | where p.age > 120 | update p set ::labels ++= {"check_age"}`,
	`find Person p | where p::labels has "spam" | delete p`,
	`find Person p | follow p ~knows r~> Person f | where r.since < 1990 | unrelate r`,
	`find Worker w | where w.idle | pop w.jobs as job | show w.name, job`,
	`find Person p`,
	`find p`,
	`find Person p, Company c`,
	`find @alice as p`,
	`find #01JD8X3K7M0000Q2W5ZB9N4T6R as x`,
	`find Person p in [@alice, @bob]`,
	`find Person p as of t"2026-01-01T00:00:00Z" | show p.name, p.salary`,
	`find Person p | where p.age > 30 and "eng" in p.tags | follow p ~knows~> Person f | show p.name, f.name`,
	`find Person p | match p ~works_at~> Company c <~works_at~ f`,
	`find Person p | let full = p.first ++ " " ++ p.last`,
	`find Person p | group by p.city as city | having count() > 5 | show city, count() as n`,
	`find Person p | sort p.age desc, p.name | skip 20 | limit 10`,
	`find Person p | distinct | distinct f | distinct a, b`,
	`find Person p | bind p as @alice`,
	`find Person p | show p.name | union (find Bot b | show b.name)`,
	`find p | follow p ~knows~> Person f | follow p <~manages~ Person boss | follow p ~knows~ f2`,
	`find p | follow p ~knows|likes~> f | follow p ~*~> x | follow p ~knows*1..3~> Person fof | follow p ~knows*~> y | follow p ~knows*2~> z`,
	`find p | follow p.address ~located_in~> City c | follow p ~mentions~> x.title`,
	`find p | follow path pth = p ~knows*1..3~> f | show len(pth), nodes(pth)`,
	`find Person p
	  | where p.age > 30 and "eng" in p.tags
	  | follow p ~works_at~> Company c
	  | follow p ~knows*2~> Person fof
	  | match fof ~works_at~> c
	  | distinct p, fof
	  | sort p.name
	  | show p.name, fof.name as colleague, c.name as company
	  | limit 50`,
	`begin`,
	`begin read only`,
	`commit`,
	`rollback`,
	`create user ramon password $pw`,
	`alter user ramon password $new_pw`,
	`alter user ramon add key "3f9a" as "laptop"`,
	`alter user ramon drop key "3f9a"`,
	`create role analyst`,
	`grant read on database sales to analyst`,
	`grant write on kind sales.Order to analyst`,
	`grant define on database sales to analyst`,
	`grant read, write on server to ops`,
	`grant analyst to ramon`,
	`revoke write on kind sales.Order from analyst`,
	`drop role analyst`,
	`show users`, `show roles`, `show grants for ramon`, `show keys for ramon`, `show grants`,
	`describe @alice`,
	`describe kind Person`,
	`describe relation knows`,
	`describe #01JD8X3K7M0000Q2W5ZB9N4T6R`,
	`show kinds`, `show relations`, `show formats`, `show indexes`, `show stats`, `show stats kind Person`, `show sessions`,
	`explain find Person p | where p.age > 30`,
	`explain analyze find Person p | where p.age > 30`,
	`history @alice.salary`,
	// expressions
	`find p | where not a = b or c and not d`,
	`find p | where a < b and x not in [1, 2] and y between 1 and 2 and z not between 3 and 4`,
	`find p | where x is null and y is not int? and z is Person and w is list<text>`,
	`find p | where p.email matches ".*@x\\.io$" and p.name starts with "A" and p.name ends with "z" and p.bio contains "go"`,
	`find p | show -p.x, - -1, -(-1), 1 - -1, (1 + 2) * 3, 1 + 2 * 3, a ++ b -- c, 10 % 3 / 2`,
	`find p | show cast(p.age as float) * 1.5, if(p.age > 18, "adult", "minor"), coalesce(p.a, p.b, 0), exists(p.age)`,
	`find p | show p.config->server->port, p.config->"key with space", p.items->[0], p.x->@id, p.x->@"odd name"`,
	`find p | show p.name::id, p::created, p::props.source, p.tags[0], m["k"]`,
	`find p | follow p ~knows r~> f | show p::kind, p.name::doc, r::from, r::to, r::type`,
	`update @a set ::labels ++= {"x"}, ::props = {}`,
	`find p | show {}, {1, 2}, {a: 1, "b c": 2, 3: x}, [], [1, [2]], "k" => 1, {k: 1 => 2}`,
	`find p | show 42, 1_000, 0x1F, 0b1010, 123n, 3.14, 1e-9, 19.99d, 250ms, 1h30m, true, false, null, nan, inf, -inf`,
	`find p | show "a\n", r` + "`raw`" + `, x"dead", b64"3q0=", t"2026-11-21T14:30:00Z", d"2026-11-21", $param, @handle`,
	"find p | show `match`, `first name`, order",
}

func TestParseCorpusRoundTrip(t *testing.T) {
	for _, src := range corpus {
		s, err := ParseStatement(src)
		if err != nil {
			t.Errorf("parse %q: %v", src, err)
			continue
		}
		once := Format(s)
		s2, err := ParseStatement(once)
		if err != nil {
			t.Errorf("reparse of %q failed: %v\nformatted: %s", src, err, once)
			continue
		}
		if twice := Format(s2); twice != once {
			t.Errorf("format not stable for %q:\n once %s\ntwice %s", src, once, twice)
		}
	}
}

func TestParseDetails(t *testing.T) {
	s := mustParse(t, `define kind Person { age: int? check age >= 0, check: text }`).(*DefineKind)
	if len(s.Fields) != 2 || s.Fields[0].Check == nil || s.Fields[1].Name != "check" {
		t.Errorf("a field named check after a check option: %+v", s.Fields)
	}

	e := mustParse(t, `find p | follow p ~knows r{since: 2019}*1..3~> Person f`).(*Pipeline).Steps[0].(*Follow)
	edge := e.Pattern.Edges[0]
	if edge.Var != "r" || edge.Props == nil || !edge.HasDepth || edge.Min != 1 || edge.Max != 3 || edge.Dir != DirOut {
		t.Errorf("edge: %+v", edge)
	}
	if n := e.Pattern.Nodes[1]; n.Kind != "Person" || n.Var != "f" {
		t.Errorf("node: %+v", n)
	}

	pop := mustParse(t, `pop 10 from @q.items wait 5s as job`).(*Pop)
	if pop.Count == nil || pop.Field.Field != "items" || pop.Wait == nil || pop.As != "job" {
		t.Errorf("pop: %+v", pop)
	}
	if pop := mustParse(t, `pop @q.items`).(*Pop); pop.Count != nil {
		t.Error("pop without count got one")
	}

	// Precedence: not binds looser than comparison, => loosest.
	if got := FormatExpr(mustExpr(t, `not a = b`)); got != "not a = b" {
		t.Errorf("not: %s", got)
	}
	b := mustExpr(t, `1 + 2 * 3 = 7 and x`).(*Binary)
	if b.Op != "and" || b.X.(*Binary).Op != "=" {
		t.Errorf("precedence: %s", FormatExpr(b))
	}
	if got := FormatExpr(mustExpr(t, `(a => b) => c`)); got != "(a => b) => c" {
		t.Errorf("pair parens: %s", got)
	}
	if got := FormatExpr(mustExpr(t, `a - (b - c)`)); got != "a - (b - c)" {
		t.Errorf("right assoc parens: %s", got)
	}
	if _, ok := mustExpr(t, `{}`).(*MapLit); !ok {
		t.Error("{} should be an empty map")
	}
	scr, err := ParseScript("use a; begin; commit;; rollback")
	if err != nil || len(scr) != 4 {
		t.Errorf("script: %d, %v", len(scr), err)
	}
}

func mustParse(t *testing.T, src string) Stmt {
	t.Helper()
	s, err := ParseStatement(src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return s
}

func mustExpr(t *testing.T, src string) Expr {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return e
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		`FIND Person p`:                                         "keywords are lowercase: did you mean find?",
		`fnid Person p`:                                         "did you mean find?",
		`find Person p | wher p.x`:                              "did you mean where?",
		`find p | where a < b < c`:                              "comparisons don't chain",
		`define kind Person { match: int }`:                     "keyword match; write `match`",
		`define kind P { a: txt }`:                              "unknown type txt (did you mean text?)",
		`define kind P { a: set<list<int>> }`:                   "not keyable",
		`define kind P { a: any? }`:                             "any? is not allowed",
		`define kind P { a: queue.stack<int> }`:                 "unknown queue kind",
		`find p | show {a: 1, 2}`:                               `expected ":" after a map key`,
		`find p | show {1, a: 2}`:                               "can't mix",
		`find p | show if(a, b)`:                                "if takes 3 arguments",
		`find p | follow p f`:                                   "expected a relation step",
		`find p | follow p ~knows f`:                            "expected ~> or ~",
		`update @a`:                                             "expected set, unset, add or drop",
		`update @a set x == 1`:                                  "expected an expression",
		`show tables`:                                           "can't show tables",
		`use a b`:                                               "expected the end of the statement",
		`find p | show 1 +`:                                     "expected an expression",
		`relate @a ~knows~> `:                                   "expected a handle, ID, parameter or variable",
		`grant read on planet x to y`:                           "expected server, database or kind",
		`find p | where x is 5`:                                 "expected null, a type or a kind",
		`select * from t`:                                       "expected a statement",
		`define kind P { a: int unique unique }`:                "unique is given more than once",
		`define kind P { a: int "x" doc "y" }`:                  "doc is given more than once",
		`define kind P closed closed { }`:                       "closed is given more than once",
		`define relation r from any to any max out 1 max out 2`: "max out is given more than once",
		`update @a add x = 1 check x > 0 check x < 5`:           "check is given more than once",
		`update @a add x = 1 unique`:                            "unique isn't allowed in an add clause",
	}
	for src, want := range cases {
		_, err := ParseStatement(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want containing %q", src, err, want)
		}
	}
	_, err := ParseStatement("find p\n  | where p.x >")
	if e, ok := err.(*Error); !ok || e.Pos.Line != 2 {
		t.Errorf("error position: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range corpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		stmts, err := ParseScript(src)
		if err != nil {
			return
		}
		for _, s := range stmts {
			once := Format(s)
			s2, err := ParseStatement(once)
			if err != nil {
				t.Fatalf("formatted statement doesn't parse: %v\nsource: %q\nformatted: %q", err, src, once)
			}
			if twice := Format(s2); twice != once {
				t.Fatalf("format not stable:\nsource: %q\n once: %q\ntwice: %q", src, once, twice)
			}
		}
	})
}
