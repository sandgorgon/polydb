package keys

import (
	"bytes"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/mem"
	"github.com/sandgorgon/polydb/value"
)

// plainTypes stores types as they are, without catalog IDs.
type plainTypes struct{}

func (plainTypes) EncodeType(t *value.Type) ([]byte, error) { return value.AppendType(nil, t), nil }
func (plainTypes) DecodeType(b []byte) (*value.Type, error) { return value.DecodeType(b) }

func TestKeyParsing(t *testing.T) {
	g := value.NewIDGenerator(0)
	a, b, c, d := g.New(), g.New(), g.New(), g.New()
	if e, n, err := ParseField(Field(a, "名前")); err != nil || e != a || n != "名前" {
		t.Errorf("ParseField: %v %q %v", e, n, err)
	}
	if k, e, err := ParseMember(Member(a, b)); err != nil || k != a || e != b {
		t.Error("ParseMember")
	}
	edge, err := ParseEdge(In(a, b, c, d))
	if err != nil || edge.Dir != PrefixIn || edge.Node != a || edge.Type != b || edge.Other != c || edge.Relation != d {
		t.Errorf("ParseEdge: %+v %v", edge, err)
	}
	if typ, id, err := ParseCatalog(Catalog('k', a)); err != nil || typ != 'k' || id != a {
		t.Error("ParseCatalog")
	}
	for _, bad := range [][]byte{nil, {PrefixField}, Entity(a), append(Out(a, b, c, d), 0)} {
		if _, err := ParseEdge(bad); err == nil {
			t.Errorf("ParseEdge(%x) accepted", bad)
		}
	}
	if !bytes.HasPrefix(Out(a, b, c, d), Adjacent(PrefixOut, a, b)) || !bytes.HasPrefix(Field(a, "x"), Fields(a)) {
		t.Error("prefix helpers")
	}
}

func TestRecords(t *testing.T) {
	g := value.NewIDGenerator(0)
	now := must(value.NewTime(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)))
	e := &model.Entity{ID: g.New(), Kind: g.New(), Meta: model.Meta{
		Created: now, Updated: now, Version: 3, Owner: "ann",
		Labels: must(value.NewSet(value.MustText("vip"))),
		Props:  must(value.MapOfText(map[string]value.Value{"source": value.MustText("import")})),
	}}
	got, err := DecodeEntity(e.ID, EncodeEntity(e))
	if err != nil || got.Kind != e.Kind || got.Meta.Version != 3 || got.Meta.Owner != "ann" ||
		!value.Equal(got.Meta.Labels, e.Meta.Labels) || !value.Equal(got.Meta.Props, e.Meta.Props) ||
		!value.Equal(got.Meta.Created, now) {
		t.Errorf("entity: %+v, %v", got, err)
	}
	kindless := &model.Entity{ID: g.New()}
	if got, err := DecodeEntity(kindless.ID, EncodeEntity(kindless)); err != nil || !got.Kind.IsZero() {
		t.Errorf("kindless entity: %+v, %v", got, err)
	}

	declared := &model.Field{ID: g.New(), Entity: e.ID, Name: "age", Value: value.Int(34), Declared: true}
	added := &model.Field{ID: g.New(), Entity: e.ID, Name: "hobby", Value: value.MustText("chess"),
		Def: &model.FieldDef{Name: "hobby", Type: value.MustParseType("text?"), Doc: "Main hobby", Check: "len(hobby) > 0",
			History: model.History{On: true, Keep: time.Hour}}}
	for _, f := range []*model.Field{declared, added} {
		b, err := EncodeField(f, plainTypes{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeField(f.Entity, f.Name, b, plainTypes{})
		if err != nil || got.ID != f.ID || got.Declared != f.Declared || !value.Equal(got.Value, f.Value) {
			t.Errorf("field %s: %+v, %v", f.Name, got, err)
			continue
		}
		if f.Def != nil && (got.Def.Doc != "Main hobby" || got.Def.Type.String() != "text?" ||
			got.Def.Check != f.Def.Check || got.Def.History != f.Def.History) {
			t.Errorf("field def: %+v", got.Def)
		}
	}
	if _, err := EncodeField(&model.Field{Name: "x", Value: value.Null}, plainTypes{}); err == nil {
		t.Error("added field without definition encoded")
	}

	rel := &model.Relation{ID: g.New(), Type: g.New(), From: e.ID, To: g.New(),
		Props: must(value.MapOfText(map[string]value.Value{"since": value.Int(2019)}))}
	if got, err := DecodeRelation(rel.ID, EncodeRelation(rel)); err != nil || got.From != rel.From || got.To != rel.To ||
		got.Type != rel.Type || !value.Equal(got.Props, rel.Props) {
		t.Errorf("relation: %+v, %v", got, err)
	}

	for _, n := range []NodeRef{{Kind: model.NodeEntity}, {Kind: model.NodeRelation}, {Kind: model.NodeField, Entity: e.ID, Name: "age"}} {
		if got, err := DecodeNode(EncodeNode(n)); err != nil || got != n {
			t.Errorf("node %+v: %+v, %v", n, got, err)
		}
	}
	for _, bad := range [][]byte{nil, {9}, {byte(model.NodeEntity), 0}, {byte(model.NodeField)}} {
		if _, err := DecodeNode(bad); err == nil {
			t.Errorf("DecodeNode(%x) accepted", bad)
		}
	}
	if _, err := DecodeEntity(e.ID, value.Encode(value.Int(1))); err == nil {
		t.Error("bad entity record accepted")
	}
	if _, err := DecodeRelation(rel.ID, EncodeEntity(e)); err == nil {
		t.Error("entity record decoded as relation")
	}
}

// TestLayoutScans checks the scans the engine relies on: an entity's
// fields, a kind's members and a node's relations of one type.
func TestLayoutScans(t *testing.T) {
	g := value.NewIDGenerator(0)
	s := mem.New()
	kind, knows, likes := g.New(), g.New(), g.New()
	ents := []value.ID{g.New(), g.New(), g.New()}
	var b storage.Batch
	for _, e := range ents {
		b.Set(Member(kind, e), nil)
		for _, f := range []string{"a", "ab", "b"} {
			b.Set(Field(e, f), []byte(f))
		}
	}
	for i, typ := range []value.ID{knows, knows, likes} {
		r := g.New()
		b.Set(Out(ents[0], typ, ents[i], r), nil)
		b.Set(In(ents[i], typ, ents[0], r), nil)
	}
	if _, err := s.Apply(&b); err != nil {
		t.Fatal(err)
	}
	count := func(prefix []byte) int {
		it := s.Iter(Range(prefix))
		defer it.Close()
		n := 0
		for it.Next() {
			n++
		}
		return n
	}
	if n := count(Fields(ents[1])); n != 3 {
		t.Errorf("fields of one entity: %d", n)
	}
	if n := count(Members(kind)); n != 3 {
		t.Errorf("members: %d", n)
	}
	if n := count(Adjacent(PrefixOut, ents[0], knows)); n != 2 {
		t.Errorf("out knows: %d", n)
	}
	if n := count(Adjacent(PrefixOut, ents[0])); n != 3 {
		t.Errorf("out all: %d", n)
	}
	if n := count(Adjacent(PrefixIn, ents[2], likes)); n != 1 {
		t.Errorf("in likes: %d", n)
	}
}
