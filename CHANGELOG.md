# Changelog

PolyDB follows [semantic versioning](https://semver.org); what each kind
of release may change is in [compatibility](docs/compatibility.md).

## 1.0.0 (unreleased)

The first release. Everything below is new.

### Data

- **Entities, kinds and fields.** Entities with permanent IDs, metadata
  (created, updated, version, owner, labels, props) and documented
  fields that have their own IDs. Kinds describe fields with types,
  `unique`, `default`, `check` and optional (`?`) fields; kinds are
  extensible (entities may `add` fields of their own) or `closed`, and
  entities may have no kind at all.
- **Relations.** Named, directed links between entities or fields, with
  their own IDs and properties, indexed both ways. Relation types are
  optional and add endpoint kinds, `max in` / `max out` limits and
  `on delete cascade | restrict`.
- **Types.** `bool`, `int`, `bigint`, `float`, exact `decimal(P,S)`,
  NFC-normalized `text`, `bytes` with a media type, `image`, `time`,
  `date`, `duration`, `id`, `ref<Kind>` (with `on delete dangle |
  restrict`), `doc<json|xml|csv|tsv>` with typed derived formats and
  `->` paths, `list`, `array`, `set`, `map`, `pair`, `record`, `union`,
  `any`, and FIFO, LIFO, ring and priority queues with blocking `pop …
  wait`. No silent conversions; a total, deterministic order of values.
- **History.** Opt-in per field or kind, with retention (`history keep
  90 * 24h`), `history @x.field` and `find … as of t"…"` queries, and a
  background sweep.
- **Database metadata.** Owner and creation time, plus a note, labels
  and props that a database's administrators set with `alter database`
  and queries read as `database::…`.

### The Rhizome language

- One language for definitions, data, queries, transactions, security
  and introspection. Queries are pipelines: `find`, `where`, `follow`,
  `match`, `let`, `group by`, `having`, `sort`, `distinct`, `skip`,
  `limit`, `show`, `bind`, `union`, ending if wanted with `update`,
  `delete`, `unrelate`, `push` or `pop` applied to every row.
- Graph patterns: directions, alternatives, any type, bound relations,
  property filters, multi-hop (`*1..3`) paths that never revisit an
  entity, and whole-path variables.
- Full-text search with `search(field, "query")`: words, phrases,
  prefixes, exclusions and `OR`, case- and accent-insensitive.
- Parameters (`$name`), session handles (`@name`), aggregates, and
  functions for text, numbers, time, collections, queues, graphs and
  documents.
- `describe`, `show` (kinds, relations, indexes, stats, alters, users,
  sessions, limits, slow queries and more), `explain` and `explain
  analyze`. Errors have stable codes, positions and suggested fixes.

### Storage

- A write-ahead log and LSM tree per database, with crash recovery,
  checksummed files and snapshot isolation (MVCC). Commits are durable
  by default; `relaxed` durability for bulk loads never loses part of a
  commit.
- Large values (over 16 KiB) in content-addressed, append-only blob
  segments, streamed in and out without being held in memory, with
  garbage collection that respects snapshots, history and uploads.
- Background `alter kind` for large kinds, and `rebuild index`.

### Indexes and queries

- Value, multi-field, unique, document-path and full-text indexes,
  built in the background.
- A cost-based planner using sampled statistics (`analyze kind`):
  index ranges and lookups, join ordering, early stops for
  `sort … limit`, and index ranges that answer conditions exactly so
  rows aren't checked again. Plans never change results.

### Server, protocol and security

- `polydb serve`: a binary, multiplexed protocol over TLS 1.3 (TCP port
  7377) and Unix sockets, with streaming results, cancellation, flow
  control, compression and keepalives.
- Logins by SCRAM-SHA-256 with channel binding, by 9auth key, or by
  operating-system user on the Unix socket. Servers are trusted by
  fingerprint (pinned or trust-on-first-use) or by a CA certificate.
- Users, roles, and `read`, `write`, `relate`, `define` and `admin`
  permissions on the server, a database or a kind; entities of kinds a
  user can't read are invisible.
- Resource limits: statement time and memory (as session ceilings),
  connections in total and per user, open transactions, upload size
  and a per-user upload quota; `show limits`.
- A slow-query log with each statement's plan.

### Operations

- Online `backup database` and `restore database` (and `polydb restore`
  offline), checked file by file.
- `export` and `import` of databases (JSON Lines or native), query
  results (also CSV, Graphviz DOT and JSON graphs) and users,
  permissions and keys, streamed both ways, with `with new ids` to copy.
- A configuration file (`polydb serve --config`), release archives for
  Linux and macOS on amd64 and arm64 with checksums, an example
  configuration and a hardened systemd unit.

### Go packages

- `polydb`: the application interface (`DB`, `Tx`, `Rows`, `Stmt`,
  `RetryTx`, typed `Scan`) and an embedded database
  (`polydb.Open`, `polydb.OpenMemory`) with streaming uploads.
- `client`: the same interface over the network (`client.Dial`).
- `value`: PolyDB's value types.
- Everything else is internal; see [compatibility](docs/compatibility.md).

### Tools and documentation

- The `polydb` command: an interactive shell with parameters and
  transactions, scripts, `init`, `export`, `import`, `restore`,
  `whoami` and `version`.
- A README, a getting-started guide, concepts, a language tour, guides
  for Go, the CLI and operations, an FAQ and a glossary; every example
  in them is run by the tests. Full specifications for the types, the
  language and the protocol.

### Known limitations

- One server is one machine: no replication or partitioning yet.
- A query reads one database; text sorts by code point (no
  language-aware collation); search has no stemming or relevance
  ranking yet.
- Kinds have no inheritance; relation types set maximums, not minimums.
- Metadata can't be indexed.
- Large lists, sets and maps are stored whole, not element by element.

### Planned after 1.0

- A 9P interface for browsing databases as files, a terminal UI,
  replication and partitioning, inference rules, and storage layouts
  tuned to real workloads.
