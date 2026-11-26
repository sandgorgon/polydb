# Getting started

This guide takes about fifteen minutes. You'll install PolyDB, start a
server, and build a small database about a team: who the people are,
which projects exist, and who works on what. Along the way you'll meet
the main ideas. If a word is new to you, the
[concepts page](concepts.md) explains it, and the
[glossary](glossary.md) has every term in one place.

## 1. Install

PolyDB is a single program called `polydb`. Pick one way to get it:

- **Download a release.** From the project's releases page, download the
  archive for your system (for example `polydb_1.0.0_linux_amd64.tar.gz`)
  and `SHA256SUMS`. Check the download, unpack it, and put `polydb`
  somewhere on your `PATH`:

  ```sh
  sha256sum --check --ignore-missing SHA256SUMS
  tar -xzf polydb_1.0.0_linux_amd64.tar.gz
  sudo install polydb_1.0.0_linux_amd64/polydb /usr/local/bin/
  ```

- **Build it with Go** (Go 1.26 or later):

  ```sh
  go install github.com/sandgorgon/polydb/cmd/polydb@latest
  ```

Check that it works:

```sh
polydb version
```

## 2. Start a server

Open a terminal and run:

```sh
polydb serve
```

It prints something like this, and keeps running:

```text
not initialized: only this OS user can log in, over the Unix socket (run polydb init)
server fingerprint: de775563fdac46f5f638f5d76512119fac84816da6fb194e6a0741b54571a14c
listening on [::]:7377 (TLS)
listening on /home/you/.local/share/polydb/polydb.sock
```

What this means:

- The server keeps its data in `~/.local/share/polydb`.
- It listens in two places: a network port (7377), which is always
  encrypted, and a *Unix socket*, a file that only programs on this
  computer can connect through.
- It hasn't been set up with passwords yet, so for now only you (the
  operating-system user who started it) can log in, and only through the
  socket. That's all we need for this guide. [Operations](operations.md)
  covers passwords and remote access.

Leave it running. When you want to stop it, press Ctrl-C there.

> **Just exploring?** `polydb --memory` opens a shell on a temporary
> database in memory, with no server at all. Everything is gone when
> you quit. All of this guide works that way too.

## 3. Open the shell

In a second terminal, run:

```sh
polydb
```

You're now in PolyDB's shell, where you type statements in **Rhizome**,
PolyDB's language. The prompt is `polydb>`. A few things to know:

- Each statement ends with `;`. When you press Enter on a complete
  statement, the shell adds the `;` for you.
- To continue a longer statement on the next line, end the line with `|`
  (for queries) or just keep typing inside `{ … }`.
- `\?` shows help, and `\q` (or Ctrl-D) quits.

A server can hold many databases. Make one for the team and switch to
it:

```rhizome
create database team;
use team;
```

The prompt becomes `polydb:team>`.

## 4. Describe what you'll store

In PolyDB, each thing you store is an **entity**: a person, a project,
an invoice. An entity has **fields**, named values like `name` or
`email`.

You can store entities without any setup, but it's usually better to
first describe each kind of thing: what fields it has and what type
each one is. That description is a **kind**, and it catches mistakes
such as a misspelled field or a number where text belongs.

```rhizome
define kind Person {
  name   : text              "Full name"
  email  : text unique       "Work email"
  skills : set<text> default {}
  joined : date default today()
};

define kind Project {
  title  : text
  status : text default "planned"
};
```

Reading the `Person` kind line by line:

- `name : text "Full name"`: a text field, with a note saying what it's
  for. Notes like this are optional but show up in `describe`.
- `email : text unique`: no two people can have the same email.
- `skills : set<text> default {}`: a *set* of texts (a collection with no
  duplicates), empty unless given.
- `joined : date default today()`: a date, filled in automatically.

Fields without `default` and without `?` after the type (you'll see
`text?` later) are required.

## 5. Add data

```rhizome
create Person @ana  { name: "Ana Lima",   email: "ana@example.com",  skills: {"go", "sql"} };
create Person @ben  { name: "Ben Okafor", email: "ben@example.com",  skills: {"design"} };
create Person @cleo { name: "Cleo Park",  email: "cleo@example.com", skills: {"go", "design"} };

create Project @site { title: "Website", status: "active" };
create Project @app  { title: "Mobile app" };
```

Each `create` prints the new entity's **ID**, a unique identifier like
`#01M89MMRDB000000MHP57692N1`. The `@ana` part is a **handle**: a
nickname for that entity that lasts until you leave the shell, so you
don't have to copy IDs around.

## 6. Ask questions

Queries start with `find` and continue with steps joined by `|`, read
from left to right, like a pipeline:

```rhizome
find Person p | show p.name, p.email;
```

```text
p.name     │ p.email
───────────┼─────────────────
Ana Lima   │ ana@example.com
Ben Okafor │ ben@example.com
Cleo Park  │ cleo@example.com
(3 rows)
```

`find Person p` means "every Person, calling each one `p`". Then
`show` picks what to print. Add `where` to filter and `sort` to order:

```rhizome
find Person p | where p.skills has "go" | sort p.name | show p.name;
```

```text
p.name
─────────
Ana Lima
Cleo Park
(2 rows)
```

## 7. Connect things

So far people and projects are separate. A **relation** links one
entity to another, with a name for the link. Relations are what make
PolyDB a *graph* database: you can follow them in queries.

You can relate things without any setup, but just like kinds, you can
first describe a relation. Here, `works_on` goes from a Person to a
Project and may say what the person's job is on it:

```rhizome
define relation works_on from Person to Project "Who works on what" {
  job : text?
};

relate @ana  ~works_on{job: "lead"}~> @site;
relate @ben  ~works_on{job: "design"}~> @site;
relate @cleo ~works_on~> @app;
relate @ben  ~works_on~> @app;
```

The arrow `~works_on~>` points from the person to the project. `text?`
means the `job` is optional, so Cleo's link doesn't need one.

Now follow the links. `follow` walks along relations from each row:

```rhizome
find Person p
  | follow p ~works_on r~> Project j
  | sort p.name, j.title
  | show p.name, j.title, r.job;
```

```text
p.name     │ j.title    │ r.job
───────────┼────────────┼───────
Ana Lima   │ Website    │ lead
Ben Okafor │ Mobile app │ null
Ben Okafor │ Website    │ design
Cleo Park  │ Mobile app │ null
(4 rows)
```

`r` names the relation itself, so you can read its `job`. `null` means
"no value". Arrows can point backwards too: who works on the website?

```rhizome
find Project j
  | where j.title = "Website"
  | follow j <~works_on~ Person p
  | show p.name;
```

And how many people does each project have?

```rhizome
find Person p
  | follow p ~works_on~> Project j
  | group by j.title as project
  | sort project
  | show project, count() as people;
```

```text
project    │ people
───────────┼───────
Mobile app │ 2
Website    │ 2
(2 rows)
```

## 8. Change things

`update` changes fields. `++=` adds to a collection (or joins text):

```rhizome
update @app set status = "active";
update @cleo set skills ++= {"sql"};
```

Queries can change data too, by ending with a data step. This marks
every planned project as active:

```rhizome
find Project j | where j.status = "planned" | update j set status = "active";
```

And because `email` is `unique`, PolyDB refuses a second person with
Ana's email:

```rhizome error
create Person { name: "Dan Reyes", email: "ana@example.com" };
```

```text
error: constraint: unique field Person.email already has this value, in entity #01M89MNM2W000000QMYFB6JHXM
```

## 9. Look around

```rhizome
show kinds;
describe kind Person;
describe @ana;
```

`show kinds` lists the kinds in the database, `describe kind Person`
shows its fields and rules, and `describe @ana` shows everything about
one entity: its fields, its relations, and *metadata* such as when it
was created and by whom.

## 10. Come back later

Quit with `\q`. The data stays in the server; the handles (`@ana`)
don't, because they belong to your shell session. Next time, find
things with a query:

```rhizome
find Person p | where p.email = "ana@example.com" | show p.name, p.skills;
```

## Where next

- [Concepts](concepts.md): the ideas behind what you just did, and how
  they compare with SQL.
- [A tour of Rhizome](rhizome-tour.md): the language step by step:
  more query steps, deeper graph patterns, grouping, text search,
  transactions.
- [Using PolyDB from Go](embedding.md): put a database inside your own
  program.
- [Running PolyDB](operations.md): passwords, users, backups, and
  running it as a service.
