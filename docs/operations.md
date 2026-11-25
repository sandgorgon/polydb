# Running PolyDB

This page is for whoever runs a PolyDB server for others: installing it
as a service, letting people connect safely, giving them the right
permissions, backing up, and keeping an eye on it.

## Install as a service (Linux, systemd)

Each release archive contains `polydb`, an example configuration
`polydb.conf` (every setting explained), and a systemd unit
`polydb.service`. From the unpacked archive:

```sh
sudo useradd --system --home /var/lib/polydb --shell /usr/sbin/nologin polydb
sudo install -m 755 polydb /usr/local/bin/polydb
sudo install -D -m 640 -g polydb polydb.conf /etc/polydb/polydb.conf
sudo install -m 644 polydb.service /etc/systemd/system/polydb.service
```

Edit `/etc/polydb/polydb.conf` if needed (the defaults are sensible),
then set the administrator's password and start the server:

```sh
sudo -u polydb polydb init --data-dir /var/lib/polydb   # asks for admin's password
sudo systemctl enable --now polydb
sudo journalctl -u polydb                                # its log, including its fingerprint
```

The unit runs the server as the unprivileged `polydb` user, with its
data in `/var/lib/polydb` and its socket in `/run/polydb`, and with the
usual systemd protections. On the server itself, the `polydb` OS user
can always get in as admin through the socket:

```sh
sudo -u polydb polydb polydb+unix:///run/polydb/polydb.sock
```

On macOS, or anywhere else, run `polydb serve --config <file>` under
whatever keeps services running there.

## Where the data lives

Everything is in the data directory (`data-dir`, `/var/lib/polydb`
above):

| Path | What |
|---|---|
| `system/` | the list of databases, and the users, roles, permissions and keys |
| `databases/<n>/` | one directory per database, with its large values in `blobs/` |
| `identity/` | the server's key, which gives its fingerprint. Back it up: a new one means clients must trust the server again |
| `backups/` | the default place for backups and exports |

Only one server can use a data directory at a time. Don't copy or edit
these files while the server runs; use [backups](#backups).

## Connections and TLS

The server listens on TCP port 7377 and on a Unix socket.

**TCP is always encrypted** with TLS. Clients must know they reach the
real server. There are two ways:

- **The server's own identity** (the default). The server creates a key
  on first start and prints its **fingerprint**. Give the fingerprint to
  your users: the `polydb` shell shows it on first connect and asks them
  to confirm it, and programs pass it to `client.WithServerFingerprint`.
  Nothing to buy or renew.
- **A certificate from a certificate authority**, for example from
  Let's Encrypt or your company. Set `tls-cert` and `tls-key`; clients
  connect with `--system-cas` (`client.WithSystemCAs()`), with no
  fingerprint to share.

**The Unix socket** works only on the same computer, and the file's
permissions decide who can use it.

`--insecure` turns TLS off for TCP, and then accepts connections only
from the same computer. Use it only for development.

## Users and permissions

Every server has an `admin` user who can do everything. Create a user
for each person or program, and give them only what they need. Users
log in with a password, or with a key (see below).

```rhizome
create database shop;
create user ana password "a long passphrase";
create user reports password "another long passphrase";
```

Permissions say what a user may do:

| Permission | Allows |
|---|---|
| `read` | queries, `describe`, `history` |
| `write` | creating, changing and deleting entities |
| `relate` | creating and removing relations |
| `define` | defining and changing kinds, relation types, formats and indexes |
| `admin` | everything, including users and databases |

They're granted on a whole database or on one kind (written
`database.Kind`). A **role** is a named set of permissions to give to
several users:

```rhizome
create role analyst;
grant read on database shop to analyst;
grant analyst to reports;

grant read on database shop to ana;
grant write on database shop to ana;
grant relate on database shop to ana;
```

Check and change them:

```rhizome
show users;
show roles;
show grants for ana;
revoke write on database shop from ana;
```

Users change their own password with `alter user ana password "…"`.
Admins can change anyone's.

**Keys instead of passwords.** On their computer, a user runs
`polydb whoami` and sends you the fingerprint it prints. You register
it, and from then on they log in from that computer with no password:

```rhizome norun
alter user ana add key "3f9a…c41e" as "ana's laptop";
show keys for ana;
```

A user without `read` on a kind doesn't see its entities at all, and
gets a `permission` error naming it directly.

## Backups

A backup copies one database, as of one moment, into a directory, while
the database keeps working. Paths are inside the backup directory
(`backup-dir`, by default `<data-dir>/backups`):

```rhizome
use shop;
backup database shop to "nightly/2026-11-25";
```

To restore, create a new database from the backup. A restore never
overwrites a database; to replace one, drop it first, or restore under
another name and switch your applications to it:

```rhizome
restore database shop_restored from "nightly/2026-11-25";
```

Every file is checked against the checksums recorded in the backup,
so a damaged backup is refused rather than restored. On a stopped
server, `polydb restore` does the same from any directory.

Things to know:

- A backup holds the data and its history, but **not users and
  permissions**, which belong to the whole server. Export them
  separately (below).
- Back up the server's `identity/` directory once, too.
- A backup made by any 1.x version can be restored by any later 1.x.
- Schedule backups with `polydb run` from cron or a systemd timer:
  `echo 'backup database shop to "daily/'$(date +%F)'";' | polydb run -`

## Export and import

An export is a portable copy of a database in a file, as readable JSON
Lines or a compact native format. Use it to move data between servers,
into a newer major version, or into other tools:

```rhizome
export database shop to "shop.jsonl";
export security to "users.jsonl";
```

`export security` writes the users, roles and permissions, including
password hashes: keep that file as secret as the passwords. Import into
another server with `import into database … from` and
`import security from`. The `polydb export` and `polydb import`
commands do the same from any computer ([details](cli.md#polydb-export-and-polydb-import)).

## Limits

Limits keep one heavy query or one busy client from slowing everyone
down. Set them in the configuration file:

| Setting | Default | What happens when it's reached |
|---|---|---|
| `statement-timeout` | none | the statement stops |
| `max-memory` | `1GiB` | a statement that would hold more (rows being sorted or grouped, large values read into memory) stops |
| `max-connections` | `1000` | new connections are refused, except for administrators |
| `max-user-connections` | `100` | that user's new connections are refused |
| `max-transactions` | `16` | a session can't begin more |
| `max-upload` | `4GiB` | the upload is refused |
| `upload-quota` | `16GiB` | a user's further uploads are refused until they use or let expire what they have |

Each is a `limit` error, with a message saying which limit. Users may
lower the time and memory limits for their own session
(`set timeout = 30s`, `set max_memory = 100000000`) but only an
administrator can raise them. `show limits` lists them all.

## Keeping an eye on it

| Statement | Shows |
|---|---|
| `show sessions` | who is connected, and what they're running (admin) |
| `show slow queries` | recent statements that took longer than `slow-query`, with how they ran (admin) |
| `show stats` | the size of a database: entities and relations by kind |
| `show indexes` | indexes, and the progress of ones being built |
| `show alters` | the progress of background `alter kind` changes |
| `show limits` | the limits that apply |

```rhizome
show stats;
show indexes;
show slow queries;
```

The server writes warnings and slow statements to its standard error,
which systemd keeps in the journal. `-v` (or `v = true`) also logs every
connection.

## Upgrading

1. Read the CHANGELOG for anything to do.
2. Back up every database, and export the users (`export security`).
3. Stop the server, replace the `polydb` binary, start it again.

Within version 1, a newer PolyDB reads everything an older one wrote,
and old clients keep working with new servers and the other way round
([what's promised](compatibility.md)). Going back to an older version
after upgrading isn't promised; restore from your backups if needed.

## Where next

- [The `polydb` command](cli.md): every command and setting
- [FAQ](faq.md): common problems and what the error messages mean
