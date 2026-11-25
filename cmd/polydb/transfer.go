package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/client"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/value"
)

// exportCmd downloads an export: of a database, the server's security
// settings, or a query's result.
func exportCmd(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var f connectFlags
	f.register(fs)
	database := fs.String("database", "", "export this database")
	sec := fs.Bool("security", false, "export the server's users, roles and grants (with password hashes: keep it secret)")
	noPasswords := fs.Bool("no-passwords", false, "with --security: leave password hashes out")
	q := fs.String("query", "", "export this query's result (in the URL's database)")
	format := fs.String("as", "", "jsonl or native; for --query also csv, dot or graph (default jsonl)")
	output := fs.String("o", "-", "file to write (- for standard output)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := defaultURL()
	if fs.NArg() == 1 {
		url = fs.Arg(0)
	} else if fs.NArg() > 1 {
		fmt.Fprintln(errOut, "usage: polydb export [flags] [url] (--database d | --security | --query q) [-o file]")
		return 2
	}
	var stmt string
	switch {
	case *database != "" && !*sec && *q == "":
		stmt = "export database " + lang.FormatName(*database)
	case *sec && *database == "" && *q == "":
		stmt = "export security"
		if *noPasswords {
			stmt += " without passwords"
		}
	case *q != "" && *database == "" && !*sec:
		stmt = "export (" + *q + ")"
	default:
		fmt.Fprintln(errOut, "polydb export: give one of --database, --security or --query")
		return 2
	}
	if *format != "" {
		stmt += " as " + *format
	}
	if err := runExport(ctx, url, &f, stmt, *output, out, errOut); err != nil {
		fmt.Fprintln(errOut, "polydb export:", err)
		return 1
	}
	return 0
}

func runExport(ctx context.Context, url string, f *connectFlags, stmt, output string, out, errOut io.Writer) error {
	db, _, err := connect(ctx, url, f, errOut)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(ctx, stmt, nil)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return errors.New("the export returned nothing")
	}
	var blob *polydb.Blob
	var entities, relations, blobs, size int64
	if err := rows.Scan(&blob, &entities, &relations, &blobs, &size); err != nil {
		return err
	}
	w := out
	if output != "-" {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		w = file
	}
	n, err := io.Copy(w, blob.Reader())
	if err != nil {
		return err
	}
	if c, ok := w.(*os.File); ok && w != out {
		if err := c.Sync(); err != nil {
			return err
		}
	}
	fmt.Fprintf(errOut, "exported %d entities, %d relations, %d large values: %d bytes\n", entities, relations, blobs, n)
	return nil
}

// importCmd uploads an export and imports it.
func importCmd(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var f connectFlags
	f.register(fs)
	database := fs.String("database", "", "import into this database, which must exist")
	newIDs := fs.Bool("new-ids", false, "give entities and relations new IDs (to copy data into a database that has it)")
	sec := fs.Bool("security", false, "import users, roles and grants")
	replace := fs.Bool("replace", false, "with --security: replace the passwords and keys of users that exist")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url, file := defaultURL(), ""
	switch fs.NArg() {
	case 1:
		file = fs.Arg(0)
	case 2:
		url, file = fs.Arg(0), fs.Arg(1)
	default:
		fmt.Fprintln(errOut, "usage: polydb import [flags] [url] (--database d | --security) file")
		return 2
	}
	var stmt string
	switch {
	case *database != "" && !*sec:
		stmt = "import into database " + lang.FormatName(*database) + " from $export"
		if *newIDs {
			stmt += " with new ids"
		}
	case *sec && *database == "":
		stmt = "import security from $export"
		if *replace {
			stmt += " replace"
		}
	default:
		fmt.Fprintln(errOut, "polydb import: give one of --database or --security")
		return 2
	}
	if err := runImport(ctx, url, &f, stmt, *database, file, in, out, errOut); err != nil {
		fmt.Fprintln(errOut, "polydb import:", err)
		return 1
	}
	return 0
}

// runImport uploads the export into the target database's blob store,
// so it streams; a security export, which is small, goes as a plain
// parameter.
func runImport(ctx context.Context, url string, f *connectFlags, stmt, database, file string, in io.Reader, out, errOut io.Writer) error {
	r := in
	if file != "-" {
		fh, err := os.Open(file)
		if err != nil {
			return err
		}
		defer fh.Close()
		r = fh
	}
	db, _, err := connect(ctx, url, f, errOut)
	if err != nil {
		return err
	}
	defer db.Close()
	var param any
	if c, ok := db.(*client.Conn); ok && database != "" {
		if _, err := c.Exec(ctx, "use "+lang.FormatName(database), nil); err != nil {
			return err
		}
		if param, err = c.Upload(ctx, r, "bytes", ""); err != nil {
			return err
		}
	} else {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		param = data
	}
	rows, err := db.Query(ctx, stmt, polydb.Params{"export": param})
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	cols := rows.Columns()
	var parts []string
	for i, v := range rows.Values() {
		n, _ := v.(value.Int)
		parts = append(parts, strconv.FormatInt(int64(n), 10)+" "+cols[i].Name)
	}
	fmt.Fprintln(errOut, "imported:", joinComma(parts))
	return nil
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
