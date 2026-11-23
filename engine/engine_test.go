package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

func fd(name, typ string) model.FieldDef {
	return model.FieldDef{Name: name, Type: value.MustParseType(typ)}
}

func txt(s string) value.Text { return value.MustText(s) }

var clock = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func newDB(t *testing.T) *DB {
	t.Helper()
	eng, err := Open(Options{Now: func() time.Time { return clock }, MaxPopWait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	db, err := eng.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// update runs fn and fails the test on error.
func update(t *testing.T, db *DB, fn func(tx *Tx) error) {
	t.Helper()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

// wantCode runs fn in a transaction (rolled back) and checks its error code
// and message.
func wantCode(t *testing.T, db *DB, code Code, msg string, fn func(tx *Tx) error) {
	t.Helper()
	tx := db.Begin(TxOptions{})
	defer tx.Rollback()
	err := fn(tx)
	if CodeOf(err) != code || !strings.Contains(err.Error(), msg) {
		t.Errorf("got %v, want %s containing %q", err, code, msg)
	}
}

func definePerson(t *testing.T, db *DB) {
	t.Helper()
	email := fd("email", "text")
	email.Unique = true
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Person", Fields: []model.FieldDef{
			fd("name", "text"), email, fd("age", "int?"), fd("balance", "decimal(12,2)?"),
			fd("tags", "set<text>?"), fd("manager", "ref<Person>?"),
		}})
		return err
	})
}

func create(t *testing.T, db *DB, kind string, vals Fields, adds ...Add) value.ID {
	t.Helper()
	var id value.ID
	update(t, db, func(tx *Tx) error {
		var err error
		id, err = tx.Create(kind, vals, adds...)
		return err
	})
	return id
}

func get(t *testing.T, db *DB, id value.ID) *Entity {
	t.Helper()
	var e *Entity
	if err := db.View(func(tx *Tx) error {
		var err error
		e, err = tx.Get(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCreateAndUpdate(t *testing.T) {
	db := newDB(t)
	definePerson(t, db)
	ann := create(t, db, "Person", Fields{"name": txt("Ann"), "email": txt("ann@x.io"), "age": value.Int(34), "balance": value.Int(5)},
		Add{Def: model.FieldDef{Name: "hobby", Type: value.TypeText, Doc: "Main hobby"}, Value: txt("chess")})

	e := get(t, db, ann)
	if e.Kind.Name != "Person" || e.Meta.Version != 1 || !e.Meta.Created.Go().Equal(clock) {
		t.Errorf("entity: %+v %+v", e.Kind, e.Meta)
	}
	if got := e.Value("balance"); got.String() != "5.00d" {
		t.Errorf("balance = %s, want int converted to 5.00d", got)
	}
	hobby, _ := e.Field("hobby")
	if hobby.Declared || e.Def(hobby).Doc != "Main hobby" {
		t.Errorf("hobby: %+v", hobby)
	}
	if names := fieldNames(e); names != "name email age balance hobby" {
		t.Errorf("field order: %s", names)
	}

	update(t, db, func(tx *Tx) error { return tx.Set(ann, Fields{"age": value.Int(35), "hobby": txt("go")}) })
	update(t, db, func(tx *Tx) error { return tx.Unset(ann, "age", "hobby") })
	e = get(t, db, ann)
	if e.Value("age") != nil || e.Value("hobby") != nil || e.Meta.Version != 3 {
		t.Errorf("after unset: age=%v hobby=%v version=%d", e.Value("age"), e.Value("hobby"), e.Meta.Version)
	}
	if f, ok := e.Field("hobby"); !ok || f.ID != hobby.ID {
		t.Error("unset field lost its ID")
	}
	update(t, db, func(tx *Tx) error { return tx.DropField(ann, "hobby") })
	if _, ok := get(t, db, ann).Field("hobby"); ok {
		t.Error("hobby not dropped")
	}

	wantCode(t, db, CodeUnknownField, "did you mean email?", func(tx *Tx) error {
		_, err := tx.Create("Person", Fields{"name": txt("B"), "emial": txt("b@x.io")})
		return err
	})
	wantCode(t, db, CodeUnknownField, "no field", func(tx *Tx) error { return tx.Set(ann, Fields{"nope": value.Int(1)}) })
	wantCode(t, db, CodeConstraint, "Person.email is required", func(tx *Tx) error {
		_, err := tx.Create("Person", Fields{"name": txt("B")})
		return err
	})
	wantCode(t, db, CodeConstraint, "can't be unset", func(tx *Tx) error { return tx.Unset(ann, "name") })
	wantCode(t, db, CodeType, "not a float", func(tx *Tx) error {
		_, err := tx.Create("", nil, Add{Def: fd("f", "float"), Value: value.Int(1)})
		return err
	})
	wantCode(t, db, CodeType, "int value is not a text", func(tx *Tx) error { return tx.Set(ann, Fields{"name": value.Int(1)}) })
	wantCode(t, db, CodeNotFound, "did you mean Person", func(tx *Tx) error {
		_, err := tx.Create("Persn", nil)
		return err
	})
	wantCode(t, db, CodeConstraint, "declared by kind", func(tx *Tx) error { return tx.DropField(ann, "name") })
	wantCode(t, db, CodeConstraint, "check failed for x: x > 0", func(tx *Tx) error {
		_, err := tx.Create("", nil, Add{Def: model.FieldDef{Name: "x", Check: "x > 0"}, Value: value.Int(-1)})
		return err
	})
}

func TestDefaultsAndChecks(t *testing.T) {
	db := newDB(t)
	field := func(name, typ, def, check string) model.FieldDef {
		f := fd(name, typ)
		f.Default, f.Check = def, check
		return f
	}
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Task", Fields: []model.FieldDef{
			fd("title", "text"),
			field("done", "bool", "false", ""),
			field("created", "date", "today()", ""),
			field("tags", "set<text>", "{}", ""),
			field("cfg", "doc<json>", `"{}"`, ""),
			field("lo", "int", "0", "lo >= 0"),
			field("hi", "int?", "", "hi is null or hi > lo"),
		}})
		return err
	})
	var id value.ID
	update(t, db, func(tx *Tx) error {
		var err error
		id, err = tx.Create("Task", Fields{"title": txt("a"), "hi": value.Int(5)})
		return err
	})
	db.View(func(tx *Tx) error {
		e, err := tx.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, n := range []string{"done", "created", "tags", "cfg", "lo", "hi"} {
			got = append(got, n+"="+e.Value(n).String())
		}
		if want := `done=false created=d"2026-11-22" tags={} cfg=cast("{}" as doc<json>) lo=0 hi=5`; strings.Join(got, " ") != want {
			t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
		}
		return nil
	})
	wantCode(t, db, CodeConstraint, "check failed for hi: hi is null or hi > lo", func(tx *Tx) error {
		return tx.Set(id, Fields{"lo": value.Int(9)})
	})
	wantCode(t, db, CodeConstraint, "check failed for lo", func(tx *Tx) error {
		_, err := tx.Create("Task", Fields{"title": txt("b"), "lo": value.Int(-1)})
		return err
	})
	update(t, db, func(tx *Tx) error { return tx.Unset(id, "hi") }) // null passes the check

	// Mistakes are reported when the definition is made.
	for _, c := range []struct {
		f    model.FieldDef
		code Code
		msg  string
	}{
		{field("a", "int", `"x"`, ""), CodeType, `default "x"`},
		{field("a", "int", "b", ""), CodeType, "a default needs a constant value"},
		{field("a", "int", "", "b > 1"), CodeUnknownField, "no field b"},
		{field("a", "int", "", "a + 1"), CodeType, "check needs a condition"},
		{field("a", "int", "1 +", ""), CodeType, "syntax"},
	} {
		wantCode(t, db, c.code, c.msg, func(tx *Tx) error {
			_, err := tx.DefineKind(model.Kind{Name: "Bad", Fields: []model.FieldDef{c.f}})
			return err
		})
	}

	// alter kind: a new field's default fills existing entities, and a new
	// check is tested against them.
	update(t, db, func(tx *Tx) error {
		return tx.AlterKind("Task", func(k *model.Kind) error {
			k.Fields = append(k.Fields, field("prio", "int", "3", ""))
			return nil
		})
	})
	db.View(func(tx *Tx) error {
		e, _ := tx.Get(id)
		if v := e.Value("prio"); v.String() != "3" {
			t.Errorf("prio = %v", v)
		}
		return nil
	})
	wantCode(t, db, CodeConstraint, "1 of 1 Task entities don't fit", func(tx *Tx) error {
		return tx.AlterKind("Task", func(k *model.Kind) error {
			for i := range k.Fields {
				if k.Fields[i].Name == "prio" {
					k.Fields[i].Check = "prio > 5"
				}
			}
			return nil
		})
	})

	// Relation property checks, including against existing relations.
	since := fd("since", "int?")
	since.Check = "since is null or since > 1900"
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineRelation(RelationDef{Name: "knows", Props: []model.FieldDef{since}})
		return err
	})
	wantCode(t, db, CodeConstraint, "relation knows: check failed for since", func(tx *Tx) error {
		_, err := tx.Relate(id, "knows", id, Fields{"since": value.Int(1800)})
		return err
	})
	update(t, db, func(tx *Tx) error {
		_, err := tx.Relate(id, "knows", id, Fields{"since": value.Int(1990)})
		return err
	})
	tight := fd("since", "int?")
	tight.Check = "since > 2000"
	wantCode(t, db, CodeConstraint, "existing knows relations don't fit", func(tx *Tx) error {
		return tx.AlterRelation("knows", RelationDef{Name: "knows", Props: []model.FieldDef{tight}})
	})

	// Table format column defaults given as text are evaluated.
	status := fd("status", "text")
	status.Default = `"active"`
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineFormat(model.Format{Name: "staff", Base: "csv", Header: true,
			Fields: []model.FieldDef{fd("name", "text"), status}})
		return err
	})
	f, _ := db.Catalog().Format("staff")
	if v, err := f.Parse("name,status\nAnn,\n"); err != nil || v.String() != `[{name: "Ann", status: "active"}]` {
		t.Errorf("staff: %v, %v", v, err)
	}
}

func fieldNames(e *Entity) string {
	var names []string
	for _, f := range e.Fields {
		names = append(names, f.Name)
	}
	return strings.Join(names, " ")
}

func TestKindlessAndClosed(t *testing.T) {
	db := newDB(t)
	note := create(t, db, "", Fields{"title": txt("Loose note"), "n": value.Int(1)})
	e := get(t, db, note)
	if e.Kind != nil || len(e.Fields) != 2 || e.Def(e.Fields[0]).Type.Code != value.CodeAny {
		t.Errorf("kindless: %+v", e)
	}
	wantCode(t, db, CodeUnknownField, "did you mean title?", func(tx *Tx) error { return tx.Set(note, Fields{"titel": txt("x")}) })
	wantCode(t, db, CodeConstraint, "already exists", func(tx *Tx) error {
		return tx.AddField(note, Add{Def: model.FieldDef{Name: "title"}, Value: txt("x")})
	})

	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Invoice", Closed: true, Fields: []model.FieldDef{fd("n", "int")}})
		return err
	})
	inv := create(t, db, "Invoice", Fields{"n": value.Int(1)})
	wantCode(t, db, CodeConstraint, "closed", func(tx *Tx) error {
		return tx.AddField(inv, Add{Def: model.FieldDef{Name: "x"}, Value: value.Int(1)})
	})
}

func TestUnique(t *testing.T) {
	db := newDB(t)
	definePerson(t, db)
	ann := create(t, db, "Person", Fields{"name": txt("Ann"), "email": txt("a@x.io")})
	dup := func(tx *Tx) error {
		_, err := tx.Create("Person", Fields{"name": txt("B"), "email": txt("a@x.io")})
		return err
	}
	wantCode(t, db, CodeConstraint, "unique", dup)
	// Changing Ann's email frees the old one.
	update(t, db, func(tx *Tx) error { return tx.Set(ann, Fields{"email": txt("ann@x.io")}) })
	update(t, db, func(tx *Tx) error { return dup(tx) })
	// Setting a field to its own current value is fine.
	update(t, db, func(tx *Tx) error { return tx.Set(ann, Fields{"email": txt("ann@x.io")}) })
	// Deleting frees the value.
	update(t, db, func(tx *Tx) error { return tx.Delete(ann) })
	create(t, db, "Person", Fields{"name": txt("C"), "email": txt("ann@x.io")})

	// Two concurrent transactions claiming the same value: one conflicts.
	t1, t2 := db.Begin(TxOptions{}), db.Begin(TxOptions{})
	for _, tx := range []*Tx{t1, t2} {
		if _, err := tx.Create("Person", Fields{"name": txt("D"), "email": txt("d@x.io")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := t2.Commit(); !IsConflict(err) {
		t.Errorf("second claim: %v", err)
	}
}

func TestRefs(t *testing.T) {
	db := newDB(t)
	definePerson(t, db)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Team", Fields: []model.FieldDef{
			func() model.FieldDef { f := fd("lead", "ref<Person>"); f.OnDelete = model.RefRestrict; return f }(),
			fd("members", "list<ref<Person>>?"),
		}})
		return err
	})
	ann := create(t, db, "Person", Fields{"name": txt("Ann"), "email": txt("a@x.io")})
	bob := create(t, db, "Person", Fields{"name": txt("Bob"), "email": txt("b@x.io"), "manager": value.Ref{Target: ann}})
	team := create(t, db, "Team", Fields{"lead": value.Ref{Target: ann}, "members": must(value.NewList(value.Ref{Target: bob}))})

	wantCode(t, db, CodeType, "is not a Person", func(tx *Tx) error {
		return tx.Set(bob, Fields{"manager": value.Ref{Target: team}})
	})
	wantCode(t, db, CodeConstraint, "doesn't exist", func(tx *Tx) error {
		return tx.Set(team, Fields{"members": must(value.NewList(value.Ref{Target: value.ID{9}}))})
	})
	wantCode(t, db, CodeConstraint, "restrict ref", func(tx *Tx) error { return tx.Delete(ann) })
	// Bob's manager ref and the members list dangle, so Bob can go.
	update(t, db, func(tx *Tx) error { return tx.Delete(bob) })
	// Once the team points elsewhere, Ann can go too.
	cat := create(t, db, "Person", Fields{"name": txt("Cat"), "email": txt("c@x.io")})
	update(t, db, func(tx *Tx) error { return tx.Set(team, Fields{"lead": value.Ref{Target: cat}}) })
	update(t, db, func(tx *Tx) error { return tx.Delete(ann) })
}

func TestRelations(t *testing.T) {
	db := newDB(t)
	definePerson(t, db)
	ann := create(t, db, "Person", Fields{"name": txt("Ann"), "email": txt("a@x.io")})
	bob := create(t, db, "Person", Fields{"name": txt("Bob"), "email": txt("b@x.io")})
	note := create(t, db, "", Fields{"text": txt("hi")})

	// An undefined type is registered on first use.
	var knows value.ID
	update(t, db, func(tx *Tx) error {
		var err error
		knows, err = tx.Relate(ann, "knows", bob, Fields{"since": value.Int(2019)})
		return err
	})
	if rt, ok := db.Catalog().RelationType("knows"); !ok || rt.Defined {
		t.Errorf("knows: %+v", rt)
	}
	// Defining it afterwards keeps the relation and checks it fits.
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineRelation(RelationDef{Name: "knows", From: "Person", To: "Person", Props: []model.FieldDef{fd("since", "int?")}})
		return err
	})
	wantCode(t, db, CodeConstraint, "must end at a Person", func(tx *Tx) error {
		_, err := tx.Relate(ann, "knows", note, nil)
		return err
	})
	wantCode(t, db, CodeUnknownField, "no field", func(tx *Tx) error {
		_, err := tx.Relate(ann, "knows", bob, Fields{"sinse": value.Int(1)})
		return err
	})

	// Max out, and relations from a field.
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineRelation(RelationDef{Name: "spouse", From: "Person", To: "Person", MaxOut: 1, MaxIn: 1})
		return err
	})
	update(t, db, func(tx *Tx) error { _, err := tx.Relate(ann, "spouse", bob, nil); return err })
	wantCode(t, db, CodeConstraint, "at most 1", func(tx *Tx) error {
		_, err := tx.Relate(ann, "spouse", ann, nil)
		return err
	})
	nameField, _ := get(t, db, ann).Field("name")
	update(t, db, func(tx *Tx) error { _, err := tx.Relate(nameField.ID, "mentioned_in", note, nil); return err })

	db.View(func(tx *Tx) error {
		out, _ := tx.Relations(ann, Out, "")
		in, _ := tx.Relations(bob, In, "knows")
		both, _ := tx.Relations(note, Both, "")
		if len(out) != 2 || len(in) != 1 || in[0].ID != knows || len(both) != 1 || both[0].From != nameField.ID {
			t.Errorf("out %d, in %d, both %d", len(out), len(in), len(both))
		}
		if v, _ := in[0].Props.Get(txt("since")); !value.Equal(v, value.Int(2019)) {
			t.Errorf("props: %s", in[0].Props)
		}
		return nil
	})

	wantCode(t, db, CodeConstraint, "still exist", func(tx *Tx) error { return tx.DropRelation("spouse") })

	// Deleting Ann removes her relations and those of her fields.
	update(t, db, func(tx *Tx) error { return tx.Delete(ann) })
	db.View(func(tx *Tx) error {
		if rels, _ := tx.Relations(bob, Both, ""); len(rels) != 0 {
			t.Errorf("bob still has %d relations", len(rels))
		}
		if rels, _ := tx.Relations(note, Both, ""); len(rels) != 0 {
			t.Errorf("note still has %d relations", len(rels))
		}
		if _, ok, _ := tx.Exists(nameField.ID); ok {
			t.Error("Ann's field still exists")
		}
		return nil
	})

	// Restrict relations block deletes; UnrelateAll clears them.
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineRelation(RelationDef{Name: "owns", OnDelete: model.RelRestrict})
		return err
	})
	update(t, db, func(tx *Tx) error { _, err := tx.Relate(bob, "owns", note, nil); return err })
	wantCode(t, db, CodeConstraint, "restrict on delete", func(tx *Tx) error { return tx.Delete(note) })
	update(t, db, func(tx *Tx) error {
		n, err := tx.UnrelateAll(bob, "owns", note)
		if n != 1 {
			t.Errorf("UnrelateAll = %d", n)
		}
		return err
	})
	update(t, db, func(tx *Tx) error { return tx.Delete(note) })
	update(t, db, func(tx *Tx) error { return tx.DropRelation("spouse") }) // its only relation went with Ann
}

func TestDefinitionsAreTransactional(t *testing.T) {
	db := newDB(t)
	tx := db.Begin(TxOptions{})
	if _, err := tx.DefineKind(model.Kind{Name: "Temp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Create("Temp", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Catalog().Kind("Temp"); ok {
		t.Error("uncommitted kind visible")
	}
	tx.Rollback()
	if _, ok := db.Catalog().Kind("Temp"); ok {
		t.Error("rolled back kind visible")
	}

	// A definition change and a concurrent data change: the later commit
	// conflicts.
	definePerson(t, db)
	data, def := db.Begin(TxOptions{}), db.Begin(TxOptions{})
	if _, err := data.Create("Person", Fields{"name": txt("A"), "email": txt("a@x")}); err != nil {
		t.Fatal(err)
	}
	if err := def.AlterKind("Person", func(k *model.Kind) error {
		k.Fields = append(k.Fields, fd("phone", "text"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := def.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := data.Commit(); !IsConflict(err) {
		t.Errorf("data written under the old kind: %v", err)
	}
}

func TestAlterKind(t *testing.T) {
	db := newDB(t)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Item", Fields: []model.FieldDef{fd("n", "int"), fd("old", "text?")}})
		return err
	})
	a := create(t, db, "Item", Fields{"n": value.Int(1), "old": txt("x")}, Add{Def: fd("color", "text"), Value: txt("red")})
	b := create(t, db, "Item", Fields{"n": value.Int(2)})

	// A required field no entity has can't be added.
	wantCode(t, db, CodeConstraint, "2 of 2 Item entities don't fit", func(tx *Tx) error {
		return tx.AlterKind("Item", func(k *model.Kind) error {
			k.Fields = append(k.Fields, fd("must", "int"))
			return nil
		})
	})
	// Declaring "color": a's added field becomes declared; dropping "old"
	// removes its data; n changes from int to decimal.
	update(t, db, func(tx *Tx) error {
		return tx.AlterKind("Item", func(k *model.Kind) error {
			k.Fields = []model.FieldDef{fd("n", "decimal(5,1)"), fd("color", "text?")}
			return nil
		})
	})
	ea := get(t, db, a)
	if f, _ := ea.Field("color"); !f.Declared || ea.Value("old") != nil || ea.Value("n").String() != "1.0d" {
		t.Errorf("a after alter: %s", fieldNames(ea))
	}
	// Closing fails while an entity has its own field.
	update(t, db, func(tx *Tx) error { return tx.AddField(b, Add{Def: fd("extra", "int"), Value: value.Int(1)}) })
	wantCode(t, db, CodeConstraint, "can't be closed", func(tx *Tx) error {
		return tx.AlterKind("Item", func(k *model.Kind) error { k.Closed = true; return nil })
	})
	// Renaming keeps the entities.
	update(t, db, func(tx *Tx) error {
		return tx.AlterKind("Item", func(k *model.Kind) error { k.Name = "Thing"; return nil })
	})
	if e := get(t, db, b); e.Kind.Name != "Thing" {
		t.Errorf("kind after rename: %s", e.Kind.Name)
	}
	wantCode(t, db, CodeConstraint, "still has 2 entities", func(tx *Tx) error { return tx.DropKind("Thing") })
	update(t, db, func(tx *Tx) error {
		if err := tx.Delete(a); err != nil {
			return err
		}
		if err := tx.Delete(b); err != nil {
			return err
		}
		return tx.DropKind("Thing")
	})
}

func TestUniqueIndexAndFormats(t *testing.T) {
	db := newDB(t)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Product", Fields: []model.FieldDef{fd("maker", "text"), fd("sku", "text")}})
		return err
	})
	create(t, db, "Product", Fields{"maker": txt("acme"), "sku": txt("1")})
	create(t, db, "Product", Fields{"maker": txt("acme"), "sku": txt("1")})
	wantCode(t, db, CodeConstraint, "have the same values", func(tx *Tx) error {
		_, err := tx.DefineIndex("maker_sku", "Product", true, false, model.IndexKey{Field: "maker"}, model.IndexKey{Field: "sku"})
		return err
	})
	// Once the duplicate is fixed, the index is created and enforced.
	var second value.ID
	db.View(func(tx *Tx) error {
		return tx.Entities("Product", func(id value.ID) bool { second = id; return true })
	})
	update(t, db, func(tx *Tx) error { return tx.Set(second, Fields{"sku": txt("2")}) })
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineIndex("maker_sku", "Product", true, false, model.IndexKey{Field: "maker"}, model.IndexKey{Field: "sku"})
		return err
	})
	wantCode(t, db, CodeConstraint, "unique", func(tx *Tx) error {
		_, err := tx.Create("Product", Fields{"maker": txt("acme"), "sku": txt("2")})
		return err
	})
	create(t, db, "Product", Fields{"maker": txt("other"), "sku": txt("2")})
	update(t, db, func(tx *Tx) error { return tx.DropIndex("maker_sku") })
	create(t, db, "Product", Fields{"maker": txt("acme"), "sku": txt("2")})

	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineFormat(model.Format{Name: "invoice", Base: "json", Fields: []model.FieldDef{fd("total", "decimal(12,2)")}})
		return err
	})
	inv, _ := db.Catalog().Format("invoice")
	doc := must(value.NewDoc(inv, `{"total": 12}`))
	note := create(t, db, "", nil, Add{Def: fd("bill", "doc<invoice>"), Value: doc})
	wantCode(t, db, CodeConstraint, "is used by field", func(tx *Tx) error { return tx.DropFormat("invoice") })
	update(t, db, func(tx *Tx) error { return tx.Delete(note) })
	update(t, db, func(tx *Tx) error { return tx.DropFormat("invoice") })
}

func TestQueues(t *testing.T) {
	db := newDB(t)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "Worker", Fields: []model.FieldDef{
			fd("jobs", "queue.fifo<text, max 2>"), fd("inbox", "queue.priority<text>"),
		}})
		return err
	})
	w := create(t, db, "Worker", nil)
	update(t, db, func(tx *Tx) error {
		for i, s := range []string{"low", "high", "mid"} {
			if err := tx.Push(w, "inbox", txt(s), int64([]int{1, 9, 5}[i])); err != nil {
				return err
			}
		}
		if err := tx.Push(w, "jobs", txt("a"), 0); err != nil {
			return err
		}
		return tx.Push(w, "jobs", txt("b"), 0)
	})
	wantCode(t, db, CodeConstraint, "full", func(tx *Tx) error { return tx.Push(w, "jobs", txt("c"), 0) })
	wantCode(t, db, CodeType, "not a text", func(tx *Tx) error { return tx.Push(w, "inbox", value.Int(1), 0) })
	update(t, db, func(tx *Tx) error {
		vs, err := tx.Pop(w, "inbox", 10)
		if got := joinValues(vs); got != `"high" "mid" "low"` {
			t.Errorf("pop order: %s", got)
		}
		return err
	})
	update(t, db, func(tx *Tx) error {
		v, err := tx.PopOne(w, "inbox")
		if !value.IsNull(v) {
			t.Errorf("empty pop: %s", v)
		}
		q, _ := tx.Queue(w, "jobs")
		if q.Len() != 2 || !value.Equal(q.Peek(), txt("a")) {
			t.Errorf("jobs: %s", q)
		}
		return err
	})
}

func joinValues(vs []value.Value) string {
	var s []string
	for _, v := range vs {
		s = append(s, v.String())
	}
	return strings.Join(s, " ")
}

func TestPopWait(t *testing.T) {
	db := newDB(t)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "W", Fields: []model.FieldDef{fd("q", "queue.fifo<int>")}})
		return err
	})
	w := create(t, db, "W", nil)
	ctx := context.Background()

	// An item already there is returned at once; an empty queue times out.
	update(t, db, func(tx *Tx) error { return tx.Push(w, "q", value.Int(1), 0) })
	if v, err := db.PopWait(ctx, w, "q", time.Second, TxOptions{}); err != nil || !value.Equal(v, value.Int(1)) {
		t.Fatalf("immediate: %v, %v", v, err)
	}
	if v, err := db.PopWait(ctx, w, "q", 20*time.Millisecond, TxOptions{}); err != nil || !value.IsNull(v) {
		t.Fatalf("timeout: %v, %v", v, err)
	}

	// Three waiters, started in order, get three pushed items in order.
	results := make([]value.Value, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			v, err := db.PopWait(ctx, w, "q", 5*time.Second, TxOptions{})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		})
		waitFor(t, func() bool {
			db.wmu.Lock()
			defer db.wmu.Unlock()
			wl := db.waiters[string(keysField(w, "q"))]
			return wl != nil && len(wl.list) == i+1
		})
	}
	update(t, db, func(tx *Tx) error {
		for i := range 3 {
			if err := tx.Push(w, "q", value.Int(int64(10+i)), 0); err != nil {
				return err
			}
		}
		return nil
	})
	wg.Wait()
	if got := joinValues(results); got != "10 11 12" {
		t.Errorf("waiters served out of order: %s", got)
	}

	// Cancelling the context ends the wait.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := db.PopWait(cctx, w, "q", 5*time.Second, TxOptions{}); err != context.Canceled {
		t.Errorf("cancel: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReadOnlyAndDatabases(t *testing.T) {
	db := newDB(t)
	err := db.View(func(tx *Tx) error {
		_, err := tx.Create("", nil)
		return err
	})
	if CodeOf(err) != CodeConstraint || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("write in read-only tx: %v", err)
	}
	eng := db.eng
	if _, err := eng.CreateDatabase("test"); CodeOf(err) != CodeConstraint {
		t.Errorf("duplicate database: %v", err)
	}
	if _, err := eng.CreateDatabase("bad name"); err == nil {
		t.Error("bad database name accepted")
	}
	eng.CreateDatabase("other")
	if got := strings.Join(eng.Databases(), ","); got != "other,test" {
		t.Errorf("databases: %s", got)
	}
	if err := eng.DropDatabase("other"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Database("other"); CodeOf(err) != CodeNotFound {
		t.Errorf("dropped database: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func keysField(id value.ID, name string) []byte { return keys.Field(id, name) }

func TestQueueFieldsStartEmpty(t *testing.T) {
	db := newDB(t)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "P", Fields: []model.FieldDef{fd("name", "text")}})
		return err
	})
	p := create(t, db, "P", Fields{"name": txt("x")})
	// Adding a queue field to a kind gives existing entities an empty queue.
	update(t, db, func(tx *Tx) error {
		return tx.AlterKind("P", func(k *model.Kind) error {
			k.Fields = append(k.Fields, fd("inbox", "queue.lifo<int>"))
			return nil
		})
	})
	if q, ok := get(t, db, p).Value("inbox").(value.Queue); !ok || q.Len() != 0 || q.Code() != value.CodeLifo {
		t.Errorf("inbox = %v", get(t, db, p).Value("inbox"))
	}
}

func TestMetadataAndLookups(t *testing.T) {
	db := newDB(t)
	tx := db.Begin(TxOptions{User: "ann"})
	id, err := tx.Create("", Fields{"x": value.Int(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.SetLabels(id, must(value.NewSet(txt("vip")))); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetProps(id, must(value.MapOfText(map[string]value.Value{"source": txt("import")}))); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetLabels(id, must(value.NewSet(value.Int(1)))); CodeOf(err) != CodeType {
		t.Errorf("int label: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	e := get(t, db, id)
	if e.Meta.Owner != "ann" || e.Meta.Labels.String() != `{"vip"}` || e.Meta.Version != 3 {
		t.Errorf("meta: %+v", e.Meta)
	}
	x, _ := e.Field("x")
	db.View(func(tx *Tx) error {
		f, err := tx.FieldByID(x.ID)
		if err != nil || f.Name != "x" || f.Entity != id {
			t.Errorf("FieldByID: %+v, %v", f, err)
		}
		if kind, ok, _ := tx.Exists(x.ID); !ok || kind != model.NodeField {
			t.Errorf("Exists(field) = %v, %v", kind, ok)
		}
		if _, err := tx.FieldByID(id); CodeOf(err) != CodeNotFound {
			t.Errorf("FieldByID(entity): %v", err)
		}
		return nil
	})
}

func TestRetryCounter(t *testing.T) {
	db := newDB(t)
	id := create(t, db, "", Fields{"n": value.Int(0)})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				err := db.Retry(100, func(tx *Tx) error {
					e, err := tx.Get(id)
					if err != nil {
						return err
					}
					return tx.Set(id, Fields{"n": e.Value("n").(value.Int) + 1})
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if n := get(t, db, id).Value("n"); !value.Equal(n, value.Int(160)) {
		t.Errorf("n = %s, want 160", n)
	}
}

func TestAlterRelationAndEndpoints(t *testing.T) {
	db := newDB(t)
	a := create(t, db, "", Fields{"x": value.Int(1)})
	b := create(t, db, "", Fields{"x": value.Int(2)})
	var r1 value.ID
	update(t, db, func(tx *Tx) error {
		var err error
		if r1, err = tx.Relate(a, "links", b, nil); err != nil {
			return err
		}
		_, err = tx.Relate(a, "links", a, nil)
		return err
	})
	wantCode(t, db, CodeConstraint, "more than max out 1", func(tx *Tx) error {
		return tx.AlterRelation("links", RelationDef{Name: "links", MaxOut: 1})
	})
	update(t, db, func(tx *Tx) error { return tx.Unrelate(r1) })
	update(t, db, func(tx *Tx) error { return tx.AlterRelation("links", RelationDef{Name: "points_to", MaxOut: 1}) })
	db.View(func(tx *Tx) error {
		rels, _ := tx.Relations(a, Out, "points_to")
		if len(rels) != 1 || rels[0].To != a {
			t.Errorf("after rename: %d relations", len(rels))
		}
		return nil
	})
	// Field-only endpoints, and relations can't connect relations.
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineRelation(RelationDef{Name: "annotates", From: "field", To: "any"})
		return err
	})
	wantCode(t, db, CodeConstraint, "must start at a field", func(tx *Tx) error {
		_, err := tx.Relate(a, "annotates", b, nil)
		return err
	})
	wantCode(t, db, CodeType, "is a relation", func(tx *Tx) error {
		rels, _ := tx.Relations(a, Out, "")
		_, err := tx.Relate(rels[0].ID, "x", b, nil)
		return err
	})
	wantCode(t, db, CodeNotFound, "no entity or field", func(tx *Tx) error {
		_, err := tx.Relate(a, "x", value.ID{7}, nil)
		return err
	})
}

func TestNestedRefsAndAnyQueues(t *testing.T) {
	db := newDB(t)
	definePerson(t, db)
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "City"})
		return err
	})
	ann := create(t, db, "Person", Fields{"name": txt("Ann"), "email": txt("a@x")})
	city := create(t, db, "City", nil)
	add := func(typ string, v value.Value) func(tx *Tx) error {
		return func(tx *Tx) error {
			_, err := tx.Create("", nil, Add{Def: fd("f", typ), Value: v})
			return err
		}
	}
	update(t, db, add("union<ref<Person> | ref<City>>", value.Ref{Target: city}))
	update(t, db, add("map<text, ref<Person>>", must(value.MapOfText(map[string]value.Value{"boss": value.Ref{Target: ann}}))))
	wantCode(t, db, CodeType, "is not a Person", add("map<text, ref<Person>>", must(value.MapOfText(map[string]value.Value{"boss": value.Ref{Target: city}}))))
	wantCode(t, db, CodeType, "is not a Person", add("record{who: ref<Person>}", must(value.NewRecord([]string{"who"}, []value.Value{value.Ref{Target: city}}))))
	wantCode(t, db, CodeNotFound, "no kind Nope", add("ref<Nope>", value.Null))

	// A queue held in an any field can be pushed to and popped.
	q := must(value.NewQueue(value.MustParseType("queue.fifo<int>")))
	n := create(t, db, "", Fields{"q": q})
	update(t, db, func(tx *Tx) error { return tx.Push(n, "q", value.Int(5), 0) })
	update(t, db, func(tx *Tx) error {
		v, err := tx.PopOne(n, "q")
		if !value.Equal(v, value.Int(5)) {
			t.Errorf("popped %v", v)
		}
		return err
	})
	x := create(t, db, "", Fields{"x": value.Int(1)})
	wantCode(t, db, CodeType, "not a queue", func(tx *Tx) error { return tx.Push(x, "x", value.Int(1), 0) })
}
