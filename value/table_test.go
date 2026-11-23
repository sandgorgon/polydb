package value

import (
	"strings"
	"testing"
)

func tableFormat(t *testing.T, base string, spec TableSpec) *DerivedFormat {
	t.Helper()
	b, _ := LookupFormat(base)
	f, err := NewTableFormat("t", b, spec)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func col(name, typ string, pos int, def Value) Column {
	return Column{Name: name, Type: MustParseType(typ), Pos: pos, Default: def}
}

func TestTableFormat(t *testing.T) {
	people := TableSpec{Header: true, Columns: []Column{
		col("name", "text", -1, nil),
		col("email", "text", 4, nil),
		col("age", "int?", -1, nil),
		col("status", "text", -1, MustText("active")),
		col("joined", "date?", -1, nil),
		col("score", "decimal(5,1)", -1, DecimalFromInt(0)),
	}}
	cases := []struct {
		name string
		spec TableSpec
		base string
		text string
		want string
	}{
		{"header and typed columns", people, "csv",
			"\ufeffname, age ,status,joined,mail,score,extra\nAnn,34,,2026-01-02,a@x.io,1.25,zzz\nBob,,away,,b@x.io,,\n",
			`[{name: "Ann", email: "a@x.io", age: 34, status: "active", joined: d"2026-01-02", score: 1.2d}, ` +
				`{name: "Bob", email: "b@x.io", age: null, status: "away", joined: null, score: 0.0d}]`},
		{"header only", TableSpec{Header: true}, "csv",
			"a,b\n1,2\n3,4\n", `[{"a": "1", "b": "2"}, {"a": "3", "b": "4"}]`},
		{"plain rows", TableSpec{}, "csv", "1,2\n3,4\n", `[["1", "2"], ["3", "4"]]`},
		{"columns by order", TableSpec{Columns: []Column{col("x", "int", -1, nil), col("y", "float", -1, nil)}}, "csv",
			"1,2.5\n3,4\n", `[{x: 1, y: 2.5}, {x: 3, y: 4.0}]`},
		{"columns by position", TableSpec{Columns: []Column{col("y", "bool", 1, nil), col("id", "text", 0, nil)}}, "tsv",
			"a\tTRUE\nb\tfalse\n", `[{y: true, id: "a"}, {y: false, id: "b"}]`},
		{"missing optional column", TableSpec{Header: true, Columns: []Column{col("a", "text", -1, nil), col("b", "int?", -1, nil), col("c", "int", -1, Int(7))}}, "csv",
			"a\nx\n", `[{a: "x", b: null, c: 7}]`},
		{"empty document", people, "csv", "", `[]`},
		{"any column", TableSpec{Columns: []Column{col("a", "any", -1, nil)}}, "csv", "x\n\"\"\n", `[{a: "x"}, {a: null}]`},
	}
	for _, c := range cases {
		f := tableFormat(t, c.base, c.spec)
		v, err := f.Parse(c.text)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if v.String() != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, v, c.want)
		}
		if _, err := Conform(v, f.Shape()); err != nil {
			t.Errorf("%s: parsed value doesn't match the shape %s: %v", c.name, f.Shape(), err)
		}
	}
}

func TestTableFormatErrors(t *testing.T) {
	people := TableSpec{Header: true, Columns: []Column{col("name", "text", -1, nil), col("age", "int", -1, nil)}}
	cases := []struct {
		spec TableSpec
		text string
		want string
	}{
		{people, "name\nAnn\n", "the header has no column age"},
		{people, "name,age\nAnn,abc\n", `row 1, column age: "abc" isn't a int`},
		{people, "name,age\nAnn,3\nBob,\n", "row 2, column age: the cell is empty"},
		{people, "name,age,age\nAnn,3,4\n", "column age appears more than once in the header; pick one with at"},
		{TableSpec{Columns: []Column{col("a", "int", 3, nil)}}, "1,2\n", "row 1, column a: the row has no such column"},
		{TableSpec{Header: true}, "a,a\n1,2\n", "duplicate"},
	}
	for _, c := range cases {
		_, err := tableFormat(t, "csv", c.spec).Parse(c.text)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.text, err, c.want)
		}
	}
	csv, _ := LookupFormat("csv")
	for _, bad := range []struct {
		spec TableSpec
		want string
	}{
		{TableSpec{Columns: []Column{col("a", "list<int>", -1, nil)}}, "a column holds"},
		{TableSpec{Columns: []Column{col("a", "int", -1, nil), col("a", "int", -1, nil)}}, "declared twice"},
		{TableSpec{Columns: []Column{col("a", "int", -1, nil), col("b", "int", 0, nil)}}, "both read column 0"},
		{TableSpec{Columns: []Column{col("a", "int", -1, MustText("x"))}}, "default"},
		{TableSpec{Columns: []Column{col("a", "int", -2, nil)}}, "negative"},
	} {
		if _, err := NewTableFormat("t", csv, bad.spec); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: got %v, want %q", bad.spec, err, bad.want)
		}
	}
}

func TestParseText(t *testing.T) {
	for _, c := range []struct{ s, typ, want string }{
		{" 42 ", "int", "42"},
		{"12345678901234567890123", "bigint", "12345678901234567890123n"},
		{"1e3", "float", "1000.0"},
		{"-1.50", "decimal", "-1.50d"},
		{"True", "bool", "true"},
		{"2026-01-02", "date", `d"2026-01-02"`},
		{"2026-01-02T03:04:05Z", "time", `t"2026-01-02T03:04:05Z"`},
		{"1h30m", "duration", "1h30m0s"},
		{" padded ", "text", `" padded "`},
	} {
		v, err := ParseText(c.s, MustParseType(c.typ))
		if err != nil || v.String() != c.want {
			t.Errorf("ParseText(%q, %s) = %v, %v; want %s", c.s, c.typ, v, err, c.want)
		}
	}
}
