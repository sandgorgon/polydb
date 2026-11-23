package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
)

// initConfig is what init needs, parsed from flags.
type initConfig struct {
	dataDir    string
	key        bool
	generate   bool
	iterations int // tests lower it
}

// initCmd sets up the first admin (spec/protocol.md §3.3).
func initCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	c := initConfig{}
	fs.StringVar(&c.dataDir, "data-dir", dataDir(), "the server's data directory")
	fs.BoolVar(&c.key, "key", false, "also register this user's 9auth identity for admin")
	fs.BoolVar(&c.generate, "generate", false, "generate admin's password and print it once, instead of asking")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(errOut, "polydb init: unexpected arguments:", fs.Args())
		return 2
	}
	if err := initData(c, out); err != nil {
		fmt.Fprintln(errOut, "polydb init:", err)
		return 1
	}
	return 0
}

// initData gives admin a password, and optionally this user's key, in the
// data directory, creating it if needed. It refuses once admin has a
// password or a key: from then on, admin changes them with alter user.
func initData(c initConfig, out io.Writer) error {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	eng, err := engine.Open(engine.Options{Dir: c.dataDir, PasswordIterations: c.iterations})
	if err != nil {
		return fmt.Errorf("%w (is the server running? stop it first)", err)
	}
	defer eng.Close()
	sec := eng.Security()
	if u, _ := sec.User(security.AdminUser); u.Creds != nil || len(u.Keys) > 0 {
		return errors.New("already initialized: admin has a password or key (change them with alter user admin)")
	}

	var fingerprint string
	if c.key {
		id, err := auth9.Load()
		if err != nil {
			return fmt.Errorf("--key: %w", err)
		}
		fingerprint = id.Fingerprint()
	}
	pw, shown := os.Getenv("POLYDB_ADMIN_PASSWORD"), false
	if pw == "" && !c.generate {
		if pw, err = askNewPassword(); err != nil && !errors.Is(err, errNoTerminal) {
			return err
		}
	}
	if pw == "" {
		b := make([]byte, 18)
		rand.Read(b)
		pw, shown = base64.RawURLEncoding.EncodeToString(b), true
	}
	if err := sec.SetPassword(security.AdminUser, pw); err != nil {
		return err
	}
	if fingerprint != "" {
		if err := sec.AddKey(security.AdminUser, fingerprint, "polydb init"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "initialized %s\n", c.dataDir)
	if shown {
		fmt.Fprintf(out, "admin password (shown once): %s\n", pw)
	}
	if fingerprint != "" {
		fmt.Fprintf(out, "admin key: %s\n", fingerprint)
	}
	return nil
}

var errNoTerminal = errors.New("no terminal")

// askNewPassword asks for a password twice on the terminal. An empty
// answer means "generate one".
func askNewPassword() (string, error) {
	if t, err := tty(); err != nil {
		return "", errNoTerminal
	} else {
		t.Close()
	}
	for {
		pw, err := askPassword("admin password (empty to generate one): ")
		if err != nil || pw == "" {
			return "", err
		}
		again, err := askPassword("again: ")
		if err != nil {
			return "", err
		}
		if again == pw {
			return pw, nil
		}
		if t, err := tty(); err == nil {
			fmt.Fprintln(t, "the passwords differ; try again")
			t.Close()
		}
	}
}
