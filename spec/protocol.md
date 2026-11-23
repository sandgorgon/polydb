# PolyDB Wire Protocol Specification

Status: **Agreed** (2026-11-22)
Related: [plan.md](../plan.md) section 4, [types.md](types.md), [rhizome.md](rhizome.md)

This document defines how clients talk to a PolyDB server: transports, security, the connection handshake, framing, messages, streaming, flow control and errors. The Go `client/` package is the reference implementation. Clients in other languages are written against this document.

### Design decisions (agreed 2026-11-22)

| Topic | Decision |
|---|---|
| Authentication | SCRAM-SHA-256 (password never sent); 9auth key login (`KEY`) over TLS; OS peer credentials on Unix sockets |
| Identities | Server and client TLS identities come from `github.com/sandgorgon/9auth` (Ed25519, trusted by fingerprint, SSH-style); CA-signed server certificates also supported |
| TLS | Required on TCP; `--insecure` development mode limited to localhost |
| Multiplexing | Many requests at once on one connection, each on its own stream, with flow control |
| Sending queries | Rhizome text, plus prepared statements with binary parameters |
| Large values | Inline up to 16 KiB; larger values sent as blob references and fetched separately |
| Compression | Optional deflate, negotiated, off by default |
| Versioning | Major/minor versions plus feature flags, negotiated in the handshake |
| Queue waiting | `pop … wait <duration>` (blocking pop) in v1; change subscriptions after v1.0 |
| 9P layer | Databases can also be exposed as a 9P2000 file tree for browsing and Unix tools, built on `github.com/sandgorgon/9p` (§14) |

---

## 1. Transports

| Transport | Security | Default address |
|---|---|---|
| TCP | TLS 1.3 required (TLS 1.2 allowed by server setting) | port **7377** (unassigned in the IANA registry as of 2026-11-22: part of the unassigned block 7366–7390 for TCP and UDP. Registering it formally with IANA is optional and can be done before public release) |
| Unix domain socket | protected by file permissions | `/var/run/polydb/polydb.sock` |

- **TLS:** the ALPN protocol name is `polydb/1`. The server's certificate comes from one of two modes:

  | Mode | Server certificate | How clients trust it |
  |---|---|---|
  | **Identity (default)** | the server's 9auth identity, loaded with `auth.LoadFrom(<data dir>/identity)`. It's generated on first start, then never changes, whichever account runs the server. `polydb serve` prints its fingerprint at startup | by fingerprint: pinned explicitly, or trust-on-first-use recorded in 9auth's `known-peers` file (§13) |
  | **CA** | a certificate and key from the server config (for example from Let's Encrypt or a company CA) | standard certificate-chain and hostname verification |

- **Client certificates:** the server *asks for* a client certificate on every TLS connection but doesn't require one. Password users connect without one. A client that presents its 9auth identity can log in with `KEY` (§3.1). The server builds its own `tls.Config` from 9auth's exported pieces (`TLSCertificate`, `FingerprintOf`), because 9auth's `ServerTLSConfig` requires a client certificate and would lock out password users.
- **`--insecure`:** turns off TLS for TCP, but the server then only accepts connections from loopback addresses (127.0.0.1, ::1) and logs a warning at startup and on every connection. It is meant for development only.
- **Unix sockets** don't use TLS. The socket file's permissions control who can connect.

---

## 2. Connection lifecycle

```
Client                                         Server
  |-- TCP connect + TLS handshake (TCP only) --->|
  |-- preface: "PLYDB\r\n" + 0x00 0x01 --------->|
  |-- HELLO (versions, features, client info) -->|
  |<------------ HELLO_OK (chosen version, ...) -|
  |-- AUTH (mechanism, first message) ---------->|
  |<----------------------- AUTH_CONTINUE -------|   (SCRAM only)
  |-- AUTH (final message) --------------------->|
  |<------------- AUTH_OK (session info) --------|
  |                                              |
  |== requests and responses on streams ========>|
  |                                              |
  |<------------------ GOAWAY (on shutdown) -----|
```

1. **Preface:** the client sends the 7 bytes `PLYDB\r\n` followed by two bytes, the highest major version it supports (big-endian). A server that receives anything else closes the connection. This rejects accidental HTTP or other traffic immediately.
2. **HELLO / HELLO_OK:** negotiate the protocol version and features (§9) and exchange limits.
3. **Authentication** (§3).
4. **AUTH_OK** marks the connection ready. It carries the session ID, the authenticated user and the current database (if the client asked for one in HELLO).
5. Normal operation: requests on streams (§5).
6. **Shutdown:**
   - The server sends GOAWAY with the last stream ID it will process, finishes those streams, then closes.
   - The client may also send GOAWAY.
   - Either side may close abruptly; open streams then fail with `connection_lost`.

**Timeouts** (server settings, defaults shown):

| Timeout | Default |
|---|---|
| Handshake (connect to AUTH_OK) | 10 s |
| Idle connection (no open streams and no traffic) | 30 min |
| Keepalive: either side sends PING after this much silence | 60 s |
| A PING not answered with PONG within this time closes the connection | 30 s |

---

## 3. Authentication

### 3.1 Mechanisms

| Mechanism | When |
|---|---|
| `SCRAM-SHA-256-PLUS` | over TLS (preferred). Includes channel binding with `tls-exporter` (RFC 9266), so the login is tied to this TLS connection and can't be relayed through a man-in-the-middle |
| `SCRAM-SHA-256` | over Unix sockets, or `--insecure` mode |
| `PEER` | Unix sockets only. The server reads the connecting process's OS user (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS, via `golang.org/x/sys/unix`) and maps it to a PolyDB user using the server config. No password needed |
| `KEY` | over TLS only. The client presented its 9auth identity certificate during the TLS handshake, which proves it holds the private key. The first AUTH message names the PolyDB user. Login succeeds if that certificate's fingerprint is registered to that user (§3.4). No password, no extra round trip |

HELLO_OK lists the mechanisms the server allows on this connection. The client picks one in its first AUTH message.
- **Over TLS** the server offers `KEY` (only when the client presented a certificate) and `SCRAM-SHA-256-PLUS`. Plain `SCRAM-SHA-256` isn't offered there, so an attacker can't strip the channel binding. A client that supports channel binding but believes the server doesn't sends the `y` GS2 flag; a server that offered `-PLUS` refuses it as a downgrade.
- **On Unix sockets** the server offers `PEER` (only when the peer's OS user maps to a PolyDB user) and `SCRAM-SHA-256`.
- For a user that doesn't exist or has no password, the server answers with stable fake SCRAM parameters, so the exchange looks the same until the final check fails with the generic `auth_failed`.
- Clients refuse SCRAM iteration counts below 4,096. The Go client and CLI try `KEY` first when they have a 9auth identity and the server offers it, and otherwise fall back to SCRAM.

### 3.2 SCRAM
- Follows RFC 5802 and RFC 7677 exactly. The client-first, server-first, client-final and server-final messages travel in AUTH, AUTH_CONTINUE, AUTH and AUTH_OK respectively.
- **Stored credentials:** the server stores only the salt, iteration count, StoredKey and ServerKey. It never stores the password or a plain password hash.
- **Iterations:** PBKDF2-SHA-256 with 100,000 iterations by default, configurable per server, minimum 4,096.
- Passwords are normalized with the PRECIS OpaqueString profile (RFC 8265, the successor to SASLprep), from `golang.org/x/text/secure/precis`. User names are PolyDB identifiers, NFC-normalized (types.md §14).
- **The client verifies the server's final signature** before treating the login as successful. A server that can't produce it is treated as an impostor.

### 3.3 Failed logins
- The error is always the generic `auth_failed`. It never reveals whether the user exists.
- Repeated failures from the same source address are slowed down (exponential backoff, capped at 30 s). An admin can see failures with `show sessions` and the server log.
- **First admin:** `polydb init` creates the initial `admin` user. It prompts for a password, or generates one and prints it once. It can also register the running user's 9auth key for `admin` (`polydb init --key`). Until it has been run, the server accepts only `PEER` connections from the OS user that owns the data directory.

### 3.4 Keys (9auth identities)
- **Users can have any number of registered keys,** each identified by its 9auth fingerprint (64 hex characters: SHA-256 of the Ed25519 public key) with an optional label:
  ```
  alter user ramon add key "3f9a…c41e" as "laptop";
  alter user ramon drop key "3f9a…c41e";
  show keys for ramon;
  ```
- A fingerprint can be registered to only one user per server.
- Adding or dropping keys for another user needs `admin`. Users may manage their own keys.
- `polydb whoami --fingerprint` prints the local 9auth fingerprint (the same one `9sh` and `9vcs` use on that machine), to send to an admin.
- Keys are stored in the system catalog, not in 9auth's `authorized-peers` file. PolyDB's roles and grants stay the only permission system; 9auth's own permission levels aren't used.

---

## 4. Framing

All data after the preface is sent in **frames**.

```
 0               1               2               3
+---------------+---------------+---------------+---------------+
|                    payload length (uint32)                    |
+---------------+---------------+---------------+---------------+
|  frame type   |     flags     |        reserved (0)           |
+---------------+---------------+---------------+---------------+
|                       stream ID (uint32)                      |
+---------------+---------------+---------------+---------------+
|                    payload (length bytes) ...                 |
```

- **12-byte header**, all integers big-endian.
- **Payload length** may not exceed the negotiated maximum frame size: 1 MiB by default, at most 16 MiB.
- **Flags:**

  | Bit | Name | Meaning |
  |---|---|---|
  | 0x01 | `END_STREAM` | last frame the sender will send on this stream |
  | 0x02 | `CONTINUED` | the message continues in the next frame on this stream |
  | 0x04 | `COMPRESSED` | the message payload is deflate-compressed (§10) |
  | 0x08 | `MUST_UNDERSTAND` | if the receiver doesn't know this frame type, it must fail instead of ignoring the frame |

- **Messages and frames:** a message larger than the frame size is split across frames: every frame but the last has `CONTINUED`. Frames from different streams may be interleaved between them. A message may be at most 64 MiB once reassembled; blob content is streamed instead (§7).
- **Stream IDs:**
  - Stream **0** is the connection itself, for control messages.
  - **Odd** IDs are opened by the client.
  - **Even** IDs are reserved for streams opened by the server (change subscriptions after v1.0).
  - Each side's IDs must increase and are never reused on a connection.
- **Unknown frame types** are ignored, unless they carry `MUST_UNDERSTAND`, in which case the connection fails with `protocol_error`. This lets newer peers send optional extras safely.

### 4.1 Message bodies

A message body is a set of **numbered fields**, encoded as:

```
field count (varint)
repeat: field number (varint), value (PolyDB value encoding, types.md §12.1)
```

- Every message type defines its field numbers in §6.
- **Receivers ignore unknown field numbers.** That's how minor versions add fields without breaking older peers.
- Field numbers 0–99 are for this spec. 100 and above are reserved for vendor or experimental extensions.

---

## 5. Streams

- A client starts a request by sending a request message on a **new odd stream ID**.
- The server replies with one or more response messages on the same stream and sets `END_STREAM` on the last one.
- When both sides have sent `END_STREAM`, the stream is closed.
- **Concurrency:** at most 100 streams open at once per connection by default (server setting, announced in HELLO_OK). Opening more fails that stream with `limit`.
- **Session state** belongs to the connection and is shared by all its streams: the current database (`use`), session settings (`set`), session handles (`@alice`) and prepared statements. Changes on one stream are visible to requests started later on other streams.
- **Transactions** are explicit objects (§6.3). Several independent transactions can run on one connection at once.

---

## 6. Messages

### 6.1 Message types

| Code | Name | Direction | Stream |
|---|---|---|---|
| 0x01 | HELLO | C → S | 0 |
| 0x02 | HELLO_OK | S → C | 0 |
| 0x03 | AUTH | C → S | 0 |
| 0x04 | AUTH_CONTINUE | S → C | 0 |
| 0x05 | AUTH_OK | S → C | 0 |
| 0x06 | PING | both | 0 |
| 0x07 | PONG | both | 0 |
| 0x08 | GOAWAY | both | 0 |
| 0x09 | WINDOW_UPDATE | both | any |
| 0x0A | CONN_ERROR | both | 0 |
| 0x20 | QUERY | C → S | new |
| 0x21 | PREPARE | C → S | new |
| 0x22 | EXECUTE | C → S | new |
| 0x23 | CLOSE_STATEMENT | C → S | new |
| 0x24 | BLOB_READ | C → S | new |
| 0x25 | CANCEL | C → S | the stream to cancel |
| 0x40 | RESULT_HEADER | S → C | request's |
| 0x41 | ROWS | S → C | request's |
| 0x42 | RESULT_END | S → C | request's |
| 0x43 | ERROR | S → C | request's |
| 0x44 | PREPARED | S → C | request's |
| 0x45 | BLOB_DATA | S → C | request's |
| 0x46 | NOTICE | S → C | request's |
| 0x60–0x6F | reserved: change subscriptions | | |
| 0x70–0x7F | reserved: replication between servers (multi-node) | | |

### 6.2 Handshake and control messages

**HELLO** (client)

| # | Field | Type | Notes |
|---|---|---|---|
| 1 | versions | `map<int, int>` | major → highest supported minor, for example `{1: 0}` |
| 2 | features | `set<text>` | features the client supports (§9) |
| 3 | client_name | text | for example `polydb-go` |
| 4 | client_version | text | |
| 5 | database | text? | database to `use` after login |
| 6 | user | text? | user name (SCRAM) |
| 7 | inline_value_limit | int? | the largest value the client wants inline; the connection uses the smaller of this and the server's setting |

**HELLO_OK** (server)

| # | Field | Type | Notes |
|---|---|---|---|
| 1 | version | `pair<int, int>` | chosen major and minor |
| 2 | features | `set<text>` | features enabled on this connection (those both sides support and the server allows) |
| 3 | server_version | text | |
| 4 | auth_mechanisms | `list<text>` | allowed on this connection, in order of preference |
| 5 | max_frame_size | int | |
| 6 | max_streams | int | |
| 7 | initial_stream_window | int | flow control (§8) |
| 8 | initial_conn_window | int | |
| 9 | inline_value_limit | int | values up to this size are sent inline: the smaller of the server setting (default 16,384; from 1,024 up to the 64 MiB message limit) and the client's request in HELLO |

**AUTH / AUTH_CONTINUE / AUTH_OK:** carry the mechanism name (first AUTH only) and opaque `bytes` for the mechanism's messages. AUTH fields: 1 `mechanism` (text?), 2 `data` (bytes), 3 `user` (text?, names the user for `KEY`). AUTH_CONTINUE: 1 `data`. AUTH_OK: 1 `data` (SCRAM server-final), 2 `session_id` (id), 3 `user` (text), 4 `database` (text?).

**PING / PONG:** 8 bytes of opaque data, echoed back.

**GOAWAY:** `last_stream_id` (int), `code` (text), `message` (text).

**CONN_ERROR:** a fatal connection error (§11.1). It is followed by closing the connection.

### 6.3 Requests

**QUERY**: run Rhizome text

| # | Field | Type | Notes |
|---|---|---|---|
| 1 | text | text | one or more Rhizome statements |
| 2 | params | `map<text, any>` | values for `$name` parameters |
| 3 | txn | id? | run inside this transaction (see below) |
| 4 | options | `map<text, any>` | per-request settings: `timeout`, `max_rows`, `explain` |

**PREPARE**: `text` (one statement). The reply is PREPARED: `statement_id` (int), `param_types` (`map<text, text>`, type expressions) and `columns` (as in RESULT_HEADER, if known in advance).

**EXECUTE**: `statement_id`, `params`, `txn` and `options`, as in QUERY.

**CLOSE_STATEMENT**: `statement_id`. Statements are also freed when the connection closes.

**Transactions over the protocol:**
- Sending `begin;` in a QUERY returns a **transaction ID** in RESULT_END. Later QUERY and EXECUTE messages with that `txn` run inside the transaction.
- `commit;` or `rollback;` with the same `txn` ends it.
- A script can also contain a whole transaction in one QUERY (`begin; …; commit;`).
- A request without `txn` runs on its own (auto-commit).
- A transaction runs one request at a time. A second request for the same `txn` while one is running fails with `txn_busy`.
- **If a statement in a transaction fails or is cancelled, the whole transaction is marked failed** (decided). Further requests in it fail with `txn_aborted` until the client sends `rollback`. The CLI makes this obvious: its prompt shows when the current transaction has failed.
- Transactions left open when a connection closes are rolled back.
- Idle transactions time out after 5 minutes by default (server setting), because they hold back old versions.

### 6.4 Responses

A request produces, per statement in order: **RESULT_HEADER**, zero or more **ROWS**, then **RESULT_END**. The last frame of the last message carries `END_STREAM`. An ERROR replaces the rest and ends the stream.

**RESULT_HEADER**: `statement_index` (int; position in the script), `columns`: `list<record{ name: text, type: text }>`. The type is a type expression from types.md, or `any`.

**ROWS**: `rows`: `list<list<any>>`, a batch of rows with values in column order. The server chooses batch sizes, aiming for about 64 KiB per batch.

**RESULT_END**

| # | Field | Type |
|---|---|---|
| 1 | statement_index | int |
| 2 | rows_returned | int |
| 3 | rows_affected | int |
| 4 | created_ids | `list<id>` |
| 5 | txn | id? (set after `begin`) |
| 6 | txn_state | text: `none`, `open`, `failed`, `committed`, `rolled_back` |
| 7 | elapsed | duration |
| 8 | warnings | `list<text>` |
| 9 | plan | any? (for `explain`) |

**NOTICE**: warnings or progress during a long statement (for example, index build progress): 1 `message` (text), 2 `code` (text?). Clients may show or ignore it.

**ERROR**

| # | Field | Type |
|---|---|---|
| 1 | code | text (§11.2) |
| 2 | message | text |
| 3 | position | `pair<int, int>?`: line and column in the statement text |
| 4 | statement_index | int? (null when the error isn't tied to a statement) |
| 5 | retryable | bool |
| 6 | details | `map<text, any>` (for example the suggested field for `unknown_field`) |

### 6.5 Values in results

Column values use the value encoding from types.md §12.1, with these additions that only appear in results:

| Tag | Value | Contents |
|---|---|---|
| 0x22 | blob reference | value type (`text`, `bytes` or `image`), content hash, total length, media type, **lease token** (§7) |
| 0x80 | entity | `id`, `kind`, metadata record, `fields: list<field>` |
| 0x81 | field | `id`, `name`, `type`, `doc`, `declared`, `value` |
| 0x82 | relation | `id`, `type`, `from` (id), `to` (id), `props: map<text, any>` |
| 0x83 | path | `nodes: list<entity or field>`, `rels: list<relation>` |

Byte layouts after the tag ("str" is a varint length then bytes; ids are 16 raw bytes; "value" is a value encoding, which may itself be one of these result values):

| Tag | Layout |
|---|---|
| 0x22 | str type, str hash, varint length, str media type, str token |
| 0x80 | id, value kind (text or null), value metadata `record{created, updated, version, labels, owner, props}`, varint count, that many 0x81 fields |
| 0x81 | id, str name, str type expression, str doc, byte declared (0/1), byte present (0/1), then the value if present (absent fields have none) |
| 0x82 | id, str type, id from, id to, value props (map) |
| 0x83 | varint count, that many nodes (0x80 entities, or ids for field nodes), varint count, that many 0x82 relations |

A list (0x50) in a result may hold these values, for example `ROWS`. Blob references only stand in for whole text, bytes or image values: a cell, or an entity field's value.

Values larger than `inline_value_limit` (bytes, text, image data) are sent as **blob references** instead of inline. A large collection is always sent inline, because a whole field value was asked for. Its size is bounded by the 64 MiB message limit and by flow control.

---

## 7. Blob transfer

- **BLOB_READ** fields: `ref` (the blob reference from a result), `offset` (int, default 0), `length` (int, default to the end).
- The server replies with **BLOB_DATA** messages: `offset` and `data` (bytes), in order, with `END_STREAM` on the last.
- Flow control applies (§8), so a client can read a 4 GiB video at its own pace. Ranges allow partial reads (headers, video seeking).
- **Lease token:** a blob reference is valid while its transaction is open, or for **10 minutes** after its RESULT_END for auto-commit requests (server setting). The token proves the client was allowed to read the value, so BLOB_READ doesn't re-check permissions. After expiry, BLOB_READ fails with `lease_expired`.
- The Go client exposes blob references as an `io.ReaderAt` plus `io.Reader` that fetches lazily, so apps rarely see the difference.

---

## 8. Flow control

Flow control stops a fast sender from flooding a slow receiver. It works like HTTP/2:
- Each stream, and the connection as a whole, has a **send window**: the number of payload bytes the sender may still send. The initial sizes are 1 MiB per stream and 16 MiB per connection, announced in HELLO_OK.
- **Only ROWS and BLOB_DATA payloads count** against windows. Control and small messages never block.
- The receiver sends **WINDOW_UPDATE** (`increment`: int) on a stream ID (or stream 0 for the connection) after consuming data, to let the sender continue.
- A sender with no window left pauses that stream. Other streams continue.
- On the server, a paused stream also pauses the query producing it, so a slow client doesn't make the server buffer an unlimited result.
- Sending beyond the window is a `flow_control` connection error.

---

## 9. Versioning and features

- **Version = major.minor.**
  - Minor versions only add things (new fields, message types, features) that older peers can ignore.
  - A new major version may change anything.
  - This document defines **1.0**.
- **Negotiation:** the client lists its majors with their highest minors in HELLO. The server picks the highest major both support and the lower of the two minors, and returns it in HELLO_OK. If there is no common major, the server sends CONN_ERROR `version_mismatch` listing what it supports.
- **Features** are optional capabilities, named by text and enabled only when both sides support them. In 1.0:

  | Feature | Meaning |
  |---|---|
  | `deflate` | message compression (§10) |
  | `blob-range` | BLOB_READ with offset and length (always on in 1.0; listed so later majors can drop it) |
  | `queue-wait` | blocking `pop … wait` |

- Reserved names for later: `subscriptions`, `client-cert`, `zstd`.

---

## 10. Compression

- Available only when the `deflate` feature is negotiated: the client lists it in HELLO `features` only when it wants compression, and the server enables it if it allows it. It is off by default, and the Go client doesn't request it on Unix sockets.
- The sender compresses a whole message (before splitting into frames) with raw deflate (`compress/flate`) and sets `COMPRESSED` on its frames.
- **The sender decides per message.** Messages under 1 KiB and BLOB_DATA for already-compressed formats (images, typical binaries) should be sent uncompressed.
- Flow control windows count **compressed** bytes, since that's what crosses the wire.
- **Protection against decompression bombs:** a message that decompresses to more than the 64 MiB message limit fails with `protocol_error`.

---

## 11. Errors

### 11.1 Connection errors (CONN_ERROR, then close)

| Code | Meaning |
|---|---|
| `protocol_error` | malformed frame or message, or a sequence the spec forbids |
| `version_mismatch` | no common major version |
| `tls_required` | non-TLS TCP connection from a non-loopback address |
| `auth_failed` | login failed (always generic) |
| `flow_control` | window exceeded |
| `too_many_connections` | server connection limit reached |
| `shutting_down` | server is stopping (normally sent as GOAWAY instead) |
| `timeout` | handshake or keepalive timeout |

### 11.2 Stream errors (ERROR on the stream; the connection continues)

All Rhizome error codes (rhizome.md §14): `syntax`, `type`, `constraint`, `unknown_field`, `not_found`, `conflict`, `permission`, `limit`, `internal`. Plus:

| Code | Meaning | Retryable |
|---|---|---|
| `cancelled` | the client cancelled the request | no |
| `stale_statement` | a prepared statement became invalid after a schema change; prepare again | yes (the Go client re-prepares automatically) |
| `txn_busy` | another request is already running in this transaction | yes |
| `txn_aborted` | the transaction failed earlier; roll it back | no |
| `txn_not_found` | unknown or expired transaction ID | no |
| `lease_expired` | blob reference no longer valid | no |
| `unsupported` | request uses something not negotiated | no |

`conflict` is always marked retryable.

---

## 12. Cancellation and blocking pop

- **CANCEL** on a stream stops that request. The server stops work as soon as it can and ends the stream with ERROR `cancelled`. Rows already sent stay valid. A CANCEL for a stream that has already finished is ignored.
- **Blocking pop** (`pop @worker.jobs wait 30s;`, rhizome.md §6.5):
  - The stream stays open until an item arrives, the wait expires (returning `null`) or the client cancels.
  - Not allowed inside a transaction. That fails with `unsupported`.
  - The maximum wait is 15 minutes (server setting). Longer waits are capped and produce a warning.
  - **Fairness:** waiters on the same queue are served in the order they started waiting.
  - Needs the `queue-wait` feature.

---

## 13. Go client (reference implementation)

A sketch of the public API. The same `polydb.DB` interface is implemented by the embedded engine (`engine.Open`), so apps can switch modes.

```go
db, err := client.Dial(ctx, "polydb://ramon@db.example.com:7377/sales",
    client.WithIdentity(id),              // 9auth identity: KEY login
    client.WithKnownPeers(knownPeersPath)) // trust-on-first-use server pinning

rows, err := db.Query(ctx, `find Person p | where p.age > $min | show p.name, p.photo`,
    polydb.Params{"min": 30})
for rows.Next() {
    var name string
    var photo polydb.Blob          // lazy: fetched only when read
    err = rows.Scan(&name, &photo)
}

stmt, err := db.Prepare(ctx, `create Person { name: $name, email: $email }`)
_, err = stmt.Exec(ctx, polydb.Params{"name": "Bob", "email": "b@x.io"})

err = polydb.RetryTx(ctx, db, func(tx polydb.Tx) error {   // retries on 'conflict'
    _, err := tx.Exec(ctx, `update @alice set balance -= 100.00d`)
    return err
})
```

- **Connection URL:** `polydb://user@host:port/database` for TCP and `polydb+unix:///path/to.sock?db=sales` for Unix sockets.
- **Trusting the server** (identity mode), one of:
  - `WithServerFingerprint(fp)`: pin a known fingerprint.
  - `WithKnownPeers(path)`: trust on first use, using 9auth's `known-peers` file (`auth.KnownPeersPath()`), like SSH's `known_hosts`. The CLI shows the fingerprint and asks before saving it on first connect. If a saved fingerprint later doesn't match, the connection is refused with a loud warning, never silently accepted.
  - `WithSystemCAs()` (CA mode): standard certificate verification.
- **Client identity:** `WithIdentity(id)` presents a 9auth identity. The CLI uses the per-user identity from `auth.Load()` (`~/.config/9`) automatically. `WithPasswordFunc(f)` supplies a password for SCRAM.
- **Debugging:** the CLI's `--trace` option prints every frame and message in readable form (field names, decoded values, with secrets redacted).

**As built (Phase 3, 2026-11-22):**
- The interfaces live in the root package `polydb` (`DB`, `Querier`, `Tx`, `Stmt`, `Rows`, `Params`, `Result`, `Error`, `RetryTx`), with plain result types `Entity`, `Field`, `Relation`, `Path` and the lazy `Blob` (an `io.ReaderAt`). `client.Dial` returns a `polydb.DB`; `polydb.Embed(ctx, engine, user, database)` returns one backed by an engine in the same program.
- `Rows` has one result set per statement: `Next` and `Scan` read the current one, `NextResultSet` moves on. If a later statement of a script fails, the earlier results can still be read and the error appears in `Rows.Err` once `NextResultSet` reaches it. `Exec` runs a whole script and sums what it changed.
- `Scan` converts to Go types (`*string`, `*int64`, `*float64`, `*bool`, `*[]byte`, `*time.Time`, `*time.Duration`, `*value.ID`), keeps values as they are for `*any` and `*value.Value`, and takes null only into pointers to pointers. Blob references are fetched when scanned into anything but `*any` or `**Blob`.
- More options: `WithPassword`, `WithConfirmPeer` (ask before trusting a new server), `WithTLSConfig`, `WithInsecure` (plain TCP to a loopback server), `WithCompression` and `WithTrace`. Dial refuses to connect over TLS without a way to trust the server.
- Prepared statements that come back `stale_statement` are prepared again once, automatically.

---

## 14. 9P access layer (browsing and Unix tools)

Besides the native protocol, the server can expose databases as a **9P2000 file tree** (decided 2026-11-22). It isn't meant for applications. Its purpose is the "see the contents of the database" requirement: mount a database and explore it with `ls`, `cat`, `grep`, `find`, `awk` and other Unix tools.

### 14.1 Implementation
- Package `server/ninep`, built on **`github.com/sandgorgon/9p`**: MIT licensed, pure Go, standard library only. It's one of the two approved dependencies outside the standard library and `golang.org/x`. The other is `github.com/sandgorgon/9auth`, used for TLS identities.
- PolyDB implements the library's `server.FileSystem` / `server.File` interfaces. Every file operation becomes a Rhizome operation inside the engine, so **the same permission checks apply** as on the native protocol.
- Protocol versions: 9P2000, plus 9P2000.u for symlinks. 9P2000.L isn't supported by the library, so Linux mounts must pass `version=9p2000.u` (the kernel defaults to `.L`).

### 14.2 Transports and identity
The library has no 9P-level authentication, so identity comes from the connection:

| Transport | Default | Identity |
|---|---|---|
| Unix socket `/var/run/polydb/9p.sock` | **on** when `--9p` is given | OS peer credentials (read in the library's per-connection hook), mapped to PolyDB users through the same mapping as `PEER` (§3.1) |
| TCP with TLS (`--9p-tcp`) | off | the client's 9auth identity. The TLS handshake **requires** a client certificate, and its fingerprint must be registered to a PolyDB user (§3.4). The library's per-connection hook records that user, and the attach user name is ignored |
| TCP without TLS | only with `--insecure`, loopback only | the attach user name, unverified (development only) |

- **Remote 9P with 9-family tools:** because identity comes from 9auth, a remote `9sh` (or any client built on the 9p library over a TLS connection) can attach a PolyDB database and graft it into its namespace with `ns.FromFid`. The same key works for `polydb`, `9sh` and `9vcs` on that machine.
- On the 9P TCP listener, 9auth's `ServerTLSConfig` is used directly. Requiring a client certificate is right here, because 9P has no password fallback.
- The Linux kernel's 9P client can't do TLS, so kernel mounts always use the Unix socket.

- **Kernel mounts:** a Linux kernel mount connects as root, because mounting needs root. When the peer is root and the mount uses `access=user`, the kernel opens a separate attach for each local user, and the server trusts the user ID in that attach and maps it like a peer credential. Only root can create such a mount, so this doesn't let ordinary users impersonate each other.
- **macOS** has no built-in 9P mounting. Use the `polydb 9p ls|cat|get` CLI commands, the library's `9pc` tool, or a FUSE-based 9P client.

### 14.3 File tree

```
/                                   databases this user can read
/<db>/
  kinds/<Kind>/<entity-id>/         one directory per entity
      fields/<name>                 the field's value (format below)
      fieldinfo/<name>              id, type, doc, declared: one per line
      meta                          entity metadata
      describe                      same text as the Rhizome 'describe' command
      out/<rel-type>/<rel-id>       relation to the target entity (symlink with .u)
      in/<rel-type>/<rel-id>        relation from the source entity
  entities/<entity-id>              every entity, including kindless ones (symlink with .u)
  relations/<rel-type>/<rel-id>/    from, to, props
  schema/kinds/<Kind>               the kind's 'define' statement
  schema/relations/<rel-type>
  schema/formats/<format>
  query/clone                       open and read it to get a new query directory N
  query/<N>/ctl                     write Rhizome text here (and 'format tsv|json|rhizome')
  query/<N>/data                    read results from here
  query/<N>/error                   the error, if the query failed
```

**How values appear in files:**
- `bytes` and `image` fields contain the **raw bytes**, so `cat …/fields/photo > alice.png` gives a working image file.
- `text` fields contain the UTF-8 text.
- All other values contain their Rhizome literal form followed by a newline, for example `34`, `{"eng", "ops"}`, `t"2026-11-21T14:30:00Z"`.
- Queue files show the queue's items in pop order without popping them.
- File sizes in `stat` are exact for bytes, text and images. For other types they're computed when the file is opened.

**Relations without symlinks:** if a client only speaks plain 9P2000, each `out/` and `in/` entry is a small file containing the path of the other entity.

**Query results:** `data` is tab-separated by default, with a header row, so it works directly with `cut`, `awk` and `sort`. `json` (one object per line) and `rhizome` (literal syntax) can be chosen through `ctl`.

### 14.4 Behaviour
- **Read-only by default.** With the server option `ninep.writable = true`, writing to a `fields/<name>` file runs an auto-commit `set` on that field. The written text is parsed as a Rhizome literal of the field's type; for bytes and image fields the raw bytes are written.
- **Not available through 9P in v1:** creating or removing entities, relations or fields, transactions, and queue pops (all fail with a permission error). These need the native protocol.
- **Consistency:** each opened file or directory reads from the snapshot taken when it was opened. Reading it in several chunks therefore never mixes two versions. Snapshots held through 9P are released when the file is closed, or after 5 minutes (server setting).
- **Large directories:** entity directories (`kinds/<Kind>/`) are listed in ID order and streamed, so `ls` on a kind with millions of entities works but is slow. `find` and query files are better for large kinds.
- **Cancellation:** a 9P `Tflush` cancels the operation behind it, which the library passes to PolyDB as context cancellation.

### 14.5 Example (Linux)
```
polydb serve --9p
sudo mount -t 9p -o trans=unix,version=9p2000.u,access=user \
     /var/run/polydb/9p.sock /mnt/polydb

ls /mnt/polydb/sales/kinds/Person | head
cat /mnt/polydb/sales/kinds/Person/01JD8X3K7M0000Q2W5ZB9N4T6R/fields/name
grep -l eng /mnt/polydb/sales/kinds/Person/*/fields/tags
ls -l /mnt/polydb/sales/kinds/Person/01JD8X3K7M0000Q2W5ZB9N4T6R/out/knows/
```

---

## 15. Open questions

None.

### Decided (2026-11-22)
1. A failed or cancelled statement inside a transaction marks the whole transaction failed; the client must roll back.
2. Default port 7377, confirmed unassigned in the IANA registry.
3. 9P access layer added for browsing and Unix tools (§14), using `github.com/sandgorgon/9p` as an approved external dependency.
4. `github.com/sandgorgon/9auth` (v0.2.0+, which added `LoadFrom`) approved as a dependency:
   - server identity (default TLS mode)
   - `KEY` login, which replaces the post-v1 `CERT` plan
   - trust-on-first-use server pinning in the CLI
   - authenticated remote 9P over TLS
5. Phase 3 decisions (2026-11-22):
   - Users, roles, grants and keys live in a **system store** with typed records, separate from the databases, so credentials can never be read through Rhizome.
   - Until persistence (Phase 4), `polydb serve` created `admin` at startup. Since Phase 4, `polydb init` (§3.3) does it once: the password comes from `POLYDB_ADMIN_PASSWORD`, a prompt (asked twice), or is generated and printed once (`--generate`, or when there is no terminal), and `--key` registers the running user's 9auth key. `init` refuses once admin has a password or key, and while a server holds the data directory. Before `init`, `polydb serve` says so and admits only the OS user running it, over the Unix socket.
   - The data directory holds the server's identity, its Unix socket, and its data: a system store (databases, users, roles, grants, keys) and one store per database. `polydb serve --durability relaxed` acknowledges commits before they are synced, for bulk loads; a crash can then lose the last moments of commits, never part of one.
   - Blob references (§7) are part of the Phase 3 server, served from values the server holds for the lease time.
   - The inline value limit is configurable by the server and requested by the client (HELLO field 7). The Go client offers `WithInlineLimit(n)`.
6. Server behaviour (Phase 3, 2026-11-22):
   - If the database named in HELLO can't be used (it doesn't exist, or the user has no permission on it), the login still succeeds and AUTH_OK's `database` is null.
   - Prepared statements are checked again on every EXECUTE, so a schema change makes them fail only when they no longer fit. `stale_statement` is returned for an unknown or closed statement ID. PREPARED reports every parameter with type `any`, because parameter types come from the values supplied.
   - `created_ids` in RESULT_END lists the IDs returned by `create` and `relate`.
   - A blob reference stays valid for the lease time (10 minutes by default) from when its result was sent, whether or not a transaction is open.
   - CANCEL stops statements that haven't finished. A statement that completed before the cancel arrived keeps its effects and its result is sent.
   - `show sessions` (admin) lists each connection's session ID, user, database, remote address, time connected and request count.
7. Client and CLI behaviour (Phase 3, 2026-11-22):
   - A client with a 9auth identity tries `KEY` first. If the identity isn't registered to the user, the login fails and the server closes the connection, so the client connects again and logs in with its password, if it has one.
   - On a Unix socket, `PEER` is preferred unless a password was given up front (`WithPassword`, the URL, or `POLYDB_PASSWORD` in the CLI); a password prompt is only a fallback.
   - `polydb serve` keeps its identity and Unix socket in its data directory (`$POLYDB_DATA`, default `~/.local/share/polydb`), because `/var/run/polydb` needs root; a system service passes `--socket /var/run/polydb/polydb.sock`. Socket paths are limited to about 100 bytes by the OS, and longer ones are refused with a clear message.
   - The CLI connects to `$POLYDB_URL`, or to the local server's socket. It uses the user's 9auth identity and known-peers file automatically, asks before trusting a new server, and asks for a password only when one is needed.
