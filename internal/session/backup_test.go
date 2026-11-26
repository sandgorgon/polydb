package session

import (
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

func TestBackupStatements(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(engine.Options{Dir: "/data", FS: vfs.NewMem(), HistorySweep: -1, StatsRefresh: -1,
		PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	s := New(eng, security.AdminUser)
	defer s.Close()
	big := map[string]value.Value{"b": value.MustText(strings.Repeat("large ", 10<<10))}
	if _, err := s.Exec(ctx, `create database shop; use shop; define kind P { name: text, body: text? };
		create P { name: "a", body: $b }; create P { name: "b" };
		create user owner password "pw"; grant admin on database shop to owner;
		create user clerk password "pw"; grant write on database shop to clerk`, big); err != nil {
		t.Fatal(err)
	}
	res, err := s.Exec(ctx, `backup database shop to "nightly/1"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsText(res[0]); !strings.HasPrefix(got, `"shop" "/data/backups/nightly/1" `) {
		t.Fatalf("backup result: %s", got)
	}
	res, err = s.Exec(ctx, `restore database shop2 from $p; use shop2; find P p | sort p.name | show p.name, len(p.body)`,
		map[string]value.Value{"p": value.MustText("nightly/1")})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsText(res[0]); !strings.HasPrefix(got, `"shop2" "shop" t"`) {
		t.Errorf("restore result: %s", got)
	}
	if got := rowsText(res[2]); got != `"a" 61440; "b" null` {
		t.Errorf("restored rows: %s", got)
	}

	// A database admin backs it up; restoring creates a database, which
	// needs a server admin. Others can't do either.
	owner, clerk := New(eng, "owner"), New(eng, "clerk")
	defer owner.Close()
	defer clerk.Close()
	if _, err := owner.Exec(ctx, `backup database shop to "by-owner"`, nil); err != nil {
		t.Errorf("database admin's backup: %v", err)
	}
	if _, err := owner.Exec(ctx, `restore database shop3 from "by-owner"`, nil); execCode(err) != "permission" {
		t.Errorf("database admin's restore: %v", err)
	}
	if _, err := clerk.Exec(ctx, `backup database shop to "by-clerk"`, nil); execCode(err) != "permission" {
		t.Errorf("writer's backup: %v", err)
	}
	for _, q := range []string{
		`backup database shop to "../outside"`,
		`backup database shop to "/tmp/x"`,
		`backup database shop to "nightly/1"`, // exists
		`restore database shop from "nightly/1"`,
		`restore database shop4 from "missing"`,
		`begin; backup database shop to "in-txn"`,
	} {
		if _, err := s.Exec(ctx, q, nil); err == nil {
			t.Errorf("%s: accepted", q)
		}
		s.Exec(ctx, `rollback`, nil)
	}
}
