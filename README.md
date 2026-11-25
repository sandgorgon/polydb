# PolyDB

PolyDB is a database for data that doesn't fit one shape. It keeps
records, the links between them, documents, text and files together,
and answers questions about all of them with one language, **Rhizome**:

```rhizome
define kind Person  { name: text, skills: set<text> };
define kind Project { title: text, spec: doc<json>? };

create Person  @ana  { name: "Ana", skills: {"go", "sql"} };
create Project @site { title: "Website", spec: "{\"deadline\": \"2027-03-01\"}" };
relate @ana ~works_on{since: 2025}~> @site;

find Person p
  | where p.skills has "go"
  | follow p ~works_on r~> Project j
  | show p.name, j.title, r.since, j.spec->deadline;
```

- **Records with rules.** Describe your data with *kinds* (typed fields,
  checks, unique values, defaults), or store loose entities with no
  kind at all, and mix the two.
- **A graph built in.** Link any two entities with named *relations*,
  and follow them in queries, across as many steps as you need.
- **Rich values.** Exact decimals, times, lists, sets and maps,
  JSON/XML/CSV documents you can query inside, images and files of
  gigabytes (streamed, never loaded whole), and work queues.
- **One readable language.** Queries are pipelines that read top to
  bottom. Errors say what's wrong and suggest a fix.
- **Safe by default.** Transactions, history of past values, encrypted
  connections, users and permissions down to each kind, online backups.
- **Server or library.** Run `polydb serve`, or use it inside a Go
  program with no server. The same code works with both.
- **Pure Go.** One static binary, no other services.

## When to use it

PolyDB suits applications whose data is part records, part graph, part
documents: knowledge bases, catalogues, case management, project and
asset tracking, configuration stores, content with relationships.

It's not the right tool for heavy analytics over billions of rows, for
data spread over many machines (a server is one machine in version 1),
or when you need SQL for existing tools.

## Try it in five minutes

Install (Go 1.26 or later, or download a release):

```sh
go install github.com/sandgorgon/polydb/cmd/polydb@latest
```

Open a shell on a scratch database in memory:

```sh
polydb --memory
```

and type:

```rhizome norun
define kind Person { name: text, age: int? };
create Person { name: "Ana", age: 34 };
create Person { name: "Ben", age: 27 };
find Person p | where p.age > 30 | show p.name;
```

To keep your data, run a server with `polydb serve` and connect to it
with `polydb` from another terminal. [Getting started](docs/getting-started.md)
walks through it.

From Go:

```go
db, err := polydb.Open("./data", nil) // or polydb.OpenMemory(nil)
if err != nil {
	log.Fatal(err)
}
defer db.Close()
rows, err := db.Query(ctx, `find Person p | where p.age > $min | show p.name`, polydb.Params{"min": 30})
```

## Documentation

- [Getting started](docs/getting-started.md): a first session, step by step
- [Concepts](docs/concepts.md): the ideas, and PolyDB for SQL users
- [A tour of Rhizome](docs/rhizome-tour.md): the language by example
- [Using PolyDB from Go](docs/embedding.md) and [connecting to a server](docs/client.md)
- [Running PolyDB](docs/operations.md): service, users, backups, limits
- [The `polydb` command](docs/cli.md), [FAQ](docs/faq.md), [glossary](docs/glossary.md)
- [All documentation](docs/README.md), and the [specifications](spec/)

## Status and compatibility

PolyDB follows semantic versioning. Within version 1, data, backups,
exports, queries and the Go API keep working across upgrades;
[compatibility](docs/compatibility.md) lists exactly what's promised.
