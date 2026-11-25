// Command polydb is PolyDB's server and command-line client.
//
//	polydb init [flags]               set up the first admin
//	polydb serve [flags]              run the server
//	polydb [flags] [url]              open an interactive shell
//	polydb run [flags] [url] file.rhz run a script
//	polydb whoami [--fingerprint]     show this machine's 9auth identity
//	polydb version
//
// The URL is polydb://user@host:port/database or
// polydb+unix:///path/to.sock?db=database; it defaults to $POLYDB_URL,
// then to the local server's Unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/client"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage:
  polydb init [--key] [--generate]   set up the server's first admin (polydb init -h for flags)
  polydb serve [flags]               run the server (polydb serve -h for flags)
  polydb restore [flags] backup-dir  restore a database into a stopped server's data directory
  polydb export [flags] [url] (--database d | --security | --query q) [-o file]
  polydb import [flags] [url] (--database d | --security) file
  polydb [flags] [url]               open an interactive shell
  polydb run [flags] [url] file.rhz  run a script ("-" reads standard input)
  polydb whoami [--fingerprint]      show this machine's 9auth identity
  polydb version

url: polydb://user@host:port/database or polydb+unix:///path/to.sock?db=database
     (default: $POLYDB_URL, then the local server's Unix socket)
`

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !strings.Contains(args[0], "://") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "shell":
		return shellCmd(ctx, args, in, out, errOut)
	case "init":
		return initCmd(args, out, errOut)
	case "serve":
		return serveCmd(ctx, args, out, errOut)
	case "restore":
		return restoreCmd(ctx, args, out, errOut)
	case "export":
		return exportCmd(ctx, args, out, errOut)
	case "import":
		return importCmd(ctx, args, in, out, errOut)
	case "run":
		return runCmd(ctx, args, in, out, errOut)
	case "whoami":
		return whoamiCmd(args, out, errOut)
	case "version":
		fmt.Fprintln(out, "polydb", client.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return 0
	}
	fmt.Fprintf(errOut, "polydb: unknown command %q\n\n%s", cmd, usage)
	return 2
}

func whoamiCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fpOnly := fs.Bool("fingerprint", false, "print only the fingerprint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id, err := auth9.Load()
	if err != nil {
		fmt.Fprintln(errOut, "polydb:", err)
		return 1
	}
	if *fpOnly {
		fmt.Fprintln(out, id.Fingerprint())
		return 0
	}
	dir, _ := auth9.ConfigDir()
	fmt.Fprintf(out, "fingerprint: %s\nidentity:    %s\n", id.Fingerprint(), dir)
	fmt.Fprintln(out, "An admin registers it with: alter user <name> add key \""+id.Fingerprint()+"\" as \"<label>\";")
	return 0
}

// dataDir is where the server keeps its identity and socket:
// $POLYDB_DATA, or ~/.local/share/polydb.
func dataDir() string {
	if d := os.Getenv("POLYDB_DATA"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "polydb-data"
	}
	return filepath.Join(home, ".local", "share", "polydb")
}

// defaultSocket is the server's Unix socket under the data directory.
func defaultSocket() string { return filepath.Join(dataDir(), "polydb.sock") }

// defaultURL is $POLYDB_URL, or the local server's Unix socket.
func defaultURL() string {
	if u := os.Getenv("POLYDB_URL"); u != "" {
		return u
	}
	return "polydb+unix://" + defaultSocket()
}

func osUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
