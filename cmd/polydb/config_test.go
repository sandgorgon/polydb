package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeConfigFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "polydb.conf")
	os.WriteFile(conf, []byte(`# a comment
listen = 127.0.0.1:9000
data-dir = "`+filepath.Join(dir, "data")+`"

max-memory = 2GiB
upload-quota = 500MB
statement-timeout = 30s
compression = true
max-connections = 50
`), 0o600)
	c, err := parseServe([]string{"--config", conf, "--max-connections", "70"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != "127.0.0.1:9000" || c.dataDir != filepath.Join(dir, "data") || !c.compression {
		t.Errorf("settings: %+v", c)
	}
	if c.limits.MaxMemory != 2<<30 || c.limits.UploadQuota != 500e6 || c.limits.StatementTimeout != 30*time.Second {
		t.Errorf("limits: %+v", c.limits)
	}
	if c.limits.MaxConnections != 70 {
		t.Errorf("the flag didn't override the file: %d", c.limits.MaxConnections)
	}
	// -config=file works too.
	if c, err := parseServe([]string{"-config=" + conf}, io.Discard); err != nil || c.limits.MaxConnections != 50 {
		t.Errorf("-config=: %v %d", err, c.limits.MaxConnections)
	}

	for body, want := range map[string]string{
		"nope = 1\n":                 `:1: unknown setting "nope"`,
		"\n\nmax-memory = lots\n":    ":3: max-memory:",
		"listen\n":                   ":1: expected key = value",
		"statement-timeout = soon\n": ":1: statement-timeout:",
	} {
		os.WriteFile(conf, []byte(body), 0o600)
		if _, err := parseServe([]string{"--config", conf}, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
	if _, err := parseServe([]string{"--config", filepath.Join(dir, "missing")}, io.Discard); err == nil {
		t.Error("a missing config file")
	}
}

func TestParseSize(t *testing.T) {
	for s, want := range map[string]int64{"1024": 1024, "64KiB": 64 << 10, "1.5GiB": 3 << 29, "2 MB": 2e6, "-1": -1, "512MiB": 512 << 20} {
		if got, err := parseSize(s); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "lots", "1.5B", "9999999TiB"} {
		if _, err := parseSize(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// The example configuration shipped with releases parses.
func TestExampleConfig(t *testing.T) {
	c, err := parseServe([]string{"--config", "../../packaging/polydb.conf"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.dataDir != "/var/lib/polydb" || c.socket != "/run/polydb/polydb.sock" || c.limits.MaxMemory != 1<<30 {
		t.Errorf("example config: %+v", c)
	}
}
