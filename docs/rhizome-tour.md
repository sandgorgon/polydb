# A tour of Rhizome

Rhizome is PolyDB's language. One language does everything: describing
data, adding and changing it, asking questions, following links,
managing users and looking inside the database.

This tour goes from simple to advanced. Every example works: you can
paste them in order into `polydb --memory` and follow along. Each
section ends with a link to the exact rules in the
[language reference](../spec/rhizome.md), for when you need them.

The examples use a small company: people, the companies they work for,
and who knows whom.

## Statements

- A statement ends with `;`. In the shell, pressing Enter on a complete
  statement adds it for you.
- Keywords are lowercase: `find`, not `FIND`. Names are case-sensitive:
  `Person` and `person` are different.
- Comments are `// to the end of the line` or `/* … */`.
- Names that clash with a keyword can be written in backticks:
  `` `order` ``, `` `first name` ``.

## Values

| Type | Examples |
|---|---|
| numbers | `42`, `-7`, `1_000_000`, `3.14`, `19.99d` (a decimal, exact), `123456789012345678901234567890n` (a bigint) |
| text | `"hello"`, `"line one\nline two"`, `` r`C:\raw\text` `` |
| true, false, nothing | `true`, `false`, `null` |
| time | `t"2026-11-21T14:30:00Z"`, `d"2026-11-21"` (a date), `90m`, `1h30m` (durations) |
| bytes | `x"deadbeef"`, `b64"3q2+7w=="` |
| collections | `[1, 2, 3]` (list), `{"a", "b"}` (set), `{name: "Ana", age: 31}` (map) |

`2` is an int and `2.0` is a float. PolyDB doesn't mix them silently:
`2 + 2.0` is an error that suggests `cast(2 as float)`.

## A database to work in

A server holds many databases. Make one for the tour and switch to it:

```rhizome
create database company;
use company;
```

## Defining kinds

```rhizome
define kind Company {
  name    : text unique
  founded : int?
};

define kind Person "A human being" {
  name   : text                 "Full name"
  email  : text unique
  age    : int? check age >= 0
  tags   : set<text> default {}
  joined : date default today()
  boss   : ref<Person>?
};
```

Each field is `name : type`, then any options:

| Option | Meaning |
|---|---|
| `?` after the type | optional: may be left out or `null` |
| `"text"` | a documentation note |
| `unique` | no two entities may share the value |
| `default <value>` | used when `create` doesn't give one |
| `check <condition>` | must hold on every write |
| `history` | keep old values ([History](#history)) |

Kinds can change later:

```rhizome
alter kind Person add phone : text? "Work phone";
alter kind Person drop phone;
```

([Reference: definitions](../spec/rhizome.md#5-definitions))

## Creating data

```rhizome
create Company @acme   { name: "Acme", founded: 1999 };
create Company @globex { name: "Globex" };

create Person @ana  { name: "Ana Lima",   email: "ana@x.io",  age: 34, tags: {"eng", "lead"} };
create Person @ben  { name: "Ben Okafor", email: "ben@x.io",  age: 27, tags: {"eng"}, boss: @ana };
create Person @cleo { name: "Cleo Park",  email: "cleo@x.io", age: 41, tags: {"sales"} };
create Person @dan  { name: "Dan Reyes",  email: "dan@x.io",  tags: {"eng"}, boss: @ana };
```

`@ana` is a **handle**: a name for the new entity that lasts for your
session. Dan has no `age`, which is allowed because it's `int?`.

A kind only accepts the fields it declares, which catches typos:

```rhizome error
create Person { name: "Eve", emial: "eve@x.io" };
```

```text
error: unknown_field: kind Person has no field emial (did you mean email?)
```

To give one entity a field its kind doesn't have, `add` it on purpose:

```rhizome
update @cleo add territory : text = "North" doc "Region she covers";
```

Entities don't need a kind at all:

```rhizome
create @note { title: "Offsite", place: "Lisbon" };
```

## Changing and deleting data

```rhizome
update @ben set age = 28;
update @ben set age += 1;                 // arithmetic on numbers
update @ben set tags ++= {"oncall"};      // add to a set
update @ben set tags --= {"oncall"};      // remove from a set
update @ana set name ++= " Silva";        // append text
update @dan unset boss;                   // clear an optional field
update @note set title = "Team offsite";
```

`set` only changes fields that exist, so a typo is an error rather than
a new field. Delete with `delete`:

```rhizome
delete @note;
```

([Reference: data statements](../spec/rhizome.md#6-data-statements))

## Queries

A query starts with `find`, which says where rows come from, then
passes them through steps joined by `|`:

```rhizome
find Person p
  | where p.age >= 30
  | sort p.age desc
  | show p.name, p.age;
```

The steps you'll use most:

| Step | What it does | Example |
|---|---|---|
| `where` | keeps rows where the condition is true | `\| where "eng" in p.tags` |
| `show` | chooses the output columns (`as` renames) | `\| show p.name as who` |
| `sort` | orders rows, `asc` (default) or `desc` | `\| sort p.age desc, p.name` |
| `limit`, `skip` | pages through results | `\| skip 20 \| limit 10` |
| `distinct` | removes duplicates | `\| distinct p.age` |
| `let` | computes a value for later steps | `\| let years = 2026 - p.age` |

Steps can come in any order and repeat. Without `show`, the result is
every variable, as whole entities.

```rhizome
find Person p
  | let initials = substr(p.name, 0, 1)
  | where p.tags has "eng"
  | sort initials
  | show initials, p.name;
```

`find` can also start from particular entities:

```rhizome
find @ana as p | show p.email;
find Person p in [@ana, @cleo] | show p.name;
```

### Conditions and expressions

| Kind | Operators |
|---|---|
| compare | `=`, `!=`, `<`, `<=`, `>`, `>=`, `between 18 and 65` |
| text | `starts with`, `ends with`, `contains`, `matches "regex"` |
| collections | `x in list`, `set has x` |
| types | `x is null`, `x is int`, `p is Person` |
| logic | `and`, `or`, `not` |
| arithmetic | `+ - * / %` for numbers; `++` joins text and lists |

```rhizome
find Person p
  | where p.name starts with "C" or (p.age is null and p.tags has "eng")
  | show p.name;
```

About `null`: a missing value. Comparing it with `<` or `>` is false,
`where` treats an unknown result as false, and `coalesce(p.age, 0)`
gives a fallback. Unlike SQL, `null = null` is true.

Some useful functions:

| Group | Functions |
|---|---|
| text | `len`, `lower`, `upper`, `trim`, `substr`, `split`, `join`, `replace` |
| numbers | `abs`, `round(x, digits)`, `floor`, `ceil`, `min`, `max`, `sqrt`, `pow` |
| time | `now()`, `today()`, `year(t)`, `month(t)`, `day(t)`, `add_months(d, n)` |
| collections | `len`, `keys`, `values`, `first`, `last`, `slice`, `sort` |
| general | `coalesce`, `if(cond, a, b)`, `cast(x as T)`, `typeof`, `exists` |

```rhizome
find Person p | show upper(p.name) as name, coalesce(p.age, 0) as age, len(p.tags) as tag_count;
```

Fields of a referenced entity are read through the ref:

```rhizome
find Person p | where p.boss.name starts with "Ana" | show p.name;
```

([Reference: queries](../spec/rhizome.md#7-queries-pipelines),
[expressions](../spec/rhizome.md#8-expressions))

## Relations and the graph

`relate` links two entities. The arrow says which way the link points,
and properties go in braces:

```rhizome
relate @ana  ~works_at{since: 2019}~> @acme;
relate @ben  ~works_at{since: 2023}~> @acme;
relate @dan  ~works_at{since: 2024}~> @acme;
relate @cleo ~works_at{since: 2015}~> @globex;

relate @ana ~knows~> @ben;
relate @ben ~knows~> @cleo;
relate @cleo ~knows~> @dan;
```

Follow relations with `follow`:

```rhizome
find Person p
  | follow p ~works_at r~> Company c
  | sort r.since
  | show p.name, c.name, r.since;
```

Patterns can point either way, match several types, and cross several
steps:

| Pattern | Means |
|---|---|
| `p ~knows~> f` | `p` knows `f` |
| `p <~knows~ f` | `f` knows `p` |
| `p ~knows~ f` | either way |
| `p ~knows\|likes~> f` | either type |
| `p ~*~> x` | any type |
| `p ~knows r~> f` | also names the relation `r`, to read its properties |
| `p ~knows*1..3~> f` | one to three steps away |
| `p ~knows*~> f` | any number of steps (up to the `max_depth` setting, 10) |

Who can Ana reach through people she knows, and how far away are they?

```rhizome
find @ana as p
  | follow path route = p ~knows*1..3~> Person f
  | show f.name, len(route) as steps;
```

`follow` adds new variables. To check a shape between variables you
already have, use `match`. Colleagues of Ana's (people at the same
company):

```rhizome
find @ana as p
  | follow p ~works_at~> Company c
  | match c <~works_at~ Person colleague
  | where colleague != p
  | show colleague.name;
```

`unrelate` removes links:

```rhizome
unrelate @cleo ~knows~> @dan;
```

Describing a relation type is optional, but adds rules:

```rhizome
define relation mentors from Person to Person "Career mentoring" max in 1 {
  since : date?
};
relate @ana ~mentors~> @ben;
```

`max in 1` means a person can have at most one mentor, so a second is
refused:

```rhizome error
relate @cleo ~mentors~> @ben;
```

([Reference: graph patterns](../spec/rhizome.md#73-graph-patterns),
[relation types](../spec/rhizome.md#52-relation-types-optional))

## Metadata

Besides its fields, every entity carries **metadata**: facts PolyDB
keeps about it. Read them with `::`, and use them anywhere a value goes:
in `where`, `sort` and `show`.

| Metadata | What it is | Can you change it? |
|---|---|---|
| `p::id` | the entity's ID | no |
| `p::kind` | its kind's name | no |
| `p::created`, `p::updated` | when it was created and last changed | no (set by PolyDB) |
| `p::version` | how many times it has been written | no |
| `p::owner` | the user who created it | no |
| `p::labels` | a set of tags of your own | yes |
| `p::props` | a map of your own extra facts, read as `p::props.source` | yes |

Labels and props are for facts *about* the data rather than part of it:
where it came from, a review status, a flag for cleanup. They exist on
every entity, whatever its kind, so they don't need to be declared:

```rhizome
update @ana set ::labels ++= {"vip"};
update @ana set ::props ++= {source: "import", checked: true};

find Person p
  | where p::labels has "vip"
  | show p.name, p::created, p::version, p::props.source;

find Person p | sort p::updated desc | limit 3 | show p.name, p::updated;
```

Fields and relations have metadata too: `p.name::doc` (the field's
note), `p.name::type`, `p.name::id`, `p.name::declared` (false for a
field this entity added itself), and for a relation `r`, `r::type`,
`r::id`, `r::from` and `r::to`:

```rhizome
find Person p | follow p ~works_at r~> Company c | show p.name, r::type, r::id;
```

Metadata can't be indexed, so a query that filters on it reads every
entity of the kind. If you filter on something often, make it a field.

### The database's own metadata

A database has metadata too: its `::name`, when it was created and by
whom (`::created`, `::owner`), and a `doc` note, `::labels` and
`::props` that its administrators can set with `alter database`:

```rhizome
alter database company set doc = "The tour's example company",
  ::labels ++= {"example"},
  ::props.schema_version = 1;

describe database company;
```

Queries read it as `database::…`. A common use is recording which
version of your application's data layout a database has, so the
application knows which upgrade steps to run:

```rhizome
alter database company set ::props.schema_version = 2;

find Person p
  | where database::props.schema_version >= 2
  | limit 1
  | show database::name, database::props.schema_version;
```

`show databases` lists every database with its owner, creation time,
labels and doc. Backups keep a database's metadata, and exports carry
its doc, labels and props.

## Grouping and counting

Aggregates summarize many rows: `count()`, `sum`, `avg`, `min`, `max`,
`collect` (into a list) and `collect_set`. Over a whole query they
give one row:

```rhizome
find Person p | show count() as people, avg(p.age) as average_age;
```

With `group by`, one row per group; `having` filters groups:

```rhizome
find Person p
  | follow p ~works_at~> Company c
  | group by c.name as company
  | having count() >= 1
  | sort company
  | show company, count() as staff, collect(p.name) as names;
```

## Changing data with a query

A query can end with `update`, `delete`, `unrelate`, `push` or `pop`,
which then applies to every row:

```rhizome
find Person p | where p.tags has "eng" | update p set tags ++= {"tech"};
```

This is the usual way to change many entities at once. Run it with
`explain` first to see what it would do, without doing it:

```rhizome
explain find Person p | where p.age > 100 | delete p;
```

## Documents inside fields

A `doc<json>` (or `doc<xml>`, `doc<csv>`) field keeps structured text
exactly as written, and queries can read inside it with `->`:

```rhizome
define kind Service { name: text, config: doc<json> };
create Service { name: "api", config: "{\"port\": 8080, \"hosts\": [\"a\", \"b\"]}" };

find Service s | show s.name, s.config->port, s.config->hosts->[0];
```

Define a **format** to give documents a shape, so paths are checked and
typed. Indexes can use paths too (`define index on Service(config->port)`).
([Reference: formats](../spec/rhizome.md#53-formats-derived))

## Searching text

`search` finds words in text, ignoring case and accents:

```rhizome
alter kind Person add bio : text?;
update @ana set bio = "Leads the platform team; likes climbing.";
update @cleo set bio = "Sales lead for the north region.";

find Person p | where search(p.bio, "lead*") | show p.name;
find Person p | where search(p.bio, "\"platform team\"") | show p.name;
```

A query can be words (all must appear), `"a phrase"`, `-word` (must not
appear), `prefix*`, or `a OR b`. A text index makes it fast on large
kinds:

```rhizome
define text index person_bio on Person(bio);
```

([Reference: full-text search](../spec/rhizome.md#87-full-text-search))

## Queues

A queue field holds work items in order. `push` adds, `pop` takes:

```rhizome
define kind Worker { name: text, jobs: queue.fifo<text> };
create Worker @w { name: "builder", jobs: [] };

push @w.jobs "compile";
push @w.jobs "test";
pop @w.jobs;
```

The `pop` returns `"compile"`. `pop @w.jobs wait 30s` waits for an item
to arrive instead of returning `null` at once, which lets a program
wait for work without polling. Priority queues take
`push … priority 5`. `peek(q)` and `len(q)` read a queue without
changing it.

## History

A field with `history` keeps its old values:

```rhizome
define kind Account history { owner: text, balance: decimal(12,2) };
create Account @acct { owner: "Ana", balance: 100.00d };
update @acct set balance = 80.00d;

history @acct.balance;
```

`find … as of t"2026-01-01T00:00:00Z"` reads the database as it was at
that moment. `history keep 90 * 24h` keeps old values for 90 days.

## Transactions

```rhizome
begin;
update @acct set balance -= 30.00d;
create Account { owner: "Ben", balance: 30.00d };
commit;
```

Everything between `begin` and `commit` happens together, or not at
all. `rollback` cancels. If anything fails inside a transaction, the
rest is refused until you `rollback`; the shell's prompt shows `!` to
remind you. If another session changed the same data meanwhile,
`commit` fails with `conflict`: run the transaction again.

## Parameters

Never build a query by pasting user input into its text. Write `$name`
for a value, and pass it separately:

```rhizome norun
find Person p | where p.email = $email | show p.name;
```

In the shell, `\set email "ana@x.io"` gives `$email` a value. From Go,
pass `polydb.Params{"email": e}`.

## Looking inside

```rhizome
show kinds;
show relations;
show indexes;
describe kind Person;
describe @ana;
show stats;
```

`explain` shows how PolyDB will run a query, including whether it uses
an index, and `explain analyze` runs it and shows real counts and
times:

```rhizome
define index person_age on Person(age);
explain find Person p | where p.age > 30 | show p.name;
```

## Mistakes and error messages

Every error has a **code** saying what kind of problem it is, a message,
and where it happened. PolyDB suggests a fix when it can:

```rhizome error
FIND Person p;
```

```text
error: syntax: keywords are lowercase: did you mean find?
```

The codes are listed in the [FAQ](faq.md#what-do-the-error-codes-mean).

## Where next

- [Using PolyDB from Go](embedding.md) and [connecting to a server from Go](client.md)
- [Running PolyDB](operations.md): users, backups and limits
- [The full language reference](../spec/rhizome.md)
