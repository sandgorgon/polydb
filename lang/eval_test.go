package lang

import (
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/catalog"
	"github.com/sandgorgon/polydb/value"
)

// fakeGraph is an in-memory Graph for evaluator tests.
type fakeGraph struct {
	cat    *catalog.Catalog
	kinds  map[value.ID]string
	fields map[value.ID]map[string]value.Value
}

func (g *fakeGraph) Field(id value.ID, name string) (value.Value, bool, error) {
	v, ok := g.fields[id][name]
	return v, ok, nil
}

func (g *fakeGraph) FieldMeta(id value.ID, field, name string) (value.Value, bool, error) {
	if _, ok := g.fields[id][field]; !ok {
		return value.Null, false, nil
	}
	switch name {
	case "name":
		return value.MustText(field), true, nil
	case "declared":
		return value.Bool(field != "nick"), true, nil
	}
	return value.Null, true, nil
}

func (g *fakeGraph) Meta(id value.ID, name string) (value.Value, error) {
	switch name {
	case "kind":
		if k, ok := g.kinds[id]; ok {
			return value.MustText(k), nil
		}
		return value.Null, nil
	case "id":
		return id, nil
	}
	return value.Null, nil
}

func (g *fakeGraph) Degree(value.ID, string, string) (int, error) { return 2, nil }

func (g *fakeGraph) Format(name string) (value.Format, bool) { return g.cat.Format(name) }

type evalFixture struct {
	env   *Env
	graph *fakeGraph
	ann   value.ID
	bob   value.ID
}

func newEvalFixture(t *testing.T) *evalFixture {
	env := testEnv(t)
	gen := value.NewIDGenerator(0)
	ann, bob := gen.New(), gen.New()
	doc := func(format, text string) value.Value {
		f, _ := env.Catalog.Format(format)
		d, err := value.NewDoc(f, text)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	g := &fakeGraph{
		cat:   env.Catalog,
		kinds: map[value.ID]string{ann: "Person", bob: "Person"},
		fields: map[value.ID]map[string]value.Value{
			ann: {
				"name":    value.MustText("Ann"),
				"nick":    value.Null, // present but null
				"manager": value.Ref{Target: bob},
				"tags":    mustV(value.NewSet(value.MustText("eng"))),
				"config":  doc("json", `{"server": {"port": 80}, "list": [1, 2], "key with space": true}`),
				"page":    doc("xml", `<config><server port="80">hi<b>x</b></server></config>`),
				"inv":     doc("invoice", `{"number": "7", "total": 2.5, "lines": [{"sku": "a", "qty": 2}]}`),
				"team":    doc("staff", "name,age,status,x,mail\nAnn,34,,1,a@x.io\nBob,,away,2,b@x.io\n"),
				"book":    doc("contacts", "email,phone\na@x.io,555\n"),
				"rows":    doc("csv", "1,2\n3,4\n"),
			},
			bob: {"name": value.MustText("Bob"), "age": value.Int(40)},
		},
	}
	env.Handles["alice"] = Static{Form: FormEntity, Kind: env.Handles["alice"].Kind}
	return &evalFixture{env: env, graph: g, ann: ann, bob: bob}
}

func mustV[T value.Value](v T, err error) value.Value {
	if err != nil {
		panic(err)
	}
	return v
}

func (fx *evalFixture) evaluator(info *Info) *Evaluator {
	return &Evaluator{
		Info:    info,
		Graph:   fx.graph,
		Params:  fx.env.Params,
		Handles: map[string]value.Value{"alice": value.Ref{Target: fx.ann}, "bob": value.Ref{Target: fx.bob}},
		Now:     time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC),
	}
}

// eval checks and evaluates src with p bound to Ann.
func (fx *evalFixture) eval(t *testing.T, src string) (value.Value, error) {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("%s: parse: %v", src, err)
	}
	info, _, err := CheckExpr(fx.env, e, map[string]Static{"p": fx.env.Handles["alice"]})
	if err != nil {
		return nil, err
	}
	return fx.evaluator(info).Eval(e, &Frame{Row: Row{"p": value.Ref{Target: fx.ann}}})
}

func TestEval(t *testing.T) {
	fx := newEvalFixture(t)
	cases := map[string]string{
		// arithmetic
		`1 + 2 * 3`:                    "7",
		`7 / 2`:                        "3",
		`-7 / 2`:                       "-3",
		`-7 % 2`:                       "-1",
		`1.5d + 1`:                     "2.5d",
		`10d / 4`:                      "2.5d",
		`1d / 3`:                       "0.3333333333333333d",
		`2.5 * 2.0`:                    "5.0",
		`5n * 2`:                       "10n",
		`10n / 4`:                      "2n",
		`2n + 1.5d`:                    "3.5d",
		`-(3)`:                         "-3",
		`7.5d % 2`:                     "1.5d",
		`1.0 / 0.0`:                    "inf",
		`2 * 1h`:                       "2h0m0s",
		`90m / 2`:                      "45m0s",
		`1h - 30m`:                     "30m0s",
		`t"2026-01-31T00:00:00Z" + 1h`: `t"2026-01-31T01:00:00Z"`,
		`t"2026-01-31T00:00:00Z" - t"2026-01-30T00:00:00Z"`: "24h0m0s",
		// null rules
		`null = null`:              "true",
		`null != 1`:                "true",
		`null < 1`:                 "false",
		`1 + null`:                 "null",
		`null and false`:           "false",
		`null or true`:             "true",
		`null and true`:            "null",
		`not null`:                 "null",
		`lower(null)`:              "null",
		`coalesce(null, 2)`:        "2",
		`5 between 1 and 10`:       "true",
		`null between 1 and 2`:     "false",
		`null not between 1 and 2`: "true",
		// comparisons
		`1 = 1.0`:   "true",
		`1 < 1.5d`:  "true",
		`"a" < "b"`: "true",
		`nan = nan`: "true",
		// text
		`"ab" ++ "cd"`:                `"abcd"`,
		`lower("ÀB")`:                 `"àb"`,
		`upper("ab")`:                 `"AB"`,
		`trim("  a ")`:                `"a"`,
		`substr("héllo", 1, 3)`:       `"éll"`,
		`substr("héllo", 3)`:          `"lo"`,
		`substr("ab", 5, 1)`:          `""`,
		`len("héllo")`:                "5",
		`split("a,b", ",")`:           `["a", "b"]`,
		`join(["a", "b"], "-")`:       `"a-b"`,
		`replace("aaa", "a", "b")`:    `"bbb"`,
		`"abc" contains "b"`:          "true",
		`"abc" starts with "ab"`:      "true",
		`"abc" ends with "bc"`:        "true",
		`"a@x.io" matches "@x\\.io$"`: "true",
		`"a@y.io" matches "@x\\.io$"`: "false",
		`typeof(1.5d)`:                `"decimal"`,
		`typeof(null)`:                `"null"`,
		// collections
		`[1, 2] ++ [3]`:                "[1, 2, 3]",
		`{1, 2} ++ {2, 3}`:             "{1, 2, 3}",
		`{"a": 1} ++ {"a": 2, "b": 3}`: `{"a": 2, "b": 3}`,
		`[1, 2, 1, 3] -- [1]`:          "[2, 3]",
		`{1, 2, 3} -- {2}`:             "{1, 3}",
		`{"a": 1, "b": 2} -- {"a"}`:    `{"b": 2}`,
		`2 in [1, 2]`:                  "true",
		`3 not in [1, 2]`:              "true",
		`{"a": 1} has "a"`:             "true",
		`[1, 2][1]`:                    "2",
		`[1, 2][5]`:                    "null",
		`{"k": 1}["k"]`:                "1",
		`{"k": 1}["z"]`:                "null",
		`first([])`:                    "null",
		`last([1, 2])`:                 "2",
		`sort([3, 1, 2])`:              "[1, 2, 3]",
		`slice([1, 2, 3, 4], 1, 3)`:    "[2, 3]",
		`keys({"b": 1, "a": 2})`:       `["a", "b"]`,
		`values({"b": 1, "a": 2})`:     "[2, 1]",
		`len({1, 2})`:                  "2",
		`contains([1, 2], 2)`:          "true",
		`"k" => 1`:                     `"k" => 1`,
		`if(1 > 2, "a", "b")`:          `"b"`,
		`if(null, "a", "b")`:           `"b"`,
		`min(3, 1, 2)`:                 "1",
		`max(1, 2.5)`:                  "2.5",
		// numbers
		`abs(-3)`:             "3",
		`abs(-1.5d)`:          "1.5d",
		`round(2.5)`:          "3.0",
		`round(2.345d, 2)`:    "2.35d",
		`round(-2.345d, 2)`:   "-2.35d",
		`round(1.5d)`:         "2d",
		`round(1250, -2)`:     "1300",
		`round(-1250, -2)`:    "-1300",
		`round(1.2345, 2)`:    "1.23",
		`floor(-1.5d)`:        "-2d",
		`ceil(1.2)`:           "2.0",
		`sqrt(4.0)`:           "2.0",
		`pow(2, 10)`:          "1024",
		`pow(1.5d, 2)`:        "2.25d",
		`pow(4.0, 0.5)`:       "2.0",
		`pow(1d, 1000000000)`: "1d",
		`pow(-1, 1000000001)`: "-1",
		`pow(0.5d, 3)`:        "0.125d",
		`pow(2n, 70)`:         "1180591620717411303424n",
		// time
		`add_months(d"2026-01-31", 1)`:                     `d"2026-02-28"`,
		`add_months(d"2026-01-31", -2)`:                    `d"2025-11-30"`,
		`year(d"2026-03-04") * 100 + month(d"2026-03-04")`: "202603",
		`day(t"2026-03-04T23:00:00Z")`:                     "4",
		`truncate(t"2026-01-01T10:35:00Z", 1h)`:            `t"2026-01-01T10:00:00Z"`,
		`now()`:                                            `t"2026-11-22T10:30:00Z"`,
		`today()`:                                          `d"2026-11-22"`,
		// casts
		`cast("42" as int)`:                     "42",
		`cast(2.5 as int)`:                      "3",
		`cast(-2.5 as int)`:                     "-3",
		`cast(3 as float)`:                      "3.0",
		`cast(0.1 as decimal)`:                  "0.1d",
		`cast(1.255d as decimal(5,2))`:          "1.26d",
		`cast(1.5d as int)`:                     "2",
		`cast(42 as text)`:                      `"42"`,
		`cast(1.5d as text)`:                    `"1.5"`,
		`cast(d"2026-01-02" as time)`:           `t"2026-01-02T00:00:00Z"`,
		`cast(t"2026-01-02T23:00:00Z" as date)`: `d"2026-01-02"`,
		`cast("1h" as duration)`:                "1h0m0s",
		`cast("true" as bool)`:                  "true",
		`cast([1, 2] as set<int>)`:              "{1, 2}",
		`cast(["1", "2"] as list<int>)`:         "[1, 2]",
		`cast("{\"a\": 1}" as doc<json>)`:       `cast("{\"a\": 1}" as doc<json>)`,
		`cast(null as int)`:                     "null",
		// type tests
		`1 is int`:                 "true",
		`1 is decimal`:             "false",
		`1 is not text`:            "true",
		`null is int`:              "false",
		`null is int?`:             "true",
		`null is null`:             "true",
		`[1, 2] is list<int>`:      "true",
		`[1, "a"] is list<int>`:    "false",
		`1.25d is decimal(3,1)`:    "false",
		`1.5d is decimal(3,1)`:     "true",
		`p is Person`:              "true",
		`p is Company`:             "false",
		`p.manager is ref<Person>`: "true",
		// entities
		`p.name`:               `"Ann"`,
		`p.missing`:            "null",
		`p.manager.name`:       `"Bob"`,
		`p.manager.age + 1`:    "41",
		`p::kind`:              `"Person"`,
		`p.name::declared`:     "true",
		`p.nick::declared`:     "false",
		`p.missing::declared`:  "null",
		`p = @alice`:           "true",
		`p.manager = @bob`:     "true",
		`p.manager = id(@bob)`: "true",
		`degree(p, "knows")`:   "2",
		`"eng" in p.tags`:      "true",
		`exists(p.nick)`:       "true",
		`exists(p.missing)`:    "false",
		`exists(p.name)`:       "true",
		`p.nick is null`:       "true",
		// documents
		`p.config->server->port`:        "80",
		`p.config->list->[1]`:           "2",
		`p.config->"key with space"`:    "true",
		`p.config->nope->deeper`:        "null",
		`exists(p.config->server)`:      "true",
		`exists(p.config->nope)`:        "false",
		`p.page->server->@port`:         `"80"`,
		`p.page->server->[0]`:           `"hi"`,
		`p.page->server->b->[0]`:        `"x"`,
		`inner_text(p.page->server)`:    `"hix"`,
		`p.page->server.name`:           `"server"`,
		`p.page->nope->@x`:              "null",
		`p.inv->total`:                  "2.50d",
		`p.inv->lines->[0]->qty`:        "2",
		`p.inv->lines->[0]->sku ++ "!"`: `"a!"`,
		`parse(p.inv).number`:           `"7"`,
		`valid("{", "json")`:            "false",
		`valid("{}", "invoice")`:        "false",
		// tables
		`p.team->[0]->age + 1`: "35",
		`p.team->[0]->status`:  `"active"`,
		`p.team->[1]->status`:  `"away"`,
		`p.team->[1]->age`:     "null",
		`p.team->[1]->email`:   `"b@x.io"`,
		`p.team->[5]->email`:   "null",
		`len(parse(p.team))`:   "2",
		`p.book->[0]->phone`:   `"555"`,
		`p.rows->[1]->[0]`:     `"3"`,
		`valid("name,age,a,b,mail\nAnn,x,,,y\n", "staff")`:   "false",
		`valid("name,status,a,b,mail\nAnn,,,,x\n", "staff")`: "true",
	}
	for src, want := range cases {
		v, err := fx.eval(t, src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if v == nil {
			t.Errorf("%s: nil value", src)
			continue
		}
		if got := v.String(); got != want {
			t.Errorf("%s = %s, want %s", src, got, want)
		}
	}
}

func TestEvalErrors(t *testing.T) {
	fx := newEvalFixture(t)
	cases := map[string]string{
		`1 / 0`:                         "type: division by zero",
		`1d / 0`:                        "type: division by zero",
		`5n % 0`:                        "type: division by zero",
		`9223372036854775807 + 1`:       "limit: int overflow",
		`-9223372036854775807 - 2`:      "limit: int overflow",
		`4611686018427387904 * 2`:       "limit: int overflow",
		`pow(10, 19)`:                   "limit: int overflow",
		`cast("x" as int)`:              `can't cast "x" to int`,
		`cast(1e30 as int)`:             "out of range for int",
		`cast(nan as decimal)`:          "has no exact value",
		`cast(x"ff" as text)`:           "not valid UTF-8",
		`cast(1234.5d as decimal(4,1))`: "does not fit",
		`pow(2, -1)`:                    "exponent ≥ 0",
		`truncate(now(), 0s)`:           "positive duration",
		`pow(1.5d, 1000000000)`:         "decimal overflow",
		`pow(3, 1000000000)`:            "bigint overflow",
	}
	for src, want := range cases {
		_, err := fx.eval(t, src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", src, err, want)
		}
		if e, ok := err.(*Error); ok && e.Pos.Line == 0 {
			t.Errorf("%s: error has no position", src)
		}
	}
	// Run-time checks for values only known at run time: any fields.
	fx.graph.fields[fx.ann]["x"] = value.MustText("t")
	for src, want := range map[string]string{
		`p.x + 1`:         "+ adds numbers; use ++ to join text",
		`p.x < 1`:         "can't compare text with int",
		`p.x ++ 1`:        "can't join text and int",
		`p.x and true`:    "and needs true or false, not text",
		`p.x.y`:           ".y reads fields of entities, records and maps, not text",
		`p.x[0]`:          "[ ] reads from lists",
		`p.x->a`:          "-> reads inside documents, not text",
		`cast(p.x as id)`: "can't cast",
	} {
		_, err := fx.eval(t, src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", src, err, want)
		}
	}
	if _, err := (&Evaluator{Info: &Info{}}).Cond(&Lit{Val: value.Int(1)}, nil); err == nil {
		t.Error("Cond(1) should fail")
	}
}

func TestEvalAggregates(t *testing.T) {
	fx := newEvalFixture(t)
	s, err := ParseStatement(`find Person p | show count() as n, count(p.age) as c, sum(p.age) as s, avg(p.age) as a,
		min(p.name) as lo, max(p.age) as hi, collect(p.age) as ages, collect_set(p.name) as names`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	var group []Row
	for _, r := range []struct {
		name string
		age  value.Value
	}{{"Ann", value.Int(30)}, {"Bob", value.Null}, {"Cy", value.Int(41)}, {"Ann", value.Int(30)}} {
		id := value.NewIDGenerator(1).New()
		fx.graph.fields[id] = map[string]value.Value{"name": value.MustText(r.name), "age": r.age}
		group = append(group, Row{"p": value.Ref{Target: id}})
	}
	ev := fx.evaluator(info)
	want := []string{"4", "3", "101", "33.6666666666666667d", `"Ann"`, "41", "[30, 41, 30]", `{"Ann", "Bob", "Cy"}`}
	for i, it := range s.(*Pipeline).Steps[0].(*Show).Items {
		v, err := ev.Eval(it.Expr, &Frame{Group: group})
		if err != nil {
			t.Errorf("%s: %v", FormatExpr(it.Expr), err)
			continue
		}
		if v.String() != want[i] {
			t.Errorf("%s = %s, want %s", FormatExpr(it.Expr), v, want[i])
		}
	}
	// Over no rows.
	for i, it := range s.(*Pipeline).Steps[0].(*Show).Items {
		v, _ := ev.Eval(it.Expr, &Frame{})
		w := "null"
		switch i {
		case 0, 1:
			w = "0"
		case 6:
			w = "[]"
		case 7:
			w = "{}"
		}
		if v.String() != w {
			t.Errorf("empty %s = %s, want %s", FormatExpr(it.Expr), v, w)
		}
	}
}

func TestEvalContexts(t *testing.T) {
	fx := newEvalFixture(t)

	// An update reads the target's fields by bare name.
	s, _ := ParseStatement(`update @bob set age = age + 1`)
	info, err := Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	rhs := s.(*Update).Actions[0].(*SetAction).Assigns[0].Value
	v, err := fx.evaluator(info).Eval(rhs, &Frame{Target: fx.bob, HasTarget: true})
	if err != nil || v.String() != "41" {
		t.Errorf("age + 1 = %v, %v", v, err)
	}

	// A check reads the new field values.
	s, _ = ParseStatement(`define kind K { lo: int, hi: int? check hi is null or hi > lo }`)
	info, err = Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	check := s.(*DefineKind).Fields[1].Check
	ev := fx.evaluator(info)
	for _, c := range []struct {
		fields map[string]value.Value
		want   bool
	}{
		{map[string]value.Value{"lo": value.Int(1), "hi": value.Int(2)}, true},
		{map[string]value.Value{"lo": value.Int(3), "hi": value.Int(2)}, false},
		{map[string]value.Value{"lo": value.Int(3)}, true},
	} {
		got, err := ev.Cond(check, &Frame{Fields: c.fields})
		if err != nil || got != c.want {
			t.Errorf("check with %v = %v, %v; want %v", c.fields, got, err, c.want)
		}
	}

	// Literals converted for field writes evaluate to the converted value.
	s, _ = ParseStatement(`create Person { name: "A", email: "e", joined: "2026-01-02" }`)
	info, err = Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	v, _ = fx.evaluator(info).Eval(s.(*Create).Inits[2].Value, nil)
	if v.String() != `d"2026-01-02"` {
		t.Errorf("joined = %v", v)
	}

	// Show columns are read from the row after show.
	s, _ = ParseStatement(`find Person p | show p.name | sort p.name`)
	info, err = Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	key := s.(*Pipeline).Steps[1].(*Sort).Keys[0].Expr
	v, _ = fx.evaluator(info).Eval(key, &Frame{Row: Row{"p.name": value.MustText("Zed")}})
	if v.String() != `"Zed"` {
		t.Errorf("column = %v", v)
	}

	// Paths are lists alternating nodes and relations.
	s, _ = ParseStatement(`find Person p | follow path pth = p ~knows*~> f | show len(pth), nodes(pth), rels(pth)`)
	info, err = Check(fx.env, s)
	if err != nil {
		t.Fatal(err)
	}
	r1 := value.NewIDGenerator(2).New()
	path := mustV(value.NewList(value.Ref{Target: fx.ann}, r1, value.Ref{Target: fx.bob}))
	ev = fx.evaluator(info)
	var got []string
	for _, it := range s.(*Pipeline).Steps[1].(*Show).Items {
		v, err := ev.Eval(it.Expr, &Frame{Row: Row{"pth": path}})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v.String())
	}
	if got[0] != "1" || !strings.HasPrefix(got[1], "[#") || strings.Count(got[1], "#") != 2 || strings.Count(got[2], "#") != 1 {
		t.Errorf("path functions: %v", got)
	}
}

// FuzzEval checks and evaluates random expressions with p bound to an
// entity: evaluation must not panic, return nil, or fail without a code.
func FuzzEval(f *testing.F) {
	for _, s := range []string{
		`p.config->server->port + 1`, `p.inv->lines->[0]->qty * 2`, `cast(p.name as int)`,
		`round(-1250, -2)`, `1d / 3 * 3`, `[1, 2] ++ p.tags`, `p.page->server->@port`,
		`add_months(today(), -13)`, `pow(2n, 70)`, `substr("héllo", -1, 9)`, `{"a": 1} -- ["a"]`,
		`cast(1e300 as decimal)`, `p.manager.manager.name`, `slice([1], 5, 2)`,
	} {
		f.Add(s)
	}
	fx := newEvalFixture(&testing.T{})
	f.Fuzz(func(t *testing.T, src string) {
		e, err := ParseExpr(src)
		if err != nil {
			return
		}
		info, _, err := CheckExpr(fx.env, e, map[string]Static{"p": fx.env.Handles["alice"]})
		if err != nil {
			return
		}
		v, err := fx.evaluator(info).Eval(e, &Frame{Row: Row{"p": value.Ref{Target: fx.ann}}})
		if err != nil {
			if ce, ok := err.(*Error); !ok || ce.Code == "" || ce.Code == "internal" {
				t.Fatalf("%s: bad error %#v", src, err)
			}
			return
		}
		if v == nil {
			t.Fatalf("%s: nil value", src)
		}
	})
}
