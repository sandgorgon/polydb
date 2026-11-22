package model

import (
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/value"
)

func fd(name, typ string) FieldDef { return FieldDef{Name: name, Type: value.MustParseType(typ)} }

func TestValidateFieldDef(t *testing.T) {
	uniq := fd("a", "int")
	uniq.Unique = true
	uniqList := fd("a", "list<int>")
	uniqList.Unique = true
	def := fd("a", "int")
	def.Default = "0"
	restrictRef := fd("m", "ref<Person>?")
	restrictRef.OnDelete = RefRestrict
	restrictInt := fd("m", "int")
	restrictInt.OnDelete = RefRestrict
	check := fd("a", "int")
	check.Check = "a > 0"
	keepOff := fd("a", "int")
	keepOff.History.Keep = 5

	cases := []struct {
		fd   FieldDef
		ctx  FieldContext
		want string // "" means valid
	}{
		{fd("name", "text"), KindField, ""},
		{fd("1x", "text"), KindField, "invalid field name"},
		{FieldDef{Name: "a"}, KindField, "no type"},
		{FieldDef{Name: "a", Type: value.SetOf(value.ListOf(value.TypeInt))}, KindField, "not keyable"},
		{uniq, KindField, ""},
		{uniq, AddedField, "unique isn't allowed"},
		{uniqList, KindField, "keyable"},
		{def, KindField, ""},
		{def, RelationProp, "default isn't allowed"},
		{restrictRef, KindField, ""},
		{restrictInt, KindField, "only applies to ref"},
		{check, AddedField, ""},
		{check, FormatField, "other than doc"},
		{keepOff, KindField, "history"},
	}
	for _, c := range cases {
		err := ValidateFieldDef(&c.fd, c.ctx)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s in %s: got %v, want %q", c.fd.Name, c.ctx, err, c.want)
		}
	}
}

func TestValidateDefinitions(t *testing.T) {
	k := Kind{Name: "Person", Fields: []FieldDef{fd("name", "text"), fd("name", "int")}}
	if err := k.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Errorf("duplicate field: %v", err)
	}
	r := RelationType{Name: "knows", MaxOut: 1}
	if err := r.Validate(); err == nil {
		t.Error("undefined relation type with options accepted")
	}
	r.Defined = true
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
	r.From = Endpoint{Mode: EndpointKind}
	if err := r.Validate(); err == nil {
		t.Error("kind endpoint without kind accepted")
	}
	f := Format{Name: "invoice", Base: "json"}
	if err := f.Validate(); err == nil {
		t.Error("format without fields accepted")
	}
	ix := Index{Name: "i", Kind: value.ID{1}, Text: true, Keys: []IndexKey{{Field: "a"}, {Field: "b"}}}
	if err := ix.Validate(); err == nil {
		t.Error("two-field text index accepted")
	}
	ix = Index{Name: "i", Kind: value.ID{1}, Keys: []IndexKey{{Field: "cfg", Path: []PathStep{{Name: "server"}, {IsPos: true, Pos: 2}, {Name: "port", Attr: true}}}}}
	if err := ix.Validate(); err != nil {
		t.Error(err)
	}
}

func TestCheckAddField(t *testing.T) {
	k := &Kind{Name: "Person", Fields: []FieldDef{fd("name", "text")}}
	has := func(n string) bool { return n == "name" || n == "hobby" }
	hobby, nick := fd("hobby", "text"), fd("nick", "text")
	name := fd("name", "text")
	if err := CheckAddField(k, has, &nick); err != nil {
		t.Error(err)
	}
	if err := CheckAddField(k, has, &hobby); err == nil {
		t.Error("existing added field accepted")
	}
	if err := CheckAddField(k, has, &name); err == nil || !strings.Contains(err.Error(), "declared") {
		t.Errorf("declared field: %v", err)
	}
	k.Closed = true
	if err := CheckAddField(k, has, &nick); err != ErrClosedKind {
		t.Errorf("closed kind: %v", err)
	}
	if err := CheckAddField(nil, has, &nick); err != nil {
		t.Errorf("kindless: %v", err)
	}
}

func TestSuggest(t *testing.T) {
	names := []string{"email", "name", "age", "joined"}
	for in, want := range map[string]string{"emial": "email", "nmae": "name", "ag": "age", "joind": "joined", "xyz": "", "address": ""} {
		if got := Suggest(in, names); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", in, got, want)
		}
	}
	if err := UnknownField("emial", names); !strings.Contains(err.Error(), `did you mean "email"`) {
		t.Errorf("got %v", err)
	}
}
