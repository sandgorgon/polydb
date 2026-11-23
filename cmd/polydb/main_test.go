package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/value"
	"github.com/sandgorgon/polydb/wire"
)

// isolate points HOME and the 9auth config at a temporary directory, so
// tests never touch the real identity, known peers or history.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("POLYDB_URL", "")
	t.Setenv("POLYDB_PASSWORD", "")
	return dir
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestShellMemory(t *testing.T) {
	isolate(t)
	input := `define kind Person {
  name : text
  age  : int?
}
create Person { name: "Ann", age: 34 }
create Person { name: "Bob" };

\set min 30
find Person p | where p.age > $min |
  show p.name, p.age

begin
create Person { name: "Cy" }
find Persn p
rollback
find Person p | show count()
\q
find Person p | show "ran after quit"
`
	out, errOut, code := runCLI(t, input, "--memory")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"p.name │ p.age\n───────┼──────\nAnn    │ 34\n(1 row)",
		"count()\n───────\n2\n(1 row)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "not_found: no kind Persn (did you mean Person?)") || !strings.Contains(errOut, "^") {
		t.Errorf("error output:\n%s", errOut)
	}
	// \q stopped the shell before the last line.
	if strings.Contains(out, "ran after quit") {
		t.Errorf("ran past \\q:\n%s", out)
	}
}

func TestPromptAndAbortedTransaction(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	db, name, err := connect(ctx, "", &connectFlags{memory: true}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, errOut bytes.Buffer
	sh := newShell(db, name, &out, &errOut, "table")
	sh.runScript(ctx, `define kind K { a: int }`, true)
	if p := sh.prompt(); p != "polydb:mem> " {
		t.Errorf("prompt %q", p)
	}
	sh.runScript(ctx, `begin`, true)
	if p := sh.prompt(); p != "polydb:mem*> " {
		t.Errorf("in a transaction: %q", p)
	}
	sh.runScript(ctx, `create K { a: "x" }`, true)
	if p := sh.prompt(); p != "polydb:mem!> " {
		t.Errorf("failed transaction: %q", p)
	}
	errOut.Reset()
	sh.runScript(ctx, `find K k`, true)
	if !strings.Contains(errOut.String(), "txn_aborted") {
		t.Errorf("after failure: %s", errOut.String())
	}
	sh.runScript(ctx, `rollback`, true)
	if p := sh.prompt(); p != "polydb:mem> " {
		t.Errorf("after rollback: %q", p)
	}
}

func TestRunScript(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.rhz")
	os.WriteFile(good, []byte("define kind K { a: int };\ncreate K { a: 1 };\nfind K k | show k.a;\n"), 0o600)
	out, errOut, code := runCLI(t, "", "run", "--memory", good)
	if code != 0 || !strings.Contains(out, "k.a\n───\n1\n(1 row)") {
		t.Errorf("good script: %d %q %q", code, out, errOut)
	}
	bad := filepath.Join(dir, "bad.rhz")
	os.WriteFile(bad, []byte("define kind K { a: int };\n\ncreate K { a: \"x\" };\nfind K k;\n"), 0o600)
	out, errOut, code = runCLI(t, "", "run", "--memory", bad)
	if code != 1 || !strings.Contains(errOut, "line 3: create K") || strings.Contains(out, "(0 rows)") {
		t.Errorf("bad script: %d %q %q", code, out, errOut)
	}
	// Standard input, and TSV output.
	out, _, code = runCLI(t, "define kind K { a: int }; create K { a: 7 }; find K k | show k.a, k.a * 2 as twice",
		"run", "--memory", "--format", "tsv", "-")
	if code != 0 || !strings.Contains(out, "k.a\ttwice\n7\t14\n") {
		t.Errorf("tsv: %d %q", code, out)
	}
	if _, _, code := runCLI(t, "", "run"); code != 2 {
		t.Errorf("no file: exit %d", code)
	}
}

func TestServeAndConnect(t *testing.T) {
	home := isolate(t)
	t.Setenv("POLYDB_ADMIN_PASSWORD", "secret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuffer
	addrs := make(chan [2]string, 1)
	cfg := serveConfig{listen: "127.0.0.1:0", dataDir: filepath.Join(home, "data"), adminKey: "self", iterations: 4096}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, &out, &errOut, func(tcp, unix string) { addrs <- [2]string{tcp, unix} })
	}()
	var a [2]string
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("serve: %v %s", err, errOut.String())
	case <-time.After(5 * time.Second):
		t.Fatal("server didn't start")
	}
	fp := regexp.MustCompile(`server fingerprint: ([0-9a-f]{64})`).FindStringSubmatch(out.String())
	if fp == nil || strings.Contains(out.String(), "admin password (shown once)") {
		t.Fatalf("serve output:\n%s", out.String())
	}

	// TCP with TLS: password login, pinned fingerprint. The client's
	// 9auth identity (registered with --admin-key self) logs in with KEY.
	t.Setenv("POLYDB_PASSWORD", "secret")
	got, errText, code := runCLI(t, "create database shop; use shop; define kind K { a: int }; create K { a: 1 }; find K k | show count()",
		"run", "--fingerprint", fp[1], "polydb://admin@"+a[0], "-")
	if code != 0 || !strings.Contains(got, "count()\n───────\n1") {
		t.Fatalf("tcp: %d %q %q", code, got, errText)
	}
	// The Unix socket: PEER login as the OS user running the server.
	t.Setenv("POLYDB_PASSWORD", "")
	got, errText, code = runCLI(t, "show databases;\n", "--no-rc", "polydb+unix://"+a[1]+"?db=shop")
	if code != 0 || !strings.Contains(got, "shop") {
		t.Fatalf("unix: %d %q %q", code, got, errText)
	}
	// --trace prints messages without secrets.
	_, errText, _ = runCLI(t, "show databases", "--trace", "polydb+unix://"+a[1])
	if !strings.Contains(errText, "send   0 Hello") || !strings.Contains(errText, "recv   1 ResultHeader") {
		t.Errorf("trace:\n%s", errText)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve ended with %v", err)
	}
	if _, err := os.Stat(a[1]); !os.IsNotExist(err) {
		t.Error("socket file left behind")
	}
}

func TestComplete(t *testing.T) {
	for src, want := range map[string]bool{
		"find p":                 true,
		"find p;":                true,
		"find p |\n":             false,
		"create Person {\n":      false,
		"show \"abc\n":           false,
		"/* comment\n":           false,
		"fnd p\n":                true, // a real error: run it to show the message
		"   \n":                  false,
		"find p; find q | where": false,
	} {
		if got := complete(src); got != want {
			t.Errorf("complete(%q) = %v", src, got)
		}
	}
}

func TestCellText(t *testing.T) {
	props := map[string]value.Value{"since": value.Int(2019)}
	ann := &polydb.Entity{Kind: "Person", Fields: []*polydb.Field{{Name: "name", Value: value.MustText("Ann")}, {Name: "age"}}}
	for _, c := range []struct {
		in   any
		want string
	}{
		{value.MustText("line1\nline2"), `line1\nline2`},
		{value.Int(3), "3"},
		{value.Null, "null"},
		{ann, `Person{name: "Ann"}`},
		{&polydb.Relation{Type: "knows", Props: props}, "knows{since: 2019}"},
		{&polydb.Path{Nodes: []any{ann, ann}, Rels: []*polydb.Relation{{Type: "knows"}}}, "Ann ~knows~> Ann"},
		{polydb.NewBlob("text", 5000, "", nil), "<text, 5000 bytes>"},
		{value.MustText(strings.Repeat("x", 100)), strings.Repeat("x", 59) + "…"},
	} {
		if got := cellText(c.in); got != c.want {
			t.Errorf("cellText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	trace := traceText(&wire.Auth{Mechanism: "SCRAM-SHA-256", Data: []byte("n,,n=admin,r=secretnonce")})
	if strings.Contains(trace, "secretnonce") || !strings.Contains(trace, "redacted") {
		t.Errorf("trace leaks: %s", trace)
	}
}

func TestWhoami(t *testing.T) {
	isolate(t)
	out, _, code := runCLI(t, "", "whoami", "--fingerprint")
	if code != 0 || !regexp.MustCompile(`^[0-9a-f]{64}\n$`).MatchString(out) {
		t.Errorf("whoami: %d %q", code, out)
	}
	again, _, _ := runCLI(t, "", "whoami", "--fingerprint")
	if again != out {
		t.Error("the identity changed between runs")
	}
	if out, _, code := runCLI(t, "", "version"); code != 0 || !strings.HasPrefix(out, "polydb ") {
		t.Errorf("version: %q", out)
	}
	if _, _, code := runCLI(t, "", "frobnicate"); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
}

// syncBuffer is a bytes.Buffer safe for the server goroutine to write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
