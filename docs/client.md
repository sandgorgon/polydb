# Connecting to a server from Go

The `client` package connects a Go program to a PolyDB server. The
connection is a `polydb.DB`, the same interface as an
[embedded database](embedding.md), so everything on that page (queries,
parameters, transactions, errors, large values) works the same way
here. This page covers what's different: connecting and logging in.

```sh
go get github.com/sandgorgon/polydb
```

## Connect

```go
import (
	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/client"
)

db, err := client.Dial(ctx, "polydb://ana@db.example.com/sales",
	client.WithPassword(os.Getenv("POLYDB_PASSWORD")),
	client.WithServerFingerprint("de775563fdac46f5…"))
if err != nil {
	log.Fatal(err)
}
defer db.Close()

rows, err := db.Query(ctx, `find Person p | show p.name`, nil)
```

The URL says where to connect, as whom, and which database to use:

| URL | Meaning |
|---|---|
| `polydb://ana@db.example.com/sales` | TCP with TLS, port 7377, user `ana`, database `sales` |
| `polydb://ana@db.example.com:9000/sales` | the same on port 9000 |
| `polydb+unix:///run/polydb/polydb.sock?db=sales` | the Unix socket of a server on this computer |

A connection can be shared by many goroutines at once. It is one
session: a `use` statement or a handle like `@ana` applies to everyone
using it, so give each independent task its own connection if they
need different databases or handles.

## Trusting the server

Over TCP, connections are encrypted, and the client must know it's
talking to the right server, or someone in between could pretend to be
it. Pick one:

| Option | Use when |
|---|---|
| `WithServerFingerprint(fp)` | you know the server's fingerprint (printed by `polydb serve` when it starts). The simplest choice for programs |
| `WithKnownPeers(path)` | like SSH: remember each server's fingerprint the first time, refuse if it changes. Add `WithConfirmPeer(f)` to ask before trusting a new server |
| `WithSystemCAs()` | the server has a certificate from a certificate authority (`tls-cert` setting) |
| `WithTLSConfig(cfg)` | you need full control of TLS |

`Dial` refuses to connect over TCP without one of these.
`WithInsecure()` connects without TLS to a server started with
`--insecure`, which only accepts connections from the same computer:
for development only.

## Logging in

| Option | How |
|---|---|
| `WithPassword(pw)` | a password. It's never sent: the exchange (SCRAM) proves you know it, and proves the server knows it too |
| `WithPasswordFunc(f)` | calls `f` for the password only if it's needed |
| `WithIdentity(id)` | a key instead of a password: this computer's identity (see `polydb whoami`), registered for the user by an admin |
| *(on a Unix socket)* | the operating-system user who runs the server logs in as `admin`, with no password |

## Other options

| Option | Does |
|---|---|
| `WithCompression()` | compress messages, if the server allows it; helps on slow networks |
| `WithInlineLimit(n)` | values larger than `n` bytes come as a `*polydb.Blob`, read when you read it |
| `WithTrace(f)` | calls `f` with a description of every message, for debugging |

## What's specific to a connection

`*client.Conn` adds a few methods to `polydb.DB`:

- `Database()` and `User()`: the current database and the logged-in user.
- `ServerVersion()`: the server's PolyDB version.
- `Upload(ctx, r, typ, mediaType)`: stream a large value to the server
  (see [large values](embedding.md#large-values)). The server keeps it
  for 10 minutes (by default) for a statement to use.

## When things go wrong

- A lost connection fails the requests in progress with the code
  `connection_lost`. Transactions open on it are rolled back by the
  server. Dial again to continue.
- A server shutting down finishes the requests it has, then closes.
- A failed login is always `auth_failed`, without saying whether the
  user exists. Too many failures from one address are slowed down.
- `polydb.IsRetryable(err)` tells you whether running the same thing
  again could succeed (a `conflict`, for instance).

## Where next

- [Using PolyDB from Go](embedding.md): queries, transactions and
  large values in detail
- [Running PolyDB](operations.md): setting up users and TLS on the
  server
