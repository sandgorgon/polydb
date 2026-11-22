# PolyDB Plan

Status: **Phase 0 (specifications) complete (2026-11-22).** Next: Phase 1. No code written yet.
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
**Dependencies:** Go standard library plus `golang.org/x/*`, plus one approved exception: `github.com/sandgorgon/9p` (MIT, itself standard-library only) for the 9P access layer. Nothing else.
**Go version:** 1.26 or later (required by the 9P library).
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
- users with password authentication (salted, slow hash); TLS for network connections
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
2. Language v1: parser and executor running on in-memory storage
3. **Server + client + binary wire protocol + CLI REPL connected to the server**, plus users, roles and permissions (still on in-memory storage)
4. Persistence: WAL + LSM, MVCC, crash recovery
5. Indexes, full-text search, planner, `explain`
6. Large-data work: blob store, compaction tuning, stats, transformations, backup/restore, import/export, graph export (DOT/JSON)
7. 9P access layer (`server/ninep`): file tree, query files, Linux mount testing
8. Embedded mode polish and packaging; API stability review
9. **v1.0 release**

**After v1.0:**
- multi-node: partitioning, replication, distributed queries
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
