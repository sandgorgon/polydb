package catalog

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/mem"
	"github.com/sandgorgon/polydb/value"
)

func fd(name, typ string) model.FieldDef {
	return model.FieldDef{Name: name, Type: value.MustParseType(typ)}
}

// commit commits e into s and checks the stored catalog loads back equal.
func commit(t *testing.T, s *mem.Store, e *Edit) *Catalog {
	t.Helper()
	var b storage.Batch
	c, err := e.Commit(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(&b); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(s, c.Version())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dump(loaded), dump(c); got != want {
		t.Fatalf("loaded catalog differs:\n got %s\nwant %s", got, want)
	}
	return c
}

// dump describes a catalog's contents for comparison.
func dump(c *Catalog) string {
	var b strings.Builder
	fields := func(fs []model.FieldDef) {
		for _, f := range fs {
			b.WriteString(" " + f.Name + ":" + f.Type.String())
			if f.Doc != "" {
				b.WriteString(" " + value.QuoteText(f.Doc))
			}
			if f.Unique {
				b.WriteString(" unique")
			}
			if f.Default != "" {
				b.WriteString(" default " + f.Default)
			}
			if f.Column != nil {
				b.WriteString(" at " + strconv.Itoa(*f.Column))
			}
			if f.DefaultValue != nil {
				b.WriteString(" = " + f.DefaultValue.String())
			}
		}
	}
	for _, k := range c.Kinds() {
		b.WriteString("\nkind " + k.Name + " " + k.ID.String())
		if k.Closed {
			b.WriteString(" closed")
		}
		fields(k.Fields)
	}
	for _, r := range c.RelationTypes() {
		b.WriteString("\nrelation " + r.Name + " " + r.ID.String())
		if r.Defined {
			b.WriteString(" defined")
		}
		b.WriteString(" " + r.From.Kind.String() + " " + r.To.Kind.String())
		fields(r.Props)
	}
	for _, f := range c.Formats() {
		b.WriteString("\nformat " + f.Name + " : " + f.Base)
		if f.Header {
			b.WriteString(" header")
		}
		fields(f.Fields)
	}
	for _, ix := range c.Indexes() {
		b.WriteString("\nindex " + ix.Name + " on " + ix.Kind.String())
		for _, k := range ix.Keys {
			b.WriteString(" " + k.Field)
			for _, s := range k.Path {
				b.WriteString("->" + s.Name)
			}
		}
	}
	return b.String()
}

func TestDefineAndLoad(t *testing.T) {
	s := mem.New()
	ids := value.NewIDGenerator(0)
	e := New().Edit(ids)

	email := fd("email", "text")
	email.Unique = true
	email.Doc = "Login email"
	joined := fd("joined", "date")
	joined.Default = "today()"
	person, err := e.DefineKind(model.Kind{Name: "Person", Doc: "A human being", Fields: []model.FieldDef{
		fd("name", "text"), email, joined, fd("manager", "ref<Person>?"), fd("tags", "set<text>"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DefineFormat(model.Format{Name: "invoice", Base: "json", Fields: []model.FieldDef{
		fd("number", "text"), fd("total", "decimal(12,2)"),
	}}); err != nil {
		t.Fatal(err)
	}
	four := 4
	status := fd("status", "text")
	status.Default, status.DefaultValue = `"active"`, value.MustText("active")
	email4 := fd("email", "text")
	email4.Column = &four
	if _, err := e.DefineFormat(model.Format{Name: "staff", Base: "csv", Header: true, Fields: []model.FieldDef{
		fd("name", "text"), email4, fd("age", "int?"), status,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DefineFormat(model.Format{Name: "contacts", Base: "csv", Header: true}); err != nil {
		t.Fatal(err)
	}
	order, err := e.DefineKind(model.Kind{Name: "Order", Closed: true, Fields: []model.FieldDef{
		fd("customer", "ref<Person>"), fd("invoice", "doc<invoice>?"), fd("raw", "doc<json>?"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DefineRelationType(model.RelationType{Name: "placed_by",
		From: model.Endpoint{Mode: model.EndpointKind, Kind: order}, To: model.Endpoint{Mode: model.EndpointKind, Kind: person},
		MaxOut: 1, Props: []model.FieldDef{fd("at", "time")}}); err != nil {
		t.Fatal(err)
	}
	knows, err := e.EnsureRelationType("knows")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.EnsureRelationType("knows"); again != knows {
		t.Error("EnsureRelationType made a second type")
	}
	if _, err := e.DefineIndex(model.Index{Name: "person_email", Kind: person, Unique: true,
		Keys: []model.IndexKey{{Field: "email"}}}); err != nil {
		t.Fatal(err)
	}
	c := commit(t, s, e)
	if c.Version() != 1 {
		t.Errorf("version %d", c.Version())
	}
	if f, ok := c.Format("invoice"); !ok || f.Name() != "invoice" {
		t.Error("derived format not available")
	}
	if _, ok := c.Format("json"); !ok {
		t.Error("base format not available")
	}
	staff, _ := c.Format("staff")
	if v, err := staff.Parse("name,age,x,status,mail\nAnn,,y,,a@x.io\n"); err != nil ||
		v.String() != `[{name: "Ann", email: "a@x.io", age: null, status: "active"}]` {
		t.Errorf("staff table: %v, %v", v, err)
	}
	if k, _ := c.Kind("Order"); !k.Closed {
		t.Error("closed flag lost")
	}
	if got := len(c.IndexesOn(person)); got != 1 {
		t.Errorf("IndexesOn = %d", got)
	}

	// Defining a type that was registered by use keeps its ID.
	e = c.Edit(ids)
	id, err := e.DefineRelationType(model.RelationType{Name: "knows", Doc: "Acquaintance"})
	if err != nil || id != knows {
		t.Errorf("define knows: %v, %v", id, err)
	}
	if _, err := e.DefineRelationType(model.RelationType{Name: "knows"}); err == nil {
		t.Error("second definition accepted")
	}
	c = commit(t, s, e)
	if r, _ := c.RelationType("knows"); !r.Defined || r.Doc != "Acquaintance" {
		t.Errorf("knows: %+v", r)
	}
}

func TestDefinitionErrors(t *testing.T) {
	ids := value.NewIDGenerator(0)
	e := New().Edit(ids)
	person, _ := e.DefineKind(model.Kind{Name: "Person", Fields: []model.FieldDef{fd("name", "text"), fd("bio", "doc<json>?"), fd("tags", "list<text>")}})
	cases := map[string]func() error{
		"no kind City": func() error {
			_, err := e.DefineKind(model.Kind{Name: "Addr", Fields: []model.FieldDef{fd("city", "ref<City>")}})
			return err
		},
		"no format nope": func() error {
			_, err := e.DefineKind(model.Kind{Name: "Doc", Fields: []model.FieldDef{fd("d", "doc<nope>")}})
			return err
		},
		"already exists": func() error {
			_, err := e.DefineKind(model.Kind{Name: "Person"})
			return err
		},
		"format json already exists": func() error {
			_, err := e.DefineFormat(model.Format{Name: "json", Base: "xml", Fields: []model.FieldDef{fd("a", "int")}})
			return err
		},
		"no base format": func() error {
			_, err := e.DefineFormat(model.Format{Name: "f", Base: "yaml", Fields: []model.FieldDef{fd("a", "int")}})
			return err
		},
		"has no field age": func() error {
			_, err := e.DefineIndex(model.Index{Name: "i", Kind: person, Keys: []model.IndexKey{{Field: "age"}}})
			return err
		},
		"can't be indexed": func() error {
			_, err := e.DefineIndex(model.Index{Name: "i", Kind: person, Keys: []model.IndexKey{{Field: "tags"}}})
			return err
		},
		"not a doc": func() error {
			_, err := e.DefineIndex(model.Index{Name: "i", Kind: person, Keys: []model.IndexKey{{Field: "name", Path: []model.PathStep{{Name: "x"}}}}})
			return err
		},
		"endpoint kind": func() error {
			_, err := e.DefineRelationType(model.RelationType{Name: "r", From: model.Endpoint{Mode: model.EndpointKind, Kind: ids.New()}})
			return err
		},
	}
	for want, fn := range cases {
		if err := fn(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want error containing %q", err, want)
		}
	}
	if _, err := e.DefineIndex(model.Index{Name: "bio_port", Kind: person, Keys: []model.IndexKey{{Field: "bio", Path: []model.PathStep{{Name: "port"}}}}}); err != nil {
		t.Errorf("doc path index: %v", err)
	}
}

func TestRenameAndDrop(t *testing.T) {
	s := mem.New()
	ids := value.NewIDGenerator(0)
	e := New().Edit(ids)
	person, _ := e.DefineKind(model.Kind{Name: "Person", Fields: []model.FieldDef{fd("boss", "ref<Person>?")}})
	if _, err := e.DefineKind(model.Kind{Name: "Team", Fields: []model.FieldDef{fd("lead", "ref<Person>"), fd("members", "list<ref<Person>>")}}); err != nil {
		t.Fatal(err)
	}
	c := commit(t, s, e)

	// Rename: every ref<Person> follows, but only Person's own definition
	// is rewritten in storage.
	e = c.Edit(ids)
	k, _ := c.Kind("Person")
	renamed := *k
	renamed.Name = "Human"
	if err := e.ReplaceKind(renamed); err != nil {
		t.Fatal(err)
	}
	var b storage.Batch
	c2, err := e.Commit(&b)
	if err != nil {
		t.Fatal(err)
	}
	if b.Len() != 1 {
		t.Errorf("rename wrote %d definitions, want 1", b.Len())
	}
	s.Apply(&b)
	c2, _ = Load(s, c2.Version())
	team, _ := c2.Kind("Team")
	if team.Fields[0].Type.String() != "ref<Human>" || team.Fields[1].Type.String() != "list<ref<Human>>" {
		t.Errorf("Team after rename: %s, %s", team.Fields[0].Type, team.Fields[1].Type)
	}
	if h, _ := c2.Kind("Human"); h.ID != person || h.Fields[0].Type.String() != "ref<Human>?" {
		t.Errorf("Human: %+v", h)
	}
	if _, ok := c2.Kind("Person"); ok {
		t.Error("old name still present")
	}
	// The original snapshot is unchanged.
	if team, _ := c.Kind("Team"); team.Fields[0].Type.String() != "ref<Person>" {
		t.Error("old snapshot changed by the edit")
	}

	// Drop: blocked while Team refers to Human.
	e = c2.Edit(ids)
	if err := e.DropKind("Human"); err == nil || !strings.Contains(err.Error(), "kind Team") {
		t.Errorf("drop Human: %v", err)
	}
	if err := e.DropKind("Team"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropKind("Human"); err != nil {
		t.Errorf("drop Human after Team: %v", err)
	}
	c3 := commit(t, s, e)
	if len(c3.Kinds()) != 0 {
		t.Errorf("kinds left: %d", len(c3.Kinds()))
	}
}

func TestStoredTypes(t *testing.T) {
	ids := value.NewIDGenerator(0)
	e := New().Edit(ids)
	e.DefineKind(model.Kind{Name: "City"})
	e.DefineFormat(model.Format{Name: "invoice", Base: "json", Fields: []model.FieldDef{fd("n", "int")}})
	c := e.Catalog()
	ty := value.MustParseType("record{at: ref<City>, inv: doc<invoice>, raw: doc<json>, any: ref}")
	b, err := c.EncodeType(ty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "City") || strings.Contains(string(b), "invoice") || !strings.Contains(string(b), "json") {
		t.Error("stored type should use IDs for kinds and derived formats only")
	}
	if got, err := c.DecodeType(b); err != nil || !got.Equal(ty) {
		t.Errorf("decoded %v, %v", got, err)
	}
	if _, err := c.EncodeType(value.MustParseType("ref<Nope>")); err == nil {
		t.Error("unknown kind encoded")
	}
	// After City is dropped, a stored ref<City> reads as a plain ref.
	e2 := c.Edit(ids)
	e2.DropKind("City")
	if got, err := e2.Catalog().DecodeType(b); err != nil || got.Fields[0].Type.String() != "ref" {
		t.Errorf("after drop: %v, %v", got, err)
	}
}

func TestAlterAndDropOthers(t *testing.T) {
	s := mem.New()
	ids := value.NewIDGenerator(0)
	e := New().Edit(ids)
	if e.Changed() {
		t.Error("new edit reports changes")
	}
	if _, err := e.DefineFormat(model.Format{Name: "resume", Base: "json", Fields: []model.FieldDef{fd("years", "int")}}); err != nil {
		t.Fatal(err)
	}
	person, err := e.DefineKind(model.Kind{Name: "Person", Fields: []model.FieldDef{fd("age", "int"), fd("cv", "doc<resume>?")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DefineIndex(model.Index{Name: "person_age", Kind: person, Keys: []model.IndexKey{{Field: "age"}}}); err != nil {
		t.Fatal(err)
	}
	rel, _ := e.DefineRelationType(model.RelationType{Name: "likes", Props: []model.FieldDef{fd("since", "int?")}})
	c := commit(t, s, e)

	e = c.Edit(ids)
	// A kind change that drops an indexed field is refused.
	k, _ := c.Kind("Person")
	noAge := *k
	noAge.Fields = noAge.Fields[1:]
	if err := e.ReplaceKind(noAge); err == nil || !strings.Contains(err.Error(), "drop the index first") {
		t.Errorf("dropping indexed field: %v", err)
	}
	// The format is in use until the kind stops using it.
	if err := e.DropFormat("resume"); err == nil || !strings.Contains(err.Error(), "kind Person") {
		t.Errorf("drop used format: %v", err)
	}
	if err := e.DropIndex("person_age"); err != nil {
		t.Fatal(err)
	}
	noCV := *k
	noCV.Fields = noCV.Fields[:1]
	if err := e.ReplaceKind(noCV); err != nil {
		t.Fatal(err)
	}
	if err := e.DropFormat("resume"); err != nil {
		t.Errorf("drop unused format: %v", err)
	}
	r, _ := c.RelationTypeByID(rel)
	r2 := *r
	r2.Name, r2.MaxIn = "fancies", 3
	if err := e.ReplaceRelationType(r2); err != nil {
		t.Fatal(err)
	}
	c = commit(t, s, e)
	if _, ok := c.FormatDef("resume"); ok || len(c.Formats()) != 0 || len(c.Indexes()) != 0 {
		t.Error("format or index not dropped")
	}
	if r, ok := c.RelationType("fancies"); !ok || r.ID != rel || r.MaxIn != 3 {
		t.Errorf("renamed relation: %+v", r)
	}
	if _, ok := c.RelationType("likes"); ok {
		t.Error("old relation name still present")
	}

	e = c.Edit(ids)
	if err := e.DropRelationType("fancies"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropRelationType("fancies"); err == nil {
		t.Error("dropped twice")
	}
	c = commit(t, s, e)
	if len(c.RelationTypes()) != 0 {
		t.Error("relation type not dropped")
	}
}
