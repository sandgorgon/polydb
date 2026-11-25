# PolyDB documentation

PolyDB is a database that keeps records, graphs, documents, text and
files together, and answers questions about all of them with one
language, Rhizome. These pages explain it from the beginning.

## Start here

**Trying it out?**

1. [Getting started](getting-started.md): install, start a server, and
   build a small database in fifteen minutes.
2. [Concepts](concepts.md): the ideas behind it, with a comparison for
   people who know SQL.
3. [A tour of Rhizome](rhizome-tour.md): the language, step by step.

**Building an application in Go?**

1. [Concepts](concepts.md)
2. [Using PolyDB from Go](embedding.md): a database inside your program.
3. [Connecting to a server from Go](client.md)
4. [A tour of Rhizome](rhizome-tour.md)

**Running it for others?**

1. [Running PolyDB](operations.md): installing as a service, TLS, users
   and permissions, backups, limits, monitoring and upgrading.
2. [The `polydb` command](cli.md): every command and setting.

## Everything

| Page | What's in it |
|---|---|
| [Getting started](getting-started.md) | a first hands-on session |
| [Concepts](concepts.md) | entities, kinds, relations, types, indexes, transactions, permissions; PolyDB for SQL users |
| [A tour of Rhizome](rhizome-tour.md) | the language by example |
| [The `polydb` command](cli.md) | the shell, scripts, the server and its settings, export and import |
| [Using PolyDB from Go](embedding.md) | `polydb.Open`, queries, transactions, errors, large values |
| [Connecting to a server from Go](client.md) | `client.Dial`, trusting servers, logging in |
| [Running PolyDB](operations.md) | operating a server |
| [Compatibility](compatibility.md) | what stays the same across versions |
| [FAQ](faq.md) | common problems and what error messages mean |
| [Glossary](glossary.md) | every term in one place |

## The reference

The pages above teach; the specifications are the exact rules, for
when you need every detail:

- [The Rhizome language](../spec/rhizome.md)
- [Types and values](../spec/types.md)
- [The network protocol](../spec/protocol.md)

Every Rhizome example in these pages is run by PolyDB's tests
(`go test -run TestDocs`), so they work as written.
