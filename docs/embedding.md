# Using PolyDB from Go

PolyDB is a set of Go packages, so a Go program can use it two ways:

- **Embedded:** the database runs inside your program. No server to
  install or run; the data lives in a directory (or in memory). Good
  for applications, tools and tests. This page.
- **Through a server:** your program connects to a PolyDB server over
  the network. Good when several programs or people share the data.
  See [Connecting to a server from Go](client.md).

Both give you the same `polydb.DB` interface, so code written for one
works with the other: you can develop against an embedded database and
deploy against a server, or the other way round.

```sh
go get github.com/sandgorgon/polydb
```

## Open a database

```go
import "github.com/sandgorgon/polydb"

db, err := polydb.Open("./data", nil) // created if it doesn't exist
if err != nil {
	log.Fatal(err)
}
defer db.Close()
```

- `polydb.Open(dir, opts)` keeps the data in `dir`. Only one program can
  have a directory open at a time; a second `Open` fails.
- `polydb.OpenMemory(opts)` keeps it in memory only, gone at `Close`:
  ideal for tests.

Statements run in a database called `main`, created for you. Your
program acts as the database's administrator, so it can do anything.

`opts` is `nil` for the defaults, or a `*polydb.Options`:

| Option | Default | Meaning |
|---|---|---|
| `Database` | `"main"` | the database to use, created if missing |
| `Relaxed` | `false` | faster commits for bulk loading; a crash may lose the last moments of changes, never half of one |
| `CacheSize` | 64 MiB | memory for caching data read from disk |
| `StatementTimeout` | none | stop any statement that runs longer |
| `MaxMemory` | 1 GiB | the most memory one statement may use; negative for no limit |
| `BackupDir` | `<dir>/backups` | where `backup` and `export … to` statements write files |

## Run statements

`Exec` runs statements that change things; `Query` runs a query and
returns its rows. Both take Rhizome text and parameters.

```go
ctx := context.Background()

_, err = db.Exec(ctx, `
	define kind Person { name: text, age: int };
	create Person { name: "Alice", age: 34 };
	create Person { name: "Bob", age: 27 };
	create Person { name: "Carol", age: 41 }`, nil)
if err != nil {
	log.Fatal(err)
}

rows, err := db.Query(ctx,
	`find Person p | where p.age > $min | sort p.name | show p.name, p.age`,
	polydb.Params{"min": 30})
if err != nil {
	log.Fatal(err)
}
defer rows.Close()
for rows.Next() {
	var name string
	var age int
	if err := rows.Scan(&name, &age); err != nil {
		log.Fatal(err)
	}
	fmt.Println(name, age) // Alice 34, then Carol 41
}
if err := rows.Err(); err != nil {
	log.Fatal(err)
}
```

**Always pass values as parameters** (`$min` with
`polydb.Params{"min": 30}`), never by pasting them into the text. That
keeps user input from changing what the statement does.

Go values convert as you'd expect: `string` → `text`, `int` → `int`,
`float64` → `float`, `bool`, `[]byte` → `bytes`, `time.Time` → `time`,
`time.Duration` → `duration`, slices → lists, maps with string keys →
maps, and a nil pointer → `null`. For other types (decimals, sets,
dates) pass a value from the `value` package, for example
`d, _ := value.ParseDecimal("19.99")`.

`Scan` converts the other way. It accepts pointers to `string`, `int`,
`int64`, `float64`, `bool`, `[]byte`, `time.Time`, `time.Duration`,
`any`, `value.Value`, `value.ID`, and `*polydb.Entity`,
`*polydb.Relation`, `*polydb.Path` or `*polydb.Blob` for whole things.
A column that can be `null` needs a pointer to a pointer:

```go
var age *int64 // nil when the person has no age
err := rows.Scan(&age)
```

A script with several queries returns several result sets: read one
with `Next`, then move on with `rows.NextResultSet()`.

## Transactions

```go
tx, err := db.Begin(ctx, polydb.TxOptions{})
if err != nil {
	return err
}
defer tx.Rollback(ctx) // undoes everything unless Commit ran first (its error is then harmless)
if _, err := tx.Exec(ctx, `find Account x | where x.owner = "a" | update x set balance = x.balance - 40`, nil); err != nil {
	return err
}
if _, err := tx.Exec(ctx, `find Account x | where x.owner = "b" | update x set balance = x.balance + 40`, nil); err != nil {
	return err
}
return tx.Commit(ctx)
```

When two transactions change the same data at once, one fails with a
`conflict` error, and running it again is the right fix. `RetryTx` does
that for you:

```go
err := polydb.RetryTx(ctx, db, func(tx polydb.Tx) error {
	_, err := tx.Exec(ctx, `…`, nil)
	return err
})
```

`polydb.TxOptions{ReadOnly: true}` gives a consistent view for several
reads.

## Errors

Every error from PolyDB is a `*polydb.Error` with a `Code`, a
`Message`, and the line and column for errors in a statement. Check the
code, not the message, which may be reworded between versions:

```go
_, err := db.Exec(ctx, `create Person { name: "Dup", email: $e }`, polydb.Params{"e": email})
switch polydb.CodeOf(err) {
case "":
	// it worked
case "constraint":
	fmt.Println("that email is taken")
default:
	return err
}
```

The codes are listed in the [FAQ](faq.md#what-do-the-error-codes-mean).

## Large values

Text, bytes and images can be up to gigabytes. Don't read them into
memory: stream them.

**Storing** one from any `io.Reader` (a file, an HTTP body):

```go
f, err := os.Open("scan.png")
if err != nil {
	return err
}
defer f.Close()
img, err := db.Upload(ctx, f, "image", "") // "text", "bytes" or "image"
if err != nil {
	return err
}
_, err = db.Exec(ctx, `create Scan { file: $img }`, polydb.Params{"img": img})
```

The upload goes straight into the database's storage, and the
statement then stores it without copying. Use it within an hour.

**Reading** one: a query returns large values as a `*polydb.Blob`,
whose content is read only when you read it:

```go
var file *polydb.Blob
if err := rows.Scan(&file); err != nil {
	return err
}
out, _ := os.Create("copy.png")
defer out.Close()
_, err = io.Copy(out, file.Reader()) // streams, a piece at a time
```

A `Blob` is also an `io.ReaderAt`, for reading parts of it.

## Shutting down

`db.Close()` rolls back open transactions and closes the database
cleanly. Data that was committed is safe even if the program crashes;
`Close` just makes the next `Open` quicker.

## Runnable examples

The package's examples (`go doc github.com/sandgorgon/polydb`, or
[example_test.go](../example_test.go)) are complete programs, checked
by PolyDB's tests: a first query, data on disk, relations, a
transaction, and a large value in and out.

## Where next

- [Connecting to a server from Go](client.md)
- [A tour of Rhizome](rhizome-tour.md): what to put in those strings
- [Compatibility](compatibility.md): what stays the same across versions
