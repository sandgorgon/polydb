package session

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/storage/lsm"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
)

func diskOptions(fs vfs.FS) engine.Options {
	return engine.Options{Dir: "/data", FS: fs, PasswordIterations: security.MinIterations,
		Storage: lsm.Options{MemtableSize: 2 << 10, TableSize: 4 << 10, LevelBaseSize: 16 << 10, BlockSize: 512}}
}

func openDisk(t *testing.T, o engine.Options) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// rows runs statements and renders the last one's rows.
func rows(t *testing.T, eng *engine.Engine, user, db, src string) string {
	t.Helper()
	s := New(eng, user)
	defer s.Close()
	if db != "" {
		if _, err := s.Exec(context.Background(), "use "+db, nil); err != nil {
			t.Fatalf("use %s: %v", db, err)
		}
	}
	res, err := s.Exec(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	var out []string
	for _, row := range res[len(res)-1].Rows {
		var cells []string
		for _, c := range row {
			cells = append(cells, fmt.Sprint(c))
		}
		out = append(out, strings.Join(cells, " | "))
	}
	return strings.Join(out, "\n")
}

func execAs(t *testing.T, eng *engine.Engine, user, db, src string) error {
	t.Helper()
	s := New(eng, user)
	defer s.Close()
	if db != "" {
		if _, err := s.Exec(context.Background(), "use "+db, nil); err != nil {
			return err
		}
	}
	_, err := s.Exec(context.Background(), src, nil)
	return err
}

// TestPersistence closes and reopens an engine kept on disk: databases,
// definitions, entities, relations, users, roles, grants and keys all
// come back.
func TestPersistence(t *testing.T) {
	fs := vfs.NewMem()
	o := diskOptions(fs)
	eng := openDisk(t, o)
	if _, err := engine.Open(o); err == nil {
		t.Fatal("a second engine opened the same directory")
	}
	setup := `
		create database shop;
		use shop;
		define kind Person { name: text, age: int? };
		define kind Company { name: text };
		create Person @ann { name: "Ann", age: 41 };
		create Person @bob { name: "Bob" };
		create Company @acme { name: "Acme" };
		relate @ann ~works_at~> @acme;
		create user ramon password "correct horse";
		create role analyst;
		grant read on database shop to analyst;
		grant analyst to ramon;
		alter user ramon add key "` + strings.Repeat("ab", 32) + `" as "laptop";
		create database scratch;
	`
	if err := execAs(t, eng, security.AdminUser, "", setup); err != nil {
		t.Fatal(err)
	}
	var big strings.Builder // enough to flush and compact
	for i := range 300 {
		fmt.Fprintf(&big, "create Person { name: \"p%03d\", age: %d };\n", i, i)
	}
	if err := execAs(t, eng, security.AdminUser, "shop", big.String()); err != nil {
		t.Fatal(err)
	}
	query := `find Person p | where p.name = "Ann" | match p ~works_at~> c | show p.name, p.age, c.name`
	before := rows(t, eng, security.AdminUser, "shop", query)
	count := rows(t, eng, security.AdminUser, "shop", `find Person p | show count()`)
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng = openDisk(t, o)
	defer eng.Close()
	if got := strings.Join(eng.Databases(), ","); got != "scratch,shop" {
		t.Errorf("databases after reopen: %s", got)
	}
	if got := rows(t, eng, security.AdminUser, "shop", query); got != before || !strings.Contains(got, "Acme") {
		t.Errorf("after reopen: %q, want %q", got, before)
	}
	if got := rows(t, eng, security.AdminUser, "shop", `find Person p | show count()`); got != count || got != "302" {
		t.Errorf("count after reopen: %s, want %s", got, count)
	}
	sec := eng.Security()
	u, ok := sec.User("ramon")
	if !ok || u.Creds == nil || !u.Creds.Verify("correct horse") || u.Creds.Verify("wrong") {
		t.Error("ramon's password didn't survive")
	}
	if who, ok := sec.UserByKey(strings.Repeat("ab", 32)); !ok || who != "ramon" {
		t.Error("ramon's key didn't survive")
	}
	if !sec.Allowed("ramon", security.Read, "shop", [16]byte{}) || sec.Allowed("ramon", security.Write, "shop", [16]byte{}) {
		t.Error("ramon's grants didn't survive")
	}
	// The data is still writable, and new IDs don't collide.
	if err := execAs(t, eng, security.AdminUser, "shop", `create Person { name: "Cy" }; find Person p | where p.name = "Bob" | update p set age = 7`); err != nil {
		t.Fatal(err)
	}
	if got := rows(t, eng, security.AdminUser, "shop", `find Person p | where p.name = "Bob" | show p.age`); got != "7" {
		t.Errorf("update after reopen: %s", got)
	}
}

// TestDropDatabaseOnDisk checks that a dropped database's files and
// grants are gone, and that a new database with its name starts empty.
func TestDropDatabaseOnDisk(t *testing.T) {
	fs := vfs.NewMem()
	o := diskOptions(fs)
	eng := openDisk(t, o)
	err := execAs(t, eng, security.AdminUser, "", `
		create database old; use old;
		define kind K { a: int }; create K { a: 1 };
		create user eve password "pw"; grant read on database old to eve;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := execAs(t, eng, security.AdminUser, "", "drop database old"); err != nil {
		t.Fatal(err)
	}
	if dirs, _ := fs.List("/data/databases"); len(dirs) != 0 {
		t.Errorf("files left after drop: %v", dirs)
	}
	eng.Close()
	eng = openDisk(t, o)
	defer eng.Close()
	if len(eng.Databases()) != 0 {
		t.Errorf("dropped database came back: %v", eng.Databases())
	}
	if err := execAs(t, eng, security.AdminUser, "", "create database old"); err != nil {
		t.Fatal(err)
	}
	if eng.Security().Allowed("eve", security.Read, "old", [16]byte{}) {
		t.Error("a new database inherited the dropped one's grants")
	}
	if err := execAs(t, eng, security.AdminUser, "old", "define kind K { a: int }"); err != nil {
		t.Errorf("the new database isn't empty: %v", err)
	}
}

// TestEngineCrash crashes the machine at random points of a workload
// that creates and drops databases, defines kinds, writes data and
// changes users: everything acknowledged must come back, and
// interrupted creates and drops must leave no trace but their effect.
func TestEngineCrash(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for iter := range 30 {
		fs := vfs.NewMem()
		o := diskOptions(fs)
		eng := openDisk(t, o)
		steps := 5 + rng.IntN(40)
		wantDBs := map[string]int{} // database → Person count
		var wantUsers []string
		for i := range steps {
			switch rng.IntN(6) {
			case 0:
				name := fmt.Sprintf("db%d", rng.IntN(4))
				if _, ok := wantDBs[name]; ok {
					if err := execAs(t, eng, security.AdminUser, "", "drop database "+name); err != nil {
						t.Fatal(err)
					}
					delete(wantDBs, name)
				} else {
					if err := execAs(t, eng, security.AdminUser, "", "create database "+name+"; use "+name+"; define kind Person { name: text }"); err != nil {
						t.Fatal(err)
					}
					wantDBs[name] = 0
				}
			case 1:
				u := fmt.Sprintf("u%d", i)
				if err := execAs(t, eng, security.AdminUser, "", fmt.Sprintf("create user %s password \"pw%d\"", u, i)); err != nil {
					t.Fatal(err)
				}
				wantUsers = append(wantUsers, u)
			default:
				for name := range wantDBs {
					n := 1 + rng.IntN(20)
					var b strings.Builder
					for j := range n {
						fmt.Fprintf(&b, "create Person { name: \"p%d-%d\" };", i, j)
					}
					if err := execAs(t, eng, security.AdminUser, name, b.String()); err != nil {
						t.Fatal(err)
					}
					wantDBs[name] += n
					break
				}
			}
		}
		crashed := fs.Crash(rng)
		eng.Close()
		o.FS = crashed
		eng = openDisk(t, o)
		var got []string
		for _, name := range eng.Databases() {
			got = append(got, name)
			want, ok := wantDBs[name]
			if !ok {
				t.Fatalf("iteration %d: database %s came back after its drop", iter, name)
			}
			if c := rows(t, eng, security.AdminUser, name, "find Person p | show count()"); c != fmt.Sprint(want) {
				t.Fatalf("iteration %d: %s has %s people, want %d", iter, name, c, want)
			}
		}
		if len(got) != len(wantDBs) {
			t.Fatalf("iteration %d: databases %v, want %v", iter, got, wantDBs)
		}
		for _, u := range wantUsers {
			if _, ok := eng.Security().User(u); !ok {
				t.Fatalf("iteration %d: user %s lost", iter, u)
			}
		}
		dirs, _ := crashed.List("/data/databases")
		if len(dirs) != len(wantDBs) {
			t.Fatalf("iteration %d: %d database directories for %d databases", iter, len(dirs), len(wantDBs))
		}
		eng.Close()
	}
}

// TestSecuritySaveFailure checks that a change to users that can't be
// saved doesn't happen at all.
func TestSecuritySaveFailure(t *testing.T) {
	ffs := vfs.NewFaulty(vfs.NewMem())
	o := diskOptions(ffs)
	eng := openDisk(t, o)
	defer eng.Close()
	ffs.FailAt(ffs.Ops()+1, true)
	if err := execAs(t, eng, security.AdminUser, "", `create user mallory password "pw"`); err == nil {
		t.Fatal("creating a user succeeded on a failed disk")
	}
	if _, ok := eng.Security().User("mallory"); ok {
		t.Error("the user exists although saving failed")
	}
}

// TestHistoryPersists checks that versions, life records and as of reads
// survive a reopen and a crash, and that compaction keeps them.
func TestHistoryPersists(t *testing.T) {
	fs := vfs.NewMem()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := diskOptions(fs)
	o.Now = func() time.Time { return clock }
	eng := openDisk(t, o)
	run := func(src string) {
		t.Helper()
		if err := execAs(t, eng, security.AdminUser, "h", src); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
	}
	if err := execAs(t, eng, security.AdminUser, "", "create database h"); err != nil {
		t.Fatal(err)
	}
	run(`define kind Acct history { owner: text, balance: int }`)
	for i := range 40 { // enough versions to flush and compact
		clock = clock.Add(24 * time.Hour)
		run(fmt.Sprintf(`create Acct { owner: "o%02d", balance: %d }`, i, i))
		run(fmt.Sprintf(`find Acct a | where a.owner = "o%02d" | update a set balance = %d`, i/2, 1000+i))
	}
	clock = clock.Add(24 * time.Hour)
	run(`find Acct a | where a.owner = "o00" | delete a`)
	past := `find Acct a as of t"2026-01-10T12:00:00Z" | sort a.owner | show a.owner, a.balance`
	pastAll := `find Acct a as of t"2026-02-10T12:00:00Z" | show count()` // o00 is deleted on Feb 11
	want := rows(t, eng, security.AdminUser, "h", past)
	wantAll := rows(t, eng, security.AdminUser, "h", pastAll)
	if !strings.Contains(want, `"o00" | 1001`) || wantAll != "40" {
		t.Fatalf("before reopen: %q, %s", want, wantAll)
	}
	check := func(what string) {
		t.Helper()
		if got := rows(t, eng, security.AdminUser, "h", past); got != want {
			t.Errorf("%s: as of\n got %s\nwant %s", what, got, want)
		}
		if got := rows(t, eng, security.AdminUser, "h", pastAll); got != wantAll {
			t.Errorf("%s: deleted entity in the past: %s, want %s", what, got, wantAll)
		}
		if got := rows(t, eng, security.AdminUser, "h", `find Acct a | show count()`); got != "39" {
			t.Errorf("%s: %s accounts now", what, got)
		}
	}
	eng.Close()
	eng = openDisk(t, o)
	check("after reopen")
	crashed := fs.Crash(rand.New(rand.NewPCG(1, 2)))
	eng.Close()
	o.FS = crashed
	eng = openDisk(t, o)
	defer eng.Close()
	check("after a crash")
}
