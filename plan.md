# PolyDB Plan

Status: **Phase 0 (specifications) complete (2026-11-22).** Phase 1 (values, model, in-memory storage, engine Go API) and Phase 2 (Rhizome language) complete and merged into `master`. Phase 3 (security, wire protocol, authentication, server, Go client, CLI) complete and merged into `master`. Phase 4 (persistence: write-ahead log, LSM storage, crash safety, on-disk databases and users, `polydb init`, field history) complete and merged into `master`. Phase 5 (secondary indexes built in the background, full-text search, query planner, `explain` with plans) complete and merged into `master`. Phase 6 (large data: history sweep, planner statistics, bulk loads, blob store with streamed large values, online backup and restore, export and import, background alter kind, slow-query log, hardening) complete and merged into `master`. Next: Phase 7.
Source requirements: [req.md](req.md)

---

## 1. Core data model

A server hosts **multiple independent databases** (decided). Each database is its own graph, with its own kinds, indexes and permissions.

```
Database ── a directed graph
 ├─ Entity     id, kind (optional), metadata{created, updated, version, labels, owner...}, fields[]
 │   └─ Field  id, name, doc, type, value, relations[]
 └─ Relation   id, type/name, from (entity|field), to (entity|field), direction, properties, doc
```

- **Relations are first-class objects** and can attach to **both entities and fields** (decided). Each relation has its own ID, a type and optional properties (for example `knows {since: 2019}`). Because relations can attach to fields, a field is a node in the graph too.
- **IDs are 128-bit and time-ordered,** generated locally. They are unique without a central coordinator, which matters later when the database is split across machines.
- **Kinds are optional schemas** (decided). A kind is a template: field names, types, docs and constraints. An entity can follow a kind, or be free-form and add fields as needed.

## 2. Type system

The distinctions below are confirmed (array vs list, map vs pair).

| Requirement | Type(s) |
|---|---|
| free form text | `text`: UTF-8, Unicode-only, **NFC-normalized on write** (via `golang.org/x/text/unicode/norm`) |
| numerics | `int` (64-bit), `float` (64-bit), `decimal` (exact), `bigint` |
| images | `image`: bytes + media type + width/height (standard library png/jpeg/gif, plus `golang.org/x/image` for webp, bmp, tiff) |
| binaries | `bytes`; large values go to a separate blob store, deduplicated by content hash |
| structured text | `doc<format>`: text plus a reference to a registered **format/codec** (json, xml, csv, or user-defined). This is the "key on how to interpret them" |
| maps | `map<K,V>` |
| key-value pairs | `pair<K,V>`: a single key and value |
| arrays | `array<T,N>`: fixed length, one element type |
| lists | `list<T>`: variable length, ordered |
| sets | `set<T>` |
| queues | `queue.fifo<T>`, `queue.lifo<T>`, `queue.ring<T,N>`, `queue.priority<T>` |
| "combination of which" | `record{...}` (nested struct) and `union<A\|B>` |
| **additions** | `bool`, `time`, `duration`, `null`, `ref` (a pointer to an entity or field) |

Queues come with their operations built into the language: push, pop, peek, size.

## 3. Query language: Rhizome

The language is named **Rhizome** (decided), with the file extension `.rhz`. One language handles both defining things and querying them. The syntax will be our own; it won't try to be compatible with SQL, Cypher or anything else. A rough sketch of the feel (not final syntax):

```
define kind Person "A human" {
  name   : text            "Full name"
  age    : int
  photo  : image
  tags   : set<text>
  inbox  : queue.priority<text>
}

create Person @alice { name: "Alice", age: 34, tags: {"eng", "ops"} }
relate @alice ~knows{since: 2019}~> @bob

find Person p where p.age > 30 and "eng" in p.tags
  | follow ~knows~> Person f
  | show p.name, f.name

push @alice.inbox "hello" priority 5
describe @alice          -- full inspection: fields, docs, relations, metadata
explain find ...         -- shows how a query will be executed
```

The language has these parts: definitions (kinds, formats, indexes), data changes (create/update/delete/relate), graph pattern matching and traversal, filters, projections, aggregates, collection and queue operations, transactions (`begin`/`commit`), and introspection commands.

**"See and reason with it"** means inspection and querying for v1: `describe`, `explain`, graph traversal, stats and graph export. **Inference rules** (relationships derived automatically from other relationships) are a planned advanced feature after v1.0. The language grammar should reserve room for a `rule` construct so it can be added without breaking changes.

## 4. Architecture

**Module path:** `github.com/sandgorgon/polydb` (decided).
**Dependencies:** Go standard library plus `golang.org/x/*`, plus two approved exceptions, both MIT and themselves standard-library only:
- `github.com/sandgorgon/9p`: the 9P access layer
- `github.com/sandgorgon/9auth` (v0.2.0+): TLS identities, key login and server pinning

Nothing else.
**Go version:** 1.26 or later (required by both libraries). Development uses Go 1.27.4.
**Platforms:** Linux and macOS only. Windows isn't supported.

```
polydb/
  value/      type system, encoding/decoding, comparison, text normalization
  model/      Entity, Field, Relation, Kind
  catalog/    kinds, formats, index definitions
  storage/    KV interface + mem (testing) + lsm (disk: WAL, memtable, SSTables, compaction)
  blob/       chunked, content-addressed blob store
  keys/       maps the graph model onto ordered keys (entities, fields, out/in adjacency)
  txn/        MVCC, snapshot isolation, transactions
  index/      secondary indexes, full-text (Unicode tokenizer)
  lang/       Rhizome lexer, parser, AST
  query/      planner, optimizer, executor
  engine/     the database core; one public Go interface used by both modes
  server/     network server (primary deployment mode)
  server/ninep  9P2000 file-tree view of databases, for browsing and Unix tools
  client/     Go client package for talking to the server
  cmd/polydb  server daemon + CLI client/REPL
  tui/        after v1.0
```

**Deployment modes** (both supported from the start, server first):
1. **Server (primary):** `polydb serve` runs the daemon; apps use `client.Dial(addr)` and the CLI connects over the network. Scaling work is aimed at this mode.
2. **Embedded (secondary, like SQLite):** `engine.Open(path)` runs the same engine in-process with no server.

Both modes implement the same Go interface, so application code can switch between them. The engine is designed for embedding from day one, but polishing and packaging embedded mode comes after the server.

**Wire protocol:** our own versioned binary protocol over TCP (with TLS) and Unix domain sockets. Messages are length-prefixed frames carrying the binary value encoding from `value/`, so results don't need converting to text and back. The protocol supports multiple requests at once on one connection, streaming result rows and cancellation. The Go `client/` package is the reference implementation; clients in other languages would be written against the protocol spec later. Full details: [spec/protocol.md](spec/protocol.md).

**9P access layer:** the server can also expose databases as a 9P2000 file tree, so they can be mounted (Linux kernel 9P) and explored with `ls`, `cat`, `grep` and other Unix tools. It's read-only by default and goes through the same permission checks. It serves the "see the contents of the database" requirement and isn't meant for applications. Details: spec/protocol.md §14.

**Security (roles from the start):**
- users log in with a password (SCRAM, never sent over the wire) or a 9auth key (Ed25519, SSH-style fingerprints); TLS for network connections
- roles that bundle permissions; users are given roles
- permissions such as read, write, define (schema), relate and admin, granted at server, database or kind level (decided; entity- and field-level permissions are out of scope for v1)
- users, roles and grants are managed through the query language (`create user`, `create role`, `grant`, `revoke`) and stored in a system catalog
- every query is checked against the caller's permissions; embedded mode runs as a built-in admin unless configured otherwise

**Platform-specific choices allowed by Linux/macOS only:**
- `mmap` for reading SSTables, via `golang.org/x/sys/unix`
- POSIX file locking and `fsync`/`fdatasync`; on macOS, use `F_FULLFSYNC` for real durability
- Unix domain sockets as a local server transport alongside TCP

## 5. Scale

**Target order:** one machine with large data first, then multi-node later.

**v1 (single machine, large data):**
- the storage engine handles datasets far larger than RAM (LSM + mmap + block cache)
- blob store for large images and binaries, with chunking and dedup
- background compaction and per-kind statistics
- transformations for efficiency:
  - re-indexing
  - re-clustering entities that are traversed together
  - converting a heavily used kind to a columnar layout
  - tiered blob storage
  - live schema migration
  - export/import between databases
- backup/restore while the server is running

**Provisions in v1 so multi-node isn't a rewrite later:**
- an ID and key layout that can be split into partitions without re-keying
- a pluggable storage interface
- a WAL format that can also serve as a replication stream
- the server/engine split, so a node can later host one or more partitions

**After v1.0 (multi-node):**
- **Partitioning:** split by entity ID ranges or by kind.
- **Replication:** first by shipping the log, then Raft-based consensus.
- **Distributed queries:** cross-partition traversal and query routing.

## 6. Other things this concept needs (req.md: "identify what else")

- transactions and concurrency control
- constraints and validation (required fields, uniqueness, type checks)
- field version history, **opt-in per kind** (or per entity/field for free-form data), with optional retention limits. When enabled, it allows viewing data as of an earlier time. MVCC keeps short-lived versions for transactions regardless; opt-in history decides what is kept after that.
- indexes and a query planner with `explain`
- blob garbage collection
- backup/restore and import/export (JSON plus a native format)
- authentication, roles and access control from the start (see section 4)
- network protocol
- observability (stats, slow-query log)
- resource limits (per-query memory and time, connection limits)
- a clear error model
- a testing approach: fuzzing the parser and encoders, plus crash-recovery tests for the storage engine

## 7. Phases

**To v1.0:**
0. **Specification:** type system, language and wire protocol specs written down and agreed. This is the most important step.
1. Value types (with normalization) + data model + in-memory storage + engine Go API
2. Language v1: parser and executor running on in-memory storage. Packages `lang` (front end and evaluator), `query` (executor) and `session` (session statements, introspection); scripts in `session/testdata/*.rhz` are run by `go test` and double as examples
3. **Server + client + binary wire protocol + CLI REPL connected to the server**, plus users, roles and permissions (still on in-memory storage)
4. Persistence: WAL + LSM, MVCC, crash recovery
5. Indexes, full-text search, planner, `explain`
6. Large-data work (planned 2026-11-24; each step a commit):
   1. History retention sweep: remove history past its retention for fields that are no longer written and for deleted entities (Phase 4 prunes only when a field is written)
   2. Statistics: sampled per-kind field and index-key stats (null share, distinct estimate, histogram), refreshed in the background after ~10% change or by `analyze kind K`; shown by `show stats kind`; the planner uses them for ranges and join fan-out, keeping counting probes for small ranges
   3. Row loading and bulk loads: profile and cut the ~6 µs per-row load cost; a bulk-load benchmark measuring write throughput and level-0 stalls, settling the compaction question with numbers
   4. Blob store: values over a threshold (~64 KiB) split into content-addressed chunks in per-database append-only blob segment files, with an index and reference counts in the store; chunks fsynced before the commit that references them; snapshot-, history- and lease-aware cleanup; rewriting mostly-dead segments; a streamed upload in the protocol (negotiated feature)
   4b. Lazy large values: a stored large value is read as a reference that statements pass along (show, store elsewhere, `len`, `=`) without loading; it is loaded only where content is needed, within a per-statement limit; uploads are validated while streaming and stored without being read back
   4c. Large-value limitations: index and unique keys of long text and bytes by start plus hash (no loading, small keys), streaming comparisons and text operators (sort, group, distinct, min/max, contains, matches, starts/ends with, substr), full-text indexing a piece at a time, checks counted against the load limit, a `max_load` session setting and server flag, the collector releasing dropped readers' segments, faster blob downloads
   5. Backup and restore while running: `backup database x to "dir"` writes a consistent snapshot of the store and the blob segments; `restore database y from "dir"` and, offline, `polydb restore`
   6. Import and export: JSON Lines and a native binary format, for whole databases or query results, keeping IDs by default (`with new ids` to copy); graph export (DOT, JSON); users, roles and grants (`export security`). Statements write a file in the backup directory or return the export as a value the client downloads, and import a file there or an upload; `polydb export` and `polydb import` wrap them
   7. Background `alter kind` for large kinds (batched, like index builds; writes to the kind pause, the new definition applies when checked) and `rebuild index`; changes that need no data work apply at once
   8. Slow-query log: statements over a threshold setting, with their plan
   9. Hardening: crash tests for blob writes, background migrations and backups; data larger than the cache; benchmarks
7. Getting v1.0 ready (planned 2026-11-25; each step a commit):
   1. Resource limits: a server default and ceiling for statement time; a per-statement working-memory budget (rows held for sort, group, distinct and results, and loaded large values); limits on connections in total and per user and on open transactions per session; a maximum upload size and a per-user quota on pending uploads; `show limits`
   2. Configuration and packaging: `polydb serve --config` (a plain `key = value` file mirroring the flags); a release build of Linux and macOS binaries (amd64, arm64) stamped with version and commit, with checksums; an example systemd unit
   3. Embedded mode polish: `polydb.Open(dir, opts)` and `polydb.OpenMemory()`, streaming a large value in from an `io.Reader`, runnable examples
   4. API stability review: the public API is `polydb`, `client` and `value`, everything else moves under `internal/`; documented compatibility promises for protocol 1.0, the Rhizome grammar, and the disk, backup and export format versions
   5. Documentation for people new to PolyDB. The specs (spec/) stay the precise reference; these docs teach. Rules: one idea per page, plain words before terms (and each term explained where it first appears, plus a glossary), a small example people can paste and run on every page, each page saying what to read next, and nothing in a guide that contradicts the specs. Kept in `docs/`, Markdown, readable on GitHub. Tested: a script runs every Rhizome example against a real server, so the docs can't drift from the code.
      - README.md: what PolyDB is in a paragraph (one database holding records, graphs, documents, text search and files together, queried with one language), when to use it and when not to, a 5-minute start (install, start a server, connect, make a kind, add data, query it), and links to the rest
      - docs/concepts.md: the ideas behind it, for someone who knows SQL or no database at all: databases, kinds (like tables) and fields, entities, relations (named links between entities, which is what makes it a graph), values and types, large values, indexes, transactions, users and permissions. A side-by-side "if you know SQL" table
      - docs/getting-started.md: a guided first session in the shell, building a small example (people, projects and who works on what) from nothing to useful queries
      - docs/rhizome-tour.md: the query language by example, from simple to advanced: find and where, show, sort and limit, following relations, grouping and counting, text search, changing data, transactions; each section links to the spec for the full rules
      - docs/cli.md: every `polydb` command and its flags (serve, the shell, init, backup, restore, export, import, user management, version), with the configuration file and every setting in one table
      - docs/embedding.md: using PolyDB inside a Go program (polydb.Open, OpenMemory, queries, transactions, large values), and docs/client.md: connecting to a server from Go
      - docs/operations.md: running a server for real: installing with the systemd unit, connections and TLS (9auth identity or a CA certificate), users and permissions, backups and restores, export and import, limits, the slow-query log, monitoring with show commands, upgrading
      - docs/faq.md and docs/glossary.md: common questions and mistakes (with the error messages people will see and what to do), and every term in one place
      - docs/README.md: a map of the docs, with "start here" paths for three readers: trying it out, building an application, running it in production
   6. Database metadata (added 2026-11-25 at the user's request): like entity metadata, a database has read-only `::name`, `::created` and `::owner`, and a `doc` note, `::labels` and `::props` that only admins of that database may change (`alter database x set …`). It is kept inside the database, so backups carry it, and exports carry the doc, labels and props. Shown by `describe database` and `show databases`, and read in queries as `database::…` (for example an application's `database::props.schema_version`).
   7. Phase 6 follow-ups: imports slowing as the database grows; the per-row cost of index range queries
   8. Release-candidate pass: full test, race, fuzz and crash runs on Linux and macOS; a CHANGELOG
8. **v1.0 release**

**After v1.0:**
- 9P access layer (`server/ninep`, protocol.md §14; moved after v1.0 on 2026-11-25, an addition rather than core; drafted steps and findings kept for then):
  1. 9P library support (`github.com/sandgorgon/9p`, needs the owner's approval): 9P2000.u `Tattach`/`Tauth` carry a numeric user ID and `Rerror` an error number, which the library doesn't read or write yet, so Linux kernel mounts fail at attach; tests that replay the kernel's message shapes; PolyDB moves to the new release
  2. Server and transports: the `FileSystem`/`File` backend, a Unix socket in the data directory (OS peer credentials, mapped like `PEER`), TCP with 9auth mutual TLS (client certificate required, its fingerprint registered to a user), plain TCP on loopback with `--insecure`; `polydb serve --9p`, `--9p-tcp`; each open file reads one snapshot, released on close or after 5 minutes; `Tflush` cancels; per-connection request and fid limits
  3. The read-only tree: databases, `kinds/<Kind>/<id>/` (fields, fieldinfo, meta, describe, out, in), `entities/`, `relations/`, `schema/`; values as raw bytes, text or Rhizome literals, with exact sizes; large values streamed from the blob store; relations as symlinks with .u and path files without; kind directories listed in ID order and streamed; permissions as on the native protocol, hidden kinds invisible
  4. Query files: `query/clone`, `query/<N>/ctl`, `data`, `error`; tsv (default), json and rhizome output; results streamed; query directories freed with their connection
  5. Writable fields (`--9p-writable`, off by default): a write to `fields/<name>` is buffered and applied as one `set` when the file is closed; editors that save by writing a temporary file and renaming it over the target (9ed does) work: temporary files in `fields/` live only in memory, and renaming one onto a field sets it; everything else stays a permission error
  6. Clients and interop: `polydb 9p ls|cat|get|query` for machines without kernel 9P (macOS); tests and documented recipes for 9sh (`bind dial(...)`), 9mux (`browse unix:…`), 9ed editing a field through a 9sh namespace, the library's `9pc`, and Linux kernel mounts (a script run as root where the 9p module is available)
  7. Hardening: fuzzing walks and names, permission and snapshot tests, `Tflush` and concurrency tests, listing a kind of a million entities, benchmarks (reading a large value, listing, query files)
  Findings (2026-11-25): the 9p library's 9P2000.u attach doesn't read the numeric user ID the Linux kernel sends, and its .u errors carry no error number, so kernel mounts fail until the library is fixed (a change to that repo, needing its owner's approval). 9sh's `dial` (9auth mutual TLS, or a Unix socket) fits the planned transports; 9mux browses only plain TCP or a Unix socket, so it reaches PolyDB by the socket; 9ed saves by creating a temporary file and renaming it over the target, so writable fields would need in-memory temporary files in `fields/` (a spec amendment); the 9P socket should default to the data directory, not `/var/run/polydb`
- multi-node: partitioning, replication, distributed queries
- re-clustering entities traversed together, columnar layout for heavily used kinds, tiered blob storage (deferred 2026-11-24)
- large collections stored element by element (types.md §13; deferred 2026-11-24, until a workload needs it)
- inference rules
- TUI (with `golang.org/x/term`)

## 8. Known constraints and risks

- **Writing our own storage engine is the biggest effort and risk.** It is doable, but correctness under crashes needs heavy testing, including crash tests on both Linux and macOS (their `fsync` behaviour differs).
- **Server-first means the wire protocol is a public contract early.** It needs versioning from the first release.
- **A custom binary protocol is harder to debug** than a text one. We'll need a protocol dump/trace tool in the CLI, and only Go has a client until others are written.
- **Roles from the start add a permission check to every operation.** It must be cheap, and it has to be designed into the query planner rather than added afterwards.
- Partitioning provisions add design work in v1 that won't pay off until multi-node arrives.

---

## 9. Open questions

None. All planning questions are settled; next step is Phase 0 (specifications).

## 10. Decisions log

| Date | Decision | Notes |
|---|---|---|
| 2026-11-21 | Relations attach to both entities and fields | Fields are graph nodes too |
| 2026-11-21 | "Reason with it" = inspecting/querying in v1 | Inference rules come after v1.0; reserve grammar space |
| 2026-11-21 | Type distinctions confirmed | array = fixed length, list = variable; map = collection, pair = single key/value |
| 2026-11-21 | Server and embedded both supported; server first | Embedded (SQLite-like) is secondary, same engine interface |
| 2026-11-21 | Scale: single machine with large data first, multi-node later | v1 includes provisions for partitioning |
| 2026-11-21 | Normalize text to NFC; `golang.org/x/*` allowed | Also enables x/image, x/sys, x/term |
| 2026-11-21 | TUI after v1.0 | |
| 2026-11-21 | Linux and macOS only | No Windows support |
| 2026-11-21 | Kinds are optional schemas | Entities can be free-form or follow a kind |
| 2026-11-21 | Custom binary wire protocol | TCP + TLS and Unix sockets; versioned; Go client is the reference |
| 2026-11-21 | Roles and permissions from the start | Managed through the query language; checked on every query |
| 2026-11-21 | Field history is opt-in | Per kind, or per entity/field for free-form data; optional retention |
| 2026-11-21 | Permissions at server, database and kind level | No entity- or field-level permissions in v1 |
| 2026-11-21 | Multiple databases per server | Each database is an independent graph |
| 2026-11-21 | Project tracked in git | Repository initialized |
| 2026-11-21 | Query language named **Rhizome**, extension `.rhz` | Web search: only conflict is RhizomeDB (abandoned 2023). Weave and Strand rejected for conflicts. Trademark search (USPTO/EUIPO) still to do before public release |
| 2026-11-21 | Go module path `github.com/sandgorgon/polydb` | |
| 2026-11-22 | Specs agreed: types, Rhizome language, wire protocol | See spec/ |
| 2026-11-22 | 9P access layer for browsing and Unix tools | Uses `github.com/sandgorgon/9p` (approved dependency exception); requires Go 1.26+; becomes Phase 7 |
| 2026-11-22 | 9auth for identities and key login | `github.com/sandgorgon/9auth` approved. Used for the server TLS identity, `KEY` login (replaces the post-v1 client-certificate plan), CLI server pinning and remote 9P over TLS. `LoadFrom` added upstream and released as v0.2.0 |
| 2026-11-22 | Go upgraded to 1.27.4 | Official release installed in `~/.local/go` |
| 2026-11-22 | One store per database | Server-level data (users, roles, database list) gets its own system store. Dropping a database removes its store |
| 2026-11-22 | One record per field | An entity is a header record plus one record per field, keyed by entity and field name, so a field write never rewrites the whole entity (key layout in `keys/`) |
| 2026-11-22 | Catalog IDs in keys and stored types | Kinds, relation types and derived formats are referenced by ID; relation types are registered on first use. Renames rewrite one definition |
| 2026-11-22 | Catalog lives in the database's store | Written in the same atomic batch as the data it goes with; transactions read an immutable cached snapshot |
| 2026-11-22 | Field IDs assigned when a field is first written | `::id` always works and any field can be related at any time |
| 2026-11-22 | Guarded reads for constraint checks | Snapshot isolation plus read guards on the keys and ranges a check depends on, so `unique`, `max out/in` and `restrict` hold under concurrency. Definition changes write a catalog-version key that every data writer guards |
| 2026-11-22 | Phase 4 storage choices | Durable commits by default with group commit (a `relaxed` setting for bulk loads); no block compression yet, with a per-block compression type in the format; leveled compaction; opt-in field history and `as of` built in Phase 4 |
| 2026-11-22 | One compaction at a time, for now (confirmed 2026-11-23) | Flushes run independently, but compactions are serialized, so a long deep compaction delays level 0→1 and sustained writes faster than the disk can compact (bulk loads) can stall at 12 level-0 tables. Revisit once the system is in real use, if stalls show up: run level 0→1 alongside deeper compactions, and slow writes gradually instead of stopping them |
| 2026-11-23 | Phase 5 index and search choices | Non-unique and text indexes are built in the background, resuming after restarts, and used once ready (unique indexes stay synchronous); `search()` on a field without a text index scans with the same rules; full-text tokenizes Unicode letters and digits, ignoring case and accents, with CJK characters as single words; queries take all-words, phrases, `-word`, `prefix*` and `OR`; no stemming or ranking yet (term positions and counts are stored so BM25 can come later); the planner estimates with counting probes, and stored statistics wait for Phase 6; absent and null values aren't indexed |
| 2026-11-23 | Query planner rules | Plans never change results or row order: an index answers a condition only when every entity the condition passes is sure to be in it; rows that an index or an earlier condition rules out skip the other conditions, so errors those would raise on them don't happen (spec/rhizome.md §7.5). `explain` shows each variable's path and estimate; `explain analyze` adds actual rows and times |
| 2026-11-24 | Phase 6 choices | Blobs live in separate append-only segment files, not in the LSM store (written once instead of rewritten by compaction, no 64 MiB record limit, no cache pollution), with content-hash chunks and reference counts in the store; clients upload large values by a streamed protocol upload; backup is a physical snapshot of files (export is the portable form); import and export run in the client over the protocol, and server side for admins within a configured directory; imports keep IDs by default; statistics are sampled in the background rather than updated on every write; a slow-query log comes in Phase 6, resource limits in Phase 8 |
| 2026-11-24 | Deferred until after v1.0 | Re-clustering entities traversed together, a columnar layout for heavily used kinds, and tiered blob storage: each needs real workloads to design against |
| 2026-11-24 | Compaction revisited with numbers (Phase 6 step 3) | A bulk load of 10 million entities on disk (4 durable writers) stalled 11 times for 49 s of 253 s with one compaction at a time. Now up to two compactions run at once on different levels (`MaxCompactions`), and writes stall at 24 level-0 tables instead of 12 (bloom filters keep reads cheap): no stalls, 47,300 entities/s, write amplification 5.2 |
| 2026-11-24 | Blob store details (Phase 6 step 4) | The spec's 16 KiB threshold (an engine setting, not per database) and 1 MiB chunks. Instead of reference counts, a use record per (blob, entity) plus a dirty mark on entities that may have dropped one: concurrent writers of the same content never contend on a shared counter, and the collector re-checks only marked entities. Leases read stored content from the blob store, pinning its segments, instead of holding it in memory. Uploads (`blob-write` feature) are referenced by lease token in a request's `blob_params`, which also lets a blob from a result be passed back without resending it. Out-of-line collections (types.md §13) remain open |
| 2026-11-24 | Lazy large values (Phase 6 step 4b) | A stored large value reads as `value.Lazy` (its reference plus a reader that pins the segments it reads, released explicitly or when garbage). `Eval` loads; `EvalLazy` doesn't, and is used where values are only passed along: `show`, field values in `create`/`update`/`add`, `if`/`coalesce` branches, `count`, `typeof` and type tests; `len` and `=`/`!=` use the reference. Loading counts against a per-statement limit (engine `MaxLoad`, default 256 MiB) and is cached per statement. Results stay lazy for the server and the embedded API (as `*polydb.Blob`), and are loaded otherwise. Unique constraints and indexes on large fields load them, since keys hold content. Uploads are NFC-normalized and validated while streaming, so a statement stores them without reading them back. Per 64 MiB value, client and server together: peak live heap for upload and store 132 → 9 MiB, fetch 137 → 9 MiB (and 4× faster: readers fetch 1 MiB at a time and keep the last chunk), updating another field 196 → 0.2 MiB |
| 2026-11-24 | Large-value limitations (Phase 6 step 4c) | Keys of text and bytes longer than 256 bytes hold the first 256 bytes, 0x00 0x02 and the SHA-256: small keys, no loading, exact equality and uniqueness, correct order against everything except other long values sharing the 256 bytes (hash order). So range and `starts with` probes with a longer bound scan, and top-N reads on through such a group. Comparisons, sort, group/distinct, min/max and the text operators read large values in 64 KiB pieces (`lang.Evaluator.Order`, with each value's first 256 bytes cached); functions that build new values still load. Full-text indexing tokenizes a piece at a time. `max_load` is a session setting (default from the server's `--max-load`), also a QUERY option; checks and defaults count against it. The collector runs a garbage collection when a removable segment is held only by readers, so dropped lazy values release it. Blob.Reader fetches 1 MiB pieces and implements WriterTo (io.Copy was bypassing its buffer and fetching 8–32 KiB at a time). Fetch of 64 MiB: 520 → 102 ms. Fixed on the way: statistics sampling panicked on blob references (not keyable now); a client's next request in a transaction could see `txn_busy` (the transaction is now freed before the request's last message). Collections stored element by element are deferred until after v1.0 |
| 2026-11-24 | Backup and restore (Phase 6 step 5) | A backup writes one snapshot of the store as compacted tables in the last level (`lsm.Build`), rather than copying the store's files: it needs no memtable flush or pinned versions, works for in-memory databases too, holds only the latest version of each key, and restores as fast as a copy (opening it is enough). Blob segments are copied whole, pinned, as long as they are when the backup starts; the snapshot's read transaction keeps the collector from deleting any it can read. `BACKUP` (JSON: format, database, time, snapshot sequence, every file's size and SHA-256) is written last, by rename, so a directory without it is unfinished; restore checks every file while copying it. Restore reserves a database number, copies without the engine lock, then registers the name; an interrupted restore leaves an unlisted directory that startup deletes. Statement paths are relative to the backup directory (`--backup-dir`, default `<data-dir>/backups`) so that remote admins can't write elsewhere; `backup database` needs admin on the database, `restore database` admin on the server and a new name. Users and grants aren't in a database backup. Measured on disk, 165 MiB (200,000 entities, 100 MiB of large values): backup 232 MiB/s, restore 512 MiB/s |
| 2026-11-24 | Export and import (Phase 6 step 6) | One record stream with two encodings: JSON Lines (values in an exact JSON form: plain JSON where it has the type, a one-key `$tag` object otherwise; types.md §12.3) and native (length-prefixed binary values). Definitions are Rhizome `define` statements, ordered so they run (a kind cycle is broken with `alter kind … add`); import accepts only definitions, runs them in a scratch database, compares each with the target's by its canonical statement, and creates the missing ones. Large values go once each as blob and 1 MiB data records, and import streams them into the blob store, so neither side loads them. Entities keep IDs, field IDs and metadata (`Tx.ImportEntity`; ref targets aren't checked, since they may come later); `with new ids` remaps entities, fields and relations and rewrites refs, not plain ids. Imports commit in batches of up to 4,000 records or 8 MiB, retrying conflicts; a failure keeps earlier batches. Query results export as rows, csv, dot or a JSON graph (the result's entities plus the relations among them). `export security` carries users with their PBKDF2 credentials (or `without passwords`), keys, roles, memberships and grants with kinds by name; import keeps existing users' passwords and keys unless `replace`, and skips grants on missing databases or kinds. Exports without a path are returned as a bytes value held in the database's blob store, so the client downloads them in pieces. Measured on disk (200,000 entities and 32 MiB of large values): export about 100,000 entities/s, import about 43,000/s, near the engine's bulk write speed |
| 2026-11-25 | Background alter kind (Phase 6 step 7) | Option B, chosen over writes continuing (A, kept for later if needed): altering a big kind is planned maintenance. A change is sorted by what it needs: nothing (applies at once; unique keys of a dropped `unique` go by range delete), a check of every entity (no rewrite, so versions don't move), or a rewrite. Up to 1,000 entities run in the statement; larger kinds run in the background: writes stop (every write guards the kind's alter key, which the alter sets, so earlier writers conflict), a read-only check in batches (keys of newly unique fields written there, removed on failure), a switch to the new definition, then a rewrite in batches while reads convert unrewritten entities (dropped fields hidden, values conformed, own fields shown as declared) and the planner ignores the kind's indexes. The job's state lives in a meta key per kind, loaded at open before anything reads or writes, so it resumes after a restart. `show alters` reports it; `rebuild index` deletes an index's entries and builds it again. Found on the way: a transaction re-sorted all its buffered keys for every scan, so transactions that read what they wrote were quadratic; keys now stay sorted with a small unsorted tail, and a 200,000-entity alter on disk went from 7,600 to 40,000 entities/s |
| 2026-11-25 | Slow-query log (Phase 6 step 8) | Statements at or over a server threshold (`--slow-query`, default 1 s, 0 off) are logged with their time, user, database, text, rows and error, and for queries the source's plan with rows bound. Kept in memory (the latest 256, `show slow queries`, server admins only, since texts hold literals) and handed to a hook the server writes to its log; no persistent table, the server log keeps history. The plan is described only when the statement's run passes the threshold (`query.Context.PlanIfSlower`), so fast statements pay only two clock reads. Passwords in `create user`/`alter user` are hidden; parameters appear by name only; waiting pops are left out |
| 2026-11-25 | Hardening (Phase 6 step 9) | Crash tests on the power-cut file system (vfs.Mem.Crash, torn writes) for large values written, shared, overwritten and deleted by concurrent writers while the collector frees blobs, moves chunks and deletes segments; for background alters cut at any phase; for backups and restores (whole or refused, the source untouched, no leftover directories); for exports to a file (whole or absent). Fault injection (vfs.Faulty) over writes with large values, collection and a backup. Every acknowledged change survives, an in-flight one may or may not, nothing is half done, and the store collects and verifies its indexes after. A correctness test reads through a 64 KiB block cache over 10 MB of tables. `POLYDB_BENCH=<entities>` reports on disk; 1 million entities (674 MiB, 64 MiB cache): load 50,900 entities/s (4 writers, durable), point reads 50,200/s, index equality 12,600 queries/s, index range of 100 rows 448/s, unindexed filtered scan 220,000 entities/s, large values 186 MB/s written and 1.6 GB/s read, backup 137 MB/s, restore 378 MB/s, export 60,900 and import 19,700 entities/s (import slows with size: a follow-up), background alter 23,700 entities/s |
| 2026-11-25 | Remaining phases to v1.0 | The 9P access layer moves after v1.0 (an addition, not core), with its drafted steps and findings kept under "After v1.0". Phase 7 is now getting v1.0 ready: resource limits (per-query memory and time, connection limits, upload quotas), embedded mode polish and packaging, API stability review; Phase 8 is the release |
| 2026-11-25 | Resource limits (Phase 7 step 1) | engine.Options.Limits holds every limit (statement time, statement memory 1 GiB, connections 1,000 total / 100 per user, 16 transactions per session, 4 GiB uploads, 16 GiB per-user upload quota; negative = none). `max_memory` replaces `max_load`: one budget for loaded large values and the rows a statement holds, estimated per step and charged as sources bind rows. Sessions and request options may lower the time and memory limits; only server admins raise or lift them, and admins may connect past the total. Estimates, not exact accounting. |
| 2026-11-25 | Configuration and packaging (Phase 7 step 2) | `serve --config` reads `key = value` lines named like the flags; flags override the file; sizes take units (KiB…TiB, KB…TB). A `version` package: the release build stamps it with -ldflags, `go install …@vX` reports the module version, a checkout reports 1.0.0-dev; `polydb version` adds commit, time and Go version. scripts/release.sh builds static, trimmed linux/darwin × amd64/arm64 archives with the example config and a hardened systemd unit (packaging/), plus SHA256SUMS. |
| 2026-11-25 | Embedded mode (Phase 7 step 3) | `polydb.Open(dir, opts)` and `OpenMemory(opts)` return `*Embedded`: a DB that owns its engine (Close closes both), runs as admin, and uses `Options.Database` (default `main`, created if missing). Options expose only what an application needs (durability, cache, statement time and memory, backup directory), no engine types, so the engine can move under internal/. `Upload` streams a large value from an io.Reader into the blob store (an hour to use it) and returns a Blob parameter; the `Uploader` interface covers it and `*client.Conn`. Runnable examples in example_test.go. |
| 2026-11-25 | API stability (Phase 7 step 4) | Public packages: `polydb`, `client`, `value` (plus the `polydb` command); the rest moved under `internal/`. Public APIs no longer show internal types: `Embed` is unexported (tests in the module reach it through internal/hook), `WithTrace` gives text, a `Blob` reads through a public `BlobSource` (the client's own source carries the lease token, replacing NewLeasedBlob/Token/SplitParams), MetaOf/PropsOf moved into the client. `client.Dial` returns `*Conn` (also fixes a non-nil DB on error); `polydb.Version()` replaces `client.Version`. Promises in docs/compatibility.md: data, backups and exports readable by any later 1.x; Rhizome adds only contextual keywords; error codes stable; protocol minors additive; interfaces may gain methods (BlobSource excepted). |
| 2026-11-25 | Documentation (Phase 7 step 5) | README.md and docs/: getting started, concepts (with an SQL comparison), a Rhizome tour (including metadata), the CLI and every setting, embedding and client from Go, operations, compatibility, FAQ with every error code, glossary, and a map with reading paths. Every ```rhizome block runs in TestDocs (fresh database per page; `error` blocks must fail, `norun` skipped) and TestDocLinks checks links and anchors. Found on the way: unique-constraint errors now name the field; protocol spec's connection-limit code is `limit`; `role` and `where` are reserved, so the FAQ explains backticks. |
| 2026-11-25 | Database metadata (Phase 7 step 6) | Asked for by the user. Stored in the database's own store (meta key `database`), so backups carry it all, owner and creation time included. `create database` records `::created` and `::owner`; `alter database d set doc = …, ::labels ±= …, ::props ±= …, ::props.key = …` needs admin on d, runs outside transactions; `::name`, `::created`, `::owner` read-only. Read as `database::x` in any expression (current metadata, even in `as of`), `describe database d`, and new `show databases` columns (owner, created, labels, doc). Exports carry doc, labels and props in the header; import merges (doc replaced if given, labels added, export's props win), keeping the target's owner and creation time. |
