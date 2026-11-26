package session

import (
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
)

func TestDatabaseMetadata(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(engine.Options{Dir: "/data", FS: vfs.NewMem(), HistorySweep: -1, StatsRefresh: -1,
		PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	s := New(eng, security.AdminUser)
	defer s.Close()
	if _, err := s.Exec(ctx, `create database shop; use shop; define kind P { n: int }; create P { n: 1 };
		create user owner password "pw"; grant admin on database shop to owner;
		create user clerk password "pw"; grant write on database shop to clerk; grant read on database shop to clerk`, nil); err != nil {
		t.Fatal(err)
	}

	// Created and owner come from create database.
	if got := sessionRows(t, s, `find P p | show database::name, database::owner, database::created is null, database::doc`); got != `"shop" "admin" false null` {
		t.Errorf("new database: %s", got)
	}

	// A database admin sets doc, labels and props.
	owner := New(eng, "owner")
	defer owner.Close()
	if _, err := owner.Exec(ctx, `alter database shop set doc = "EU orders", ::labels ++= {"prod", "eu"},
		::props ++= {schema_version: 3, team: "web"};
		alter database shop set ::props.schema_version = 4, ::labels --= {"eu"}, ::props --= {"team"}`, nil); err != nil {
		t.Fatal(err)
	}
	if got := sessionRows(t, s, `find P p | where database::props.schema_version = 4 | show database::doc, database::labels, database::props`); got != `"EU orders" {"prod"} {"schema_version": 4}` {
		t.Errorf("after alter: %s", got)
	}
	if got := sessionRows(t, s, `describe database shop`); !strings.Contains(got, `"doc" "EU orders"; "labels" {"prod"}; "props" {"schema_version": 4}`) {
		t.Errorf("describe database: %s", got)
	}
	if got := sessionRows(t, s, `show databases`); !strings.HasPrefix(got, `"shop" "admin" t"`) || !strings.HasSuffix(got, `{"prod"} "EU orders"`) {
		t.Errorf("show databases: %s", got)
	}

	// Others may read it, but only admins of the database change it.
	clerk := New(eng, "clerk")
	defer clerk.Close()
	if got := sessionRows(t, clerk, `use shop; describe database shop`); !strings.Contains(got, `"EU orders"`) {
		t.Errorf("a reader's describe: %s", got)
	}
	if _, err := clerk.Exec(ctx, `alter database shop set doc = "mine"`, nil); execCode(err) != "permission" {
		t.Errorf("a writer's alter database: %v", err)
	}
	for q, code := range map[string]string{
		`alter database shop set ::owner = "clerk"`: "constraint",
		`alter database shop set doc = 5`:           "type",
		`alter database shop set ::labels ++= {1}`:  "type",
		`alter database nope set doc = "x"`:         "not_found",
		`begin; alter database shop set doc = "x"`:  "constraint",
		`find P p | show database::colour`:          "not_found",
	} {
		if _, err := s.Exec(ctx, q, nil); execCode(err) != code {
			t.Errorf("%s: %v, want %s", q, err, code)
		}
		s.Exec(ctx, `rollback`, nil)
	}
	// doc = null removes the doc.
	if got := sessionRows(t, owner, `alter database shop set doc = null; use shop; find P p | show database::doc`); got != `null` {
		t.Errorf("doc removed: %s", got)
	}
	owner.Exec(ctx, `alter database shop set doc = "EU orders"`, nil)

	// A backup carries all of it, owner and creation time too.
	if _, err := s.Exec(ctx, `backup database shop to "b"; restore database shop2 from "b"`, nil); err != nil {
		t.Fatal(err)
	}
	if got := sessionRows(t, s, `use shop2; find P p | show database::name, database::owner, database::doc, database::props`); got != `"shop2" "admin" "EU orders" {"schema_version": 4}` {
		t.Errorf("restored: %s", got)
	}

	// An export carries doc, labels and props, merged into the target;
	// the target keeps its own owner, creation time and other props.
	if _, err := owner.Exec(ctx, `alter database shop set ::props.region = "eu"`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, `export database shop to "shop.jsonl"; create database copy; use copy;
		alter database copy set ::labels = {"staging"}, ::props = {schema_version: 1, local: true};
		import into database copy from "shop.jsonl"`, nil); err != nil {
		t.Fatal(err)
	}
	if got := sessionRows(t, s, `use copy; find P p | show database::doc, database::labels, database::props, database::owner`); got != `"EU orders" {"prod", "staging"} {"local": true, "region": "eu", "schema_version": 4} "admin"` {
		t.Errorf("imported: %s", got)
	}
}
