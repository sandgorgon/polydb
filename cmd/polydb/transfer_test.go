package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// polydb export and import move a database through a running server,
// and a query's result out of it.
func TestExportImportCommands(t *testing.T) {
	home := isolate(t)
	cfg := serveConfig{listen: "", dataDir: filepath.Join(home, "data"), iterations: 4096}
	_, unix, _, stop := startServe(t, cfg)
	defer stop()
	url := "polydb+unix://" + unix
	big := strings.Repeat("x", 100<<10)
	script := `create database src; use src; define kind K { a: int, s: text };
		create K { a: 1, s: "` + big + `" }; create K { a: 2, s: "small" }; create database dst`
	if _, errText, code := runCLI(t, script, "run", url, "-"); code != 0 {
		t.Fatalf("setup: %s", errText)
	}
	file := filepath.Join(home, "src.jsonl")
	_, errText, code := runCLI(t, "", "export", "--database", "src", "-o", file, url)
	if code != 0 || !strings.Contains(errText, "exported 2 entities") {
		t.Fatalf("export: %d %s", code, errText)
	}
	if _, errText, code := runCLI(t, "", "export", "--database", "src", "-o", file, url); code == 0 {
		t.Errorf("export over an existing file: %s", errText)
	}
	_, errText, code = runCLI(t, "", "import", "--database", "dst", url, file)
	if code != 0 || !strings.Contains(errText, "2 entities") {
		t.Fatalf("import: %d %s", code, errText)
	}
	got, errText, code := runCLI(t, "use dst; find K k | sort k.a | show k.a, len(k.s)", "run", url, "-")
	if code != 0 || !strings.Contains(got, "102400") || !strings.Contains(got, "5") {
		t.Fatalf("imported data: %d %q %s", code, got, errText)
	}
	// A query as csv, to standard output.
	got, errText, code = runCLI(t, "", "export", "--query", "find K k | sort k.a | show k.a", "--as", "csv", url+"?db=src")
	if code != 0 || got != "k.a\n1\n2\n" {
		t.Fatalf("csv export: %d %q %s", code, got, errText)
	}
	// Security without passwords, back in.
	secFile := filepath.Join(home, "sec.jsonl")
	if _, errText, code := runCLI(t, "", "export", "--security", "--no-passwords", "-o", secFile, url); code != 0 {
		t.Fatalf("security export: %s", errText)
	}
	data, _ := os.ReadFile(secFile)
	if !strings.Contains(string(data), `"polydb":"security"`) || strings.Contains(string(data), "stored_key") {
		t.Errorf("security export:\n%s", data)
	}
	if _, errText, code := runCLI(t, "", "import", "--security", url, secFile); code != 0 || !strings.Contains(errText, "kept") {
		t.Fatalf("security import: %d %s", code, errText)
	}
}
