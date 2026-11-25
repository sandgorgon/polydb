# Concepts

This page explains the ideas PolyDB is built on, in the order you'll
meet them. You don't need to know any other database to follow it. If
you do know SQL, the [table at the end](#if-you-know-sql) maps the
familiar words to PolyDB's.

## The big picture

PolyDB stores **things** and the **links between them**.

- A *thing* can be anything you want to keep: a person, an order, a
  photo, a configuration file, a to-do list.
- A *link* says how two things are connected: Ana *works on* the
  website, an order *was placed by* a customer.

Many databases are good at one shape of data: tables of rows, or
documents, or graphs, or files. PolyDB holds all of them in one place
and answers questions about all of them with one language, **Rhizome**.
You don't have to split your data between several systems and keep
them in step.

## Server and databases

A PolyDB **server** is a program (`polydb serve`) that keeps data and
answers requests. It can hold many **databases**, each a separate,
named collection of data: one for a shop, another for a team.
You work in one database at a time:

```rhizome
create database shop;
use shop;
```

PolyDB can also run **inside your own Go program**, with no server at
all ([Using PolyDB from Go](embedding.md)).

## Entities and fields

Each thing you store is an **entity**. An entity has **fields**: named
values such as `name: "Ana"` or `price: 19.99d`. Every entity also gets:

- an **ID**, a unique identifier that never changes, written like
  `#01M89MMRDB000000MHP57692N1`;
- **metadata**: when it was created and last changed, by whom, and a
  version number, kept by PolyDB, plus **labels** (tags) and **props**
  (extra facts) you can attach to any entity without declaring them.
  Queries read metadata with `::`, as in `p::created` or
  `p::labels has "vip"`.

Fields have IDs too, and can carry a short documentation note saying
what they are for.

## Kinds

A **kind** describes a type of entity: which fields it has, what type
each holds, and the rules they follow. It is like a form with labelled
boxes.

```rhizome
define kind Product "Something we sell" {
  name  : text
  sku   : text unique
  price : decimal(10,2) check price >= 0
  tags  : set<text> default {}
};
```

Kinds protect your data. A misspelled field, a missing required field,
or text where a number belongs is refused with a clear error, before
anything is written.

Kinds are flexible where it helps:

- **Optional fields** are marked with `?` (`text?`) and may be left out.
- **Extra fields on one entity.** By default a single entity can carry
  fields its kind doesn't declare, added on purpose with `add`. This
  suits data where most items look alike but some have extras. A kind
  marked `closed` forbids that.
- **No kind at all.** An entity may have no kind, for loose notes or data
  whose shape you don't know yet.
- **Changing kinds later** with `alter kind` (adding or dropping a field,
  changing a type) is normal. PolyDB checks and converts existing data
  for you.

## Values and types

Every value has exactly one type, and PolyDB never quietly changes it:
an `int` field never ends up holding a fraction. The types cover:

| Group | Types |
|---|---|
| Simple values | `bool`, `int`, `bigint` (any size), `float`, `decimal` (exact, for money), `text`, `bytes`, `time`, `date`, `duration` |
| Identity | `id`, and `ref<Kind>`, a pointer to another entity stored in a field |
| Media | `image` (PNG, JPEG, GIF, WebP), and `bytes` with a media type, for any file |
| Structured text | `doc<json>`, `doc<xml>`, `doc<csv>`: text kept as written, but readable inside, so a query can look at `config->server->port` |
| Collections | `list<T>`, `set<T>`, `map<K, V>`, `array<T, N>`, `pair<A, B>`, `record{…}` |
| Queues | `queue.fifo<T>`, `queue.lifo<T>`, `queue.ring<T, N>`, `queue.priority<T>`: push and pop work items |
| Anything | `any` |

Text is always Unicode. A type followed by `?` may also be `null`,
which means "no value".

Large values (long text, files, images) are stored separately from the
rest of the entity and streamed in and out in pieces, so a 2 GB video
doesn't need 2 GB of memory. To you, they behave like any other value.

## Relations

A **relation** links one entity to another (or to a field of one), and
has a **type name** saying what the link means: `works_on`, `placed_by`,
`knows`. A relation has a direction, its own ID, and may carry
**properties**, such as when it started.

```rhizome
define kind Person { name: text };
define kind Company { name: text };
create Person @ana { name: "Ana" };
create Company @acme { name: "Acme" };
relate @ana ~works_at{since: 2021}~> @acme;
```

Relations are what make PolyDB a **graph database**. In a query you can
*follow* them, even over several steps ("friends of friends"), which is
slow and awkward when links are kept as plain values in tables.

You can relate anything without setup, or describe a relation type
first (`define relation`) to say what it may connect, add rules like
"at most one", and choose what happens when an entity at either end is
deleted.

> **A relation or a ref?** Both connect two entities. A **ref** is just
> a value in a field, like a foreign key: simple, and right when the
> link is a property of one side (an order's customer). A **relation**
> is a first-class link: it has its own properties, is indexed in both
> directions, and is what graph patterns follow. When in doubt, use a
> relation.

## Queries

A **query** starts with `find` and passes rows through a **pipeline**
of steps, each beginning with `|`:

```rhizome
find Person p
  | follow p ~works_at~> Company c
  | where c.name = "Acme"
  | show p.name;
```

Read it top to bottom: take every Person (call each `p`), follow their
`works_at` links to a Company `c`, keep the rows where the company is
Acme, show the person's name. Each step does one thing, so long queries
stay readable.

The same language also defines kinds, changes data, manages users and
inspects the database. See [A tour of Rhizome](rhizome-tour.md).

## Indexes

An **index** is a lookup table PolyDB keeps up to date so it can find
entities by a field's value without reading them all, like the index
at the back of a book. Queries give the same results with or without
one; indexes only make them faster.

```rhizome
define index product_price on Product(price);
define text index product_name on Product(name);
```

`unique` fields get an index automatically. A **text index** makes word
search (`search(p.name, "blue shirt")`) fast. `explain` before a query
shows whether it will use an index.

## Transactions

A **transaction** groups changes so they all happen or none do.
Moving money from one account to another must never do only half:

```rhizome norun
begin;
update @from set balance -= 100.00d;
update @to   set balance += 100.00d;
commit;
```

Every statement outside `begin … commit` is its own small transaction.
Inside one, you see the database as it was when the transaction began,
plus your own changes. If two transactions change the same data at
once, the second to commit fails with a `conflict` error and can simply
be run again.

## History

Fields can keep their **history**: old values with the time they
changed. Turn it on for a field or a whole kind, optionally with how
long to keep it, then ask what a value was, or query the database as
it was at a past moment:

```rhizome
define kind Account history { owner: text, balance: decimal(12,2) };
```

## Users and permissions

A server has **users**, who log in with a password or a key, and
**roles**, named groups of permissions you give to users. Permissions
(`read`, `write`, `relate`, `define`, `admin`) are granted on the whole
server, on one database, or on one kind. A user sees nothing they
haven't been granted. See [Running PolyDB](operations.md#users-and-permissions).

## If you know SQL

| SQL | PolyDB | Notes |
|---|---|---|
| database | database | the same idea |
| table | kind | but entities without a kind, or with extra fields, are allowed |
| row | entity | with its own permanent ID |
| column | field | fields have IDs and documentation too |
| primary key | ID | every entity has one; you don't choose it |
| foreign key | `ref<Kind>` field, or a relation | relations are usually better: indexed both ways, with properties |
| join table (many-to-many) | relation | no extra table: relate the two entities directly |
| `JOIN … ON` | `follow` / `match` | follow links instead of matching columns |
| recursive CTE | multi-hop pattern | `p ~reports_to*1..5~> boss` |
| `SELECT a, b` | `show a, b` | the last step, not the first |
| `WHERE` | `where` | can appear anywhere in the pipeline |
| `GROUP BY … HAVING` | `group by … as x`, `having` | |
| `ORDER BY`, `LIMIT`, `OFFSET` | `sort`, `limit`, `skip` | |
| `INSERT` / `UPDATE` / `DELETE` | `create` / `update` / `delete` | `update` and `delete` can also end a query |
| `CREATE TABLE` / `ALTER TABLE` | `define kind` / `alter kind` | large alters run in the background |
| `NULL` | `null` | but `null = null` is true, and a missing field is different from a null one |
| JSON column | `doc<json>` | with paths, typed by a format you can define |
| BLOB | `bytes`, `image`, long `text` | streamed, never loaded whole |
| `GRANT` / `REVOKE` | `grant` / `revoke` | the same idea, at server, database or kind level |

## Where next

- [Getting started](getting-started.md), if you haven't done it: these
  ideas in a hands-on session.
- [A tour of Rhizome](rhizome-tour.md): the language step by step.
- [Glossary](glossary.md): every term in one place.
