package session

import (
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

func TestExportImportStatements(t *testing.T) {
	ctx := context.Background()
	fs := vfs.NewMem()
	eng, err := engine.Open(engine.Options{Dir: "/data", FS: fs, HistorySweep: -1, StatsRefresh: -1,
		PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	s := New(eng, security.AdminUser)
	defer s.Close()
	run := func(src string, params map[string]value.Value) [][]any {
		t.Helper()
		res, err := s.Exec(ctx, src, params)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return res[len(res)-1].Rows
	}
	run(`create database a; use a; define kind City { name: text };
		define kind P { name: text, city: ref<City>?, note: text? };
		create City @x { name: "Oslo" }; create P @p { name: "Pia", city: @x }; create P @q { name: "Quin" };
		relate @p ~knows~> @q;
		create user reader password "pw"; grant read on database a to reader;
		create user owner password "pw"; grant admin on database a to owner`, nil)

	// To a file in the backup directory, and back into another database.
	rows := run(`export database a to "a.jsonl"`, nil)
	if rows[0][0].(value.Text).Go() != "/data/backups/a.jsonl" || rows[0][1] != value.Int(3) || rows[0][2] != value.Int(1) {
		t.Fatalf("export to a file: %v", rows)
	}
	if _, err := s.Exec(ctx, `export database a to "a.jsonl"`, nil); err == nil {
		t.Error("an export overwrote a file")
	}
	rows = run(`create database b; import into database b from "a.jsonl"`, nil)
	if rows[0][0] != value.Int(2) || rows[0][1] != value.Int(3) {
		t.Fatalf("import: %v", rows)
	}
	if got := rowsText(&query.Result{Rows: run(`use b; find P p | sort p.name | show p.name, p.city.name`, nil)}); got != `"Pia" "Oslo"; "Quin" null` {
		t.Errorf("imported data: %s", got)
	}

	// As a value, passed back in as a parameter, with new IDs.
	rows = run(`use a; export database a as native`, nil)
	exp := rows[0][0].(value.Value)
	rows = run(`use b; import into database b from $e with new ids`, map[string]value.Value{"e": exp})
	if rows[0][1] != value.Int(3) {
		t.Fatalf("import of a value: %v", rows)
	}
	if got := rowsText(&query.Result{Rows: run(`find P p | show count()`, nil)}); got != "4" {
		t.Errorf("after the copy: %s P", got)
	}

	// Query results.
	q := `export (find P p | sort p.name | show p.name, p.city) as `
	csv := string(must(value.Load(run(q+`csv`, nil)[0][0].(value.Value))).(value.Bytes).Go())
	if !strings.HasPrefix(csv, "p.name,p.city\nPia,") || !strings.Contains(csv, "\nQuin,\n") {
		t.Errorf("csv:\n%s", csv)
	}
	dot := string(must(value.Load(run(`export (find P p) as dot`, nil)[0][0].(value.Value))).(value.Bytes).Go())
	if !strings.Contains(dot, `label="P: Pia"`) || !strings.Contains(dot, `[label="knows"]`) {
		t.Errorf("dot:\n%s", dot)
	}
	graph := string(must(value.Load(run(`export (find P p) as graph`, nil)[0][0].(value.Value))).(value.Bytes).Go())
	if !strings.Contains(graph, `"edges":[{`) || !strings.Contains(graph, `"type":"knows"`) {
		t.Errorf("graph:\n%s", graph)
	}
	rows = run(`export (find P p | sort p.name) to "rows.jsonl"`, nil)
	if rows[0][1] != value.Int(4) {
		t.Errorf("rows export: %v", rows)
	}

	// Security, through a file.
	run(`export security without passwords to "sec.jsonl"`, nil)
	if rows = run(`import security from "sec.jsonl"`, nil); rows[0][3] != value.Int(3) { // all exist: kept
		t.Errorf("security import: %v", rows)
	}

	// Permissions.
	reader, owner := New(eng, "reader"), New(eng, "owner")
	defer reader.Close()
	defer owner.Close()
	if _, err := reader.Exec(ctx, `export database a`, nil); err != nil {
		t.Errorf("reader's export: %v", err)
	}
	for who, q := range map[*Session]string{
		reader: `import into database a from "a.jsonl"`,
		owner:  `export security`,
	} {
		if _, err := who.Exec(ctx, q, nil); execCode(err) != "permission" {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{`export database a as csv`, `export database a to "../x"`, `import into database a from 3`,
		`export (create P { name: "x" })`} {
		if _, err := s.Exec(ctx, q, nil); err == nil {
			t.Errorf("%s accepted", q)
		}
	}
}
