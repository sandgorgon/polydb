# Questions and common problems

## Getting connected

### `polydb` says it can't connect: "dial unix … no such file or directory"

With no URL, `polydb` connects to a server on this computer through its
Unix socket, so no server is running, or it uses a different data
directory. Start one with `polydb serve` (in another terminal), give the
server's URL (`polydb polydb://you@host/db`), or try PolyDB without a
server: `polydb --memory`.

### "the socket path … is longer than Unix sockets allow"

Unix socket paths are limited to about 100 characters. Start the server
with a shorter `--socket` path, or a shorter `--data-dir`.

### Login fails with `auth_failed`

The user name or password is wrong, or the user has no password yet.
For security the error never says which. Check with an administrator
(`show users`). Repeated failures from one address are slowed down for
a while.

### The shell asks me to trust a fingerprint

That's normal the first time you connect to a server over the network.
Compare the fingerprint with the one the server printed when it
started (an administrator can tell you). If they match, answer `y`.
If a server you already trust shows a *different* fingerprint, PolyDB
refuses to connect: the server's identity changed or someone is
impersonating it. Ask the administrator before doing anything else.

### "only this OS user can log in, over the Unix socket (run polydb init)"

The server has no admin password yet. Until it has, only the
operating-system user running it can log in, through the socket. Stop
the server, run `polydb init`, and start it again.

## Writing statements

### "expected a field name, found the keyword role"

Some common words are reserved by the language, such as `role`,
`user`, `where`, `from`, `to`, `index` and `limit`
([the full list](../spec/rhizome.md#15-keywords)), so they can't be used
as names directly. Pick another name (`job` instead of `role`), or write
the name in backticks: `` `role` ``.

### "kind Person has no field emial (did you mean email?)"

A kind only accepts the fields it declares, which catches typos. If you
meant a new field for that one entity, add it on purpose:
`update @x add nickname : text = "Bo"`. For every entity of the kind,
`alter kind Person add nickname : text?`.

### "keywords are lowercase"

Write `find`, `where`, `create`: Rhizome keywords are lowercase only.

### `2 + 2.5` is an error

PolyDB never mixes number types silently, so a whole number never turns
into a fraction behind your back. Write `2.0 + 2.5`, or
`cast(x as float) + 2.5`. For money, use `decimal` (`19.99d`), which is
exact.

### My query returns nothing for a field I know exists

Check its spelling: for kinds that allow extra fields, reading a field
that no entity has isn't an error, it's just `null`. Also check `null`
rules: `p.age > 30` is false when `age` is missing.

### Why `show` at the end and not `select` at the start?

Queries are pipelines that read top to bottom: first say where rows
come from (`find`), then filter and follow links, then say what to
show. Each step can use what the steps before it produced.

### A transaction says `txn_aborted`

A statement inside the transaction failed, so PolyDB stops the rest
from running as if it had worked. Run `rollback`, fix the problem, and
start again. In the shell, the prompt shows `!` in this state.

### A commit fails with `conflict`

Another session changed the same data since your transaction began.
Nothing of yours was written. Run the transaction again; from Go,
`polydb.RetryTx` does that automatically.

## Data and performance

### A query is slow

Put `explain` in front of it. It shows how PolyDB will find each
variable's entities:

- `scan kind Person` reads every Person. If the query filters on a
  field, an index on it helps: `define index person_age on Person(age)`.
- `index person_age for p.age > 30, exact` is the best case: the index
  alone decides which entities match, so they aren't checked again.
- A warning about variables that "no condition links" means every
  combination is formed (`find Person a, Person b` with no `where`
  joining them). Add a condition, or a `follow`.
- `search()` without a text index reads every text. Add
  `define text index …`.

`explain analyze` runs the query and shows the real counts and times
of each step.

### A query stops with a `limit` error

It hit a limit set to protect the server: running time
(`statement-timeout`), memory (`max-memory`), or path depth
(`max_depth`). The message says which. Usually the fix is a narrower
`where`, a `limit`, or an index. `show limits` lists them; an
administrator can raise them.

### How big can values be?

Text up to 64 MiB, bytes and images up to 4 GiB. Large values are
streamed in and out, never loaded whole, unless a query works on their
content (`upper(p.text)`), which counts against the statement's memory
limit.

### How does my application know which version of its data a database has?

Keep the version in the database's metadata, and read it when the
application starts:

```rhizome norun
alter database shop set ::props.schema_version = 3;
describe database shop;
```

`describe database` lists the props, and any query can read
`database::props.schema_version`. When the application finds an older
version, it runs its upgrade steps and then sets the new number. An
administrator of the database must run the `alter`.

### Can a query use two databases?

Not in version 1. Each session works in one database at a time.

### Is there an ORM or SQL support?

No. PolyDB is queried with Rhizome. It's designed for data that's part
records, part graph, part documents, which SQL doesn't express well.

## What do the error codes mean?

Every error has a code; programs should check the code, since messages
may be reworded between versions.

| Code | Means | What to do |
|---|---|---|
| `syntax` | the statement can't be read | check the position shown |
| `type` | a value has the wrong type for where it's used | cast it, or fix the value |
| `unknown_field` | the field doesn't exist | check spelling; `add` it if it's new |
| `not_found` | no such database, kind, handle or entity | check the name; `show kinds`, `show databases` |
| `constraint` | a rule would be broken: `unique`, `check`, a required field, a relation maximum, a delete blocked by `restrict` | change the data, or the rule |
| `permission` | your user isn't allowed to do that | ask an administrator for a grant |
| `conflict` | another transaction changed the same data | run it again |
| `limit` | a time, memory, size or connection limit was reached | narrow the work, or ask for a higher limit |
| `txn_aborted` | an earlier statement in this transaction failed | `rollback` |
| `auth_failed` | login failed | check user and password |
| `connection_lost` | the connection to the server dropped | reconnect |
| `cancelled` | you cancelled it (Ctrl-C) | nothing |
| `lease_expired` | an uploaded or fetched large value expired before it was used | upload or query it again |
| `stale_statement` | a prepared statement's kinds changed | prepare it again (the Go client does it for you) |
| `internal` | a bug, or a problem with the disk | check the server's log; please report it |

## Where next

- [Glossary](glossary.md): every term in one place
- [The language reference](../spec/rhizome.md): the exact rules
