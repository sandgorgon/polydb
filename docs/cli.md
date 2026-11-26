# The `polydb` command

One program does everything: it runs the server, opens the interactive
shell, runs scripts, and moves data in and out.

| Command | What it does |
|---|---|
| `polydb` | open the interactive shell |
| `polydb run file.rhz` | run a script of statements |
| `polydb serve` | run the server |
| `polydb init` | set the administrator's password (once, before first real use) |
| `polydb export` / `polydb import` | copy a database, a query's result, or the users, to or from a file |
| `polydb restore` | restore a backup into a stopped server's data directory |
| `polydb whoami` | show this computer's identity key, for password-free login |
| `polydb version` | show the version |

`polydb <command> -h` lists a command's options. Options can be written
with one dash or two: `-memory` and `--memory` are the same.

## Connecting: the URL

The shell, `run`, `export` and `import` connect to a server named by a
URL:

| URL | Connects to |
|---|---|
| *(none)* | `$POLYDB_URL` if set, otherwise the server on this computer, through its Unix socket |
| `polydb://ana@db.example.com/sales` | the server at `db.example.com`, port 7377, as user `ana`, using database `sales` |
| `polydb://ana@db.example.com:9000/sales` | the same, on port 9000 |
| `polydb+unix:///run/polydb/polydb.sock?db=sales` | a server's Unix socket on this computer |

The user and database are optional. Over TCP, connections are always
encrypted (TLS).

**Passwords** are asked for when needed. For scripts, set
`POLYDB_PASSWORD` instead. On the Unix socket, the operating-system
user who runs the server logs in as `admin` with no password.

**Trusting a server.** The first time you connect to a server over the
network, `polydb` shows the server's *fingerprint* (a long hex string
the server prints when it starts) and asks whether to trust it. Check
that it matches, and `polydb` remembers it from then on, much like SSH.
If a server's fingerprint ever changes, `polydb` refuses to connect and
warns you, because something may be impersonating it.

Options for connecting, accepted by the shell, `run`, `export` and
`import`:

| Option | Meaning |
|---|---|
| `--fingerprint <hex>` | trust only a server with this fingerprint (no question asked) |
| `--system-cas` | verify the server with the usual certificate authorities instead, for servers set up with a CA-signed certificate |
| `--insecure` | connect without TLS; the server must allow it, and only from the same computer |
| `--memory` | don't connect: use a temporary database in memory |
| `--format table\|tsv` | how results are printed: a table for people, or tab-separated for scripts |
| `--no-rc` | don't run `~/.polydbrc` |
| `--trace` | print every protocol message, for debugging (passwords are hidden) |

## The shell

```sh
polydb                                     # the local server
polydb polydb://ana@db.example.com/sales   # a remote one
polydb --memory                            # a scratch database
```

Type Rhizome statements at the prompt. The prompt shows the database
in use: `polydb:sales>`. Inside a transaction it shows `*`
(`polydb:sales*>`), and `!` when the transaction has failed and must be
rolled back.

- Enter runs a complete statement; the `;` is added for you.
- To continue a query on the next line, end the line with `|`. An empty
  line runs whatever has been typed so far.
- Ctrl-C stops a running statement. Ctrl-D or `\q` quits.

Shell commands start with a backslash:

| Command | Does |
|---|---|
| `\?` | help |
| `\set name value` | set the parameter `$name` (the value is written in Rhizome: `\set min 30`, `\set who "Ana"`) |
| `\set` | list parameters |
| `\unset name` | remove a parameter |
| `\i file.rhz` | run the statements in a file |
| `\timing` | show how long each statement takes |
| `\format table` or `\format tsv` | change how results are printed |
| `\q` | quit |

If `~/.polydbrc` exists, the shell runs it first: a good place for
`\timing` or `use mydb;`.

## `polydb run`: scripts

```sh
polydb run setup.rhz
polydb run polydb://ana@db.example.com/sales report.rhz
echo 'show databases;' | polydb run -
```

Runs every statement in the file (or standard input, for `-`), printing
results, and stops at the first error with a non-zero exit status.
Takes the same options as the shell; `--format tsv` gives output that's
easy for other programs to read.

## `polydb serve`: the server

```sh
polydb serve
polydb serve --config /etc/polydb/polydb.conf
```

Runs until stopped with Ctrl-C or a `SIGTERM`, then finishes the
requests in progress and exits. On start it prints its fingerprint and
where it listens.

Every setting can be given as an option or in a **configuration file**
(`--config file`): one setting per line as `name = value`, with the
option's name without dashes. `#` starts a comment. Options given on
the command line win over the file. An example file with every setting
explained comes with each release (`polydb.conf`).

```text
# /etc/polydb/polydb.conf
data-dir = /var/lib/polydb
listen = :7377
max-memory = 2GiB
statement-timeout = 5m
```

Sizes take units (`512MiB`, `2GiB`, `500MB`) or plain bytes. Durations
look like `30s`, `5m`, `1h30m`.

| Setting | Default | Meaning |
|---|---|---|
| `data-dir` | `$POLYDB_DATA`, else `~/.local/share/polydb` | where the server keeps its data, identity and socket |
| `listen` | `:7377` | the network address to listen on; empty for none |
| `socket` | `<data-dir>/polydb.sock` | the Unix socket's path; `none` for none |
| `tls-cert`, `tls-key` | *(none)* | a certificate and key from a certificate authority, instead of the server's own identity |
| `insecure` | `false` | serve the network port without TLS, to this computer only (for development) |
| `durability` | `durable` | `durable`: a change is on disk before PolyDB says it's done. `relaxed`: much faster, for bulk loading; a crash can lose the last moments of changes, but never half of one |
| `backup-dir` | `<data-dir>/backups` | where `backup`, `restore`, `export … to` and `import … from` read and write |
| `statement-timeout` | none | the longest a statement may run |
| `max-memory` | `1GiB` | the most memory one statement may use; `-1` for no limit |
| `max-connections` | `1000` | connections at once (administrators may go past it) |
| `max-user-connections` | `100` | connections per user |
| `max-transactions` | `16` | open transactions per session |
| `max-upload` | `4GiB` | the largest single upload |
| `upload-quota` | `16GiB` | how much one user may have uploaded and not yet used |
| `slow-query` | `1s` | statements at least this slow are logged with their plan; `0` turns it off |
| `compression` | `false` | allow compressed connections |
| `inline-limit` | `16KiB` | larger values are sent to clients in pieces, on request |
| `v` | `false` | log every connection |

[Running PolyDB](operations.md) explains the limits, TLS and running the
server as a service.

## `polydb init`: the first administrator

Every server has a built-in `admin` user. Until it has a password (or a
key), only the operating-system user who owns the data can log in, and
only through the Unix socket. `init` sets it up. Stop the server first:

```sh
polydb init                 # asks for admin's password
polydb init --generate      # makes a strong password and prints it once
polydb init --key           # also lets this computer's identity log in as admin
```

`POLYDB_ADMIN_PASSWORD` gives the password without a question, for
automated installs. `--data-dir` picks the server's data directory. It
runs once: after that, change admin's password with
`alter user admin password …`.

## `polydb export` and `polydb import`

Copy data to and from files, through a running server.

```sh
polydb export --database sales -o sales.jsonl          # a whole database
polydb export --database sales --as native -o sales.native
polydb export --query 'find Person p | show p.name, p.email' --as csv -o people.csv
polydb export --security -o users.jsonl                # users, roles and permissions

polydb import --database sales_copy sales.jsonl        # into an existing database
polydb import --database sales --new-ids sales.jsonl   # a second copy, with new IDs
polydb import --security users.jsonl
```

| Option | Meaning |
|---|---|
| `--database d` | export or import this database |
| `--query q` | export the result of a query, in the URL's database |
| `--security` | users, roles and grants. The export includes password hashes, so keep it secret, or add `--no-passwords` |
| `--as` | `jsonl` (readable, the default) or `native` (smaller and faster); for a query also `csv`, `dot` (a Graphviz drawing) or `graph` (JSON nodes and edges) |
| `-o file` | where to write (`-` for the screen, the default) |
| `--new-ids` | on import, give everything new IDs, to copy data into a database that already holds it |
| `--replace` | on security import, also replace the passwords and keys of users that exist |

Plus the connection options above. Exports stream: they never need the
whole database in memory.

## `polydb restore`

```sh
polydb restore --data-dir /var/lib/polydb --database sales_old /backups/nightly/mon
```

Restores a backup directory (made with `backup database`) as a new
database, into the data directory of a **stopped** server. While a
server runs, use the `restore database` statement instead
([Running PolyDB](operations.md#backups)).

## `polydb whoami`

```sh
polydb whoami
```

Shows this computer's identity: a key pair created the first time any
of these tools needs it, stored in `~/.config/9`. An administrator can
register its fingerprint for your user (`alter user ana add key "…"`),
after which you log in from this computer with no password.

## Environment variables

| Variable | Used for |
|---|---|
| `POLYDB_URL` | the server to connect to when no URL is given |
| `POLYDB_PASSWORD` | the password, instead of asking |
| `POLYDB_DATA` | the default data directory for `serve`, `init` and `restore` |
| `POLYDB_ADMIN_PASSWORD` | the admin password for `polydb init`, instead of asking |
