package value

import (
	"strings"
	"testing"
)

func TestParseTypeRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"int", "int"},
		{"text?", "text?"},
		{"decimal", "decimal"},
		{"decimal(12,2)", "decimal(12,2)"},
		{"decimal( 12 , 2 )?", "decimal(12,2)?"},
		{"ref", "ref"},
		{"ref<Person>", "ref<Person>"},
		{"doc<invoice>", "doc<invoice>"},
		{"list<int>", "list<int>"},
		{"list<any>", "list<any>"},
		{"array<float, 3>", "array<float, 3>"},
		{"set<text>", "set<text>"},
		{"map<text, list<float>>", "map<text, list<float>>"},
		{"pair<text, int>", "pair<text, int>"},
		{"queue.fifo<text>", "queue.fifo<text>"},
		{"queue.lifo<int, max 10>", "queue.lifo<int, max 10>"},
		{"queue.ring<text, 100>", "queue.ring<text, 100>"},
		{"queue.priority<text>", "queue.priority<text>"},
		{"queue.priority<text, high>", "queue.priority<text>"},
		{"queue.priority<text, low>", "queue.priority<text, low>"},
		{"queue.priority<text, low, max 5>", "queue.priority<text, low, max 5>"},
		{"queue.priority<text, max 5>", "queue.priority<text, max 5>"},
		{"record{ x: float, y: float }", "record{x: float, y: float}"},
		{`record{ sku: text "Stock unit" qty: int }`, "record{sku: text, qty: int}"},
		{"union<int | text>?", "union<int | text>?"},
		{"queue.fifo<ref<Task>>", "queue.fifo<ref<Task>>"},
		{"list<`odd name`>", ""}, // invalid: not a type name, checked below
	}
	for _, c := range cases {
		got, err := ParseType(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("ParseType(%q) = %s, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseType(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("ParseType(%q).String() = %q, want %q", c.in, got, c.want)
		}
		again, err := ParseType(got.String())
		if err != nil || !again.Equal(got) {
			t.Errorf("round trip of %q failed: %v", c.in, err)
		}
	}
}

func TestParseTypeRecordDoc(t *testing.T) {
	ty := MustParseType(`record{ sku: text doc "Stock unit", qty: int }`)
	if ty.Fields[0].Doc != "Stock unit" {
		t.Errorf("doc = %q", ty.Fields[0].Doc)
	}
}

func TestParseTypeErrors(t *testing.T) {
	cases := map[string]string{
		"":                         "expected a type",
		"integer":                  "unknown type",
		"any?":                     "any? is not allowed",
		"decimal(0,0)":             "precision must be at least 1",
		"decimal(5,6)":             "scale 6 out of range",
		"decimal(1001,0)":          "out of range",
		"array<int, 0>":            "out of range",
		"queue.ring<int, 0>":       "out of range",
		"set<list<int>>":           "not keyable",
		"map<list<int>, int>":      "not keyable",
		"map<text, int":            `expected ">"`,
		"record{}":                 "at least one field",
		"record{a: int, a: text}":  "duplicate record field",
		"union<int>":               "at least two",
		"union<int | int>":         "duplicate union",
		"union<int? | text>":       "may not be optional",
		"union<int | any>":         "not allowed",
		"queue.stack<int>":         "unknown queue kind",
		"queue.fifo<int, max 0>":   "at least 1",
		"queue.priority<int, mid>": "expected max",
		"list<int> extra":          "after type",
		"ref<1x>":                  "expected a name",
		"ref<`bad name`>":          "invalid kind name",
	}
	for in, want := range cases {
		_, err := ParseType(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseType(%q) error = %v, want containing %q", in, err, want)
		}
	}
}

func TestNestingLimit(t *testing.T) {
	s := strings.Repeat("list<", MaxNesting) + "int" + strings.Repeat(">", MaxNesting)
	if _, err := ParseType(s); err == nil {
		t.Error("expected nesting error")
	}
	s = strings.Repeat("list<", MaxNesting-1) + "int" + strings.Repeat(">", MaxNesting-1)
	if _, err := ParseType(s); err != nil {
		t.Errorf("nesting at the limit: %v", err)
	}
}

func TestKeyable(t *testing.T) {
	yes := []string{"int", "text", "pair<int, text>", "array<id, 2>", "record{a: int}", "union<int | text>"}
	no := []string{"list<int>", "image", "doc<json>", "map<text, int>", "any", "pair<int, list<int>>"}
	for _, s := range yes {
		if !MustParseType(s).Keyable() {
			t.Errorf("%s should be keyable", s)
		}
	}
	for _, s := range no {
		if MustParseType(s).Keyable() {
			t.Errorf("%s should not be keyable", s)
		}
	}
}

func TestIsIdentifier(t *testing.T) {
	for _, s := range []string{"a", "_x", "Person", "first_name", "名前", "café", "x1"} {
		if !IsIdentifier(s) {
			t.Errorf("IsIdentifier(%q) = false", s)
		}
	}
	decomposed := "café" // not NFC
	for _, s := range []string{"", "1x", "first name", "a-b", decomposed, strings.Repeat("a", MaxNameLen+1)} {
		if IsIdentifier(s) {
			t.Errorf("IsIdentifier(%q) = true", s)
		}
	}
}
