# Rhizome Language Specification

Status: **Agreed** (2026-11-22)
Related: [plan.md](../plan.md) section 3, [types.md](types.md)

Rhizome is PolyDB's language for defining, changing, querying and inspecting data. A single language covers schema, data, graph traversal, queues, transactions, security and introspection.

### Style decisions (agreed 2026-11-21)
- **Queries are pipelines.** Steps are joined with `|` and read top to bottom. A `match` step handles complex graph shapes.
- **Edges use tilde arrows:** `~type~>` outgoing, `<~type~` incoming, `~type~` either direction.
- **Keywords are lowercase only.** Names are case-sensitive.
- **Statements end with `;`.** Comments are `//` and `/* */`. The REPL adds the `;` itself when you press Enter on a complete statement.

---

## 1. Lexical structure

### 1.1 Source text
- Source is UTF-8 and is NFC-normalized before parsing (types.md §4.6). Invalid UTF-8 is a syntax error.
- Whitespace (space, tab, newline) separates tokens and has no other meaning.

### 1.2 Comments
```
// to end of line
/* block comment, can span lines; does not nest */
```

### 1.3 Names
- **Identifiers** (kinds, fields, relation types, variables, databases, users, roles, formats, indexes) follow types.md §14: a Unicode letter or `_` first, then letters, digits or `_`, up to 255 code points. They are case-sensitive: `Person` and `person` are different.
- A name that clashes with a keyword, or contains other characters, can be written in backticks: `` `order` ``, `` `first name` ``.
- **Conventions** (not enforced): kinds in `PascalCase`; fields, relations and variables in `snake_case`.

### 1.4 Special prefixes

| Prefix | Meaning | Example |
|---|---|---|
| `@` | session handle: a named reference kept for the current session only (decided: no persistent entity names in v1; `@@` is reserved for them later) | `@alice` |
| `#` | ID literal | `#01JD8X3K7M0000Q2W5ZB9N4T6R` |
| `$` | parameter supplied by the client | `$min_age` |
| `::` | metadata access | `p::created`, `p.name::doc` |

### 1.5 Keywords
Keywords are reserved and lowercase only:

```
alter and as asc begin between bind by cast commit create database default
define delete desc describe distinct doc drop exists explain false find follow
for format from grant group has having history if in index inf is keep kind
let limit match matches nan not null of on or password pop priority push read
relate relation restrict revoke role rollback set show skip sort to true union
unique unrelate unset update use user where with write
```

Reserved now for future features (inference rules, kind reuse and others), so they can't be used as bare names: `rule infer derive when then trigger view extends trait`.

**Contextual keywords** are special only in particular positions and can be used as names elsewhere. Examples: `add extensible closed cascade dangle max out path only analyze`, type names like `text` and `list`, and the `show` targets like `kinds` and `stats`. For example, `max` is an aggregate function in expressions but a limit in `max out 1`. The grammar (§15) marks every keyword: quoted words not in the list above are contextual.

### 1.6 Literals

| Type | Examples |
|---|---|
| bool, null | `true`, `false`, `null` |
| int | `42`, `-7`, `1_000_000`, `0x1F`, `0b1010` |
| bigint | `123456789012345678901234567890n` |
| float | `3.14`, `1e-9`, `2.0`, `nan`, `inf`, `-inf` |
| decimal | `19.99d`, `0.10d` |
| text | `"hello\n"`. Escapes: `\n \t \r \\ \" \u{1F600}` |
| raw text | `` r`C:\no\escapes` ``, can span lines |
| bytes | `x"deadbeef"` (hex), `b64"3q2+7w=="` (base64) |
| time | `t"2026-11-21T14:30:00Z"` |
| date | `d"2026-11-21"` |
| duration | `250ms`, `1h30m`, `-5s` |
| id | `#01JD8X3K7M0000Q2W5ZB9N4T6R` |
| list | `[1, 2, 3]` |
| set | `{"eng", "ops"}` (no colons) |
| map / record | `{name: "Ann", age: 3}`, `{"key with space": 1, 42: "x"}` |
| pair | `"k" => 1` |

- An **unquoted key** in `{k: v}` is a text key; a quoted or computed key is an expression.
- `{}` with no target type is an empty map. Assigned to a `set<T>` field, it is an empty set.
- Arrays, records and queues are written with list or map literals and checked against the field's declared type.
- Because there is no implicit `int` ↔ `float` conversion (types.md §10), `2` is an int and `2.0` is a float.

---

## 2. Model refresher

- A **server** hosts many **databases**. A session works in one database at a time (`use`).
- A database holds **entities** with **fields**, connected by **relations**.
- Relations go from an entity or field to an entity or field. They have a type name, an ID and optional properties.
- **Kinds** (entity schemas), **relation types**, **formats** and **indexes** are optional definitions in the database catalog.

### 2.1 Addressing things

| Expression | Refers to |
|---|---|
| `p` | a variable bound to an entity, field or relation in a pipeline |
| `@alice` | a session handle |
| `#01JD...` | anything by ID |
| `p.name` | the value of field `name` |
| `p.name::id`, `p.name::doc`, `p.name::type`, `p.name::declared` | metadata of the field (`declared` is false for fields this entity added itself) |
| `p::id`, `p::kind`, `p::created`, `p::updated`, `p::version`, `p::labels`, `p::owner` | entity metadata |
| `p::props.source` | a custom metadata property |
| `r.since`, `r::type`, `r::from`, `r::to` | relation property and metadata |

`p.name` gives the field's *value*. To use the field itself as a graph node, for example to relate it, write the field path in a relation position (`relate p.address ~located_in~> c`). The parser knows from context that a node is meant.

---

## 3. Statements overview

| Group | Statements |
|---|---|
| Databases and session | `create database`, `drop database`, `use`, `set`, `show databases` |
| Definitions | `define kind / relation / format / index`, `alter …`, `drop …` |
| Data | `create`, `update`, `delete`, `relate`, `unrelate`, `push`, `pop` |
| Queries | pipelines starting with `find` |
| Transactions | `begin`, `commit`, `rollback` |
| Security | `create user / role`, `alter user`, `drop user / role`, `grant`, `revoke` |
| Introspection | `describe`, `explain`, `show …`, `history` |

Each statement ends with `;`.

---

## 4. Databases and session

```
create database sales;
use sales;
drop database sales;             // needs admin; asks for confirmation in the CLI
show databases;

set timeout = 30s;               // session settings
set max_rows = 10000;
```

Queries can't span databases in v1.

---

## 5. Definitions

### 5.1 Kinds

```
define kind Person "A human being" {
  name     : text                         "Full name"
  email    : text unique                  "Login email"
  age      : int? check age >= 0
  joined   : date default today()
  photo    : image?
  tags     : set<text> default {}
  manager  : ref<Person>? on delete restrict
  inbox    : queue.priority<text>
  salary   : decimal(12,2)? history keep 365d
}
```

**Field rules:**
- A field whose type isn't `?` is **required**: it must be given on `create` unless it has a `default`. A `?` field may be absent or null.
- **Field options**, in any order after the type:

  | Option | Meaning |
  |---|---|
  | `"doc string"` | documentation |
  | `unique` | no two entities of this kind may have equal values (creates an index automatically) |
  | `default <expr>` | value used when not given on create |
  | `check <expr>` | must be true on every write; the field's own name refers to the new value |
  | `on delete dangle\|restrict` | for refs (types.md §4.12) |
  | `history [keep <duration>]` | keep old versions of this field (opt-in history) |

**Kind options:**
```
define kind Invoice closed { ... }       // entities may not add their own fields
define kind Person history { ... }       // history on for every field of the kind
```

**Kind modes** (decided):

| Mode | Declared fields | Extra fields added to one entity with `add` (§6.2) |
|---|---|---|
| **extensible** (default) | enforced | allowed |
| `closed` | enforced | not allowed |
| no kind (kindless entity) | none | allowed |

- In every mode, **`set` only changes fields that already exist**. Writing to an unknown field is an error, which catches typos like `emial`, and the message suggests the closest existing field. New fields are always created on purpose with `add`.
- Fields added to one entity are full fields, like declared ones: they have their own ID, type, doc, and can take part in relations. They exist only on that entity, not on others of the same kind.
- `alter kind Person set closed;` and `alter kind Person set extensible;` switch modes. Switching to closed fails if existing entities have added fields; the error reports how many.
- `alter kind Person add …` adds a declared field for *every* entity of the kind. If some entities already added a field of that name, the alter fails unless the types match. When they match, those fields become declared fields.
- Kinds have no inheritance in v1 (decided). Each entity has exactly one kind. `extends` and `trait` are reserved for possible field reuse later.

**Changing and removing kinds:**
```
alter kind Person add nickname : text? "Informal name";
alter kind Person drop nickname;
alter kind Person alter age : int? check age between 0 and 150;
alter kind Person rename to Human;
drop kind Person;                // fails while entities of this kind exist
```
An `alter` that existing data doesn't satisfy (for example adding a required field without a default) fails, with a count of the entities that don't fit.

### 5.2 Relation types (optional)

Relations can be created with any type name, no definition needed. Defining a relation type adds rules and docs:

```
define relation knows from Person to Person "Personal acquaintance" {
  since : int?
}
define relation located_in from any to City;
```
- `from` and `to` take a kind name, `any`, or `field` (meaning the relation must start or end on a field rather than an entity).
- Properties use the same field syntax and options as kinds.

**Relation options** (after the `from … to …` part):

```
define relation spouse    from Person to Person max out 1 max in 1;
define relation placed_by from Order  to Customer max out 1 on delete restrict;
```

| Option | Meaning |
|---|---|
| `"doc string"` | documentation |
| `max out N` | a node may have at most N outgoing relations of this type (decided: maximums only in v1) |
| `max in N` | a node may have at most N incoming relations of this type |
| `on delete cascade` (default) | deleting an entity at either end also deletes these relations |
| `on delete restrict` | deleting an entity at either end fails while these relations exist |

- `relate` fails with a `constraint` error if it would exceed a maximum.
- Minimums ("every Order must have a customer") aren't in v1. They may be added later as checks at commit time.
- Relation types that aren't defined use `cascade` and have no maximums.
- `alter relation` works like `alter kind`: `add`, `drop` or `alter` properties, `rename to`, or `set` an option (for example `alter relation spouse set max out 2;`). Setting a lower maximum fails if existing relations exceed it.
- In definitions, a doc string can be written bare (`"Full name"`) or with the keyword (`doc "Full name"`). In value positions (`create`, `add`), the `doc` keyword is required, so a doc can't be mistaken for a value.

### 5.3 Formats (derived)

Base formats (json, xml, csv, tsv) are built in; others are added in Go (types.md §6.1). Derived formats are defined here:

```
define format invoice : json "Customer invoice" {
  number : text
  total  : decimal(12,2)
  lines  : list<record{ sku: text, qty: int }>
}
```

### 5.4 Indexes

```
define index person_age on Person(age);
define index order_lookup on Order(customer, placed);     // multi-field
define unique index sku on Product(sku);
define text index person_bio on Person(bio);              // full-text
define index cfg_port on Server(config->server->port);    // inside a doc
drop index person_age;
```
- Indexes are built in the background. `show indexes` reports their progress.
- Relation adjacency (out and in, by type) is always indexed and doesn't need defining.

---

## 6. Data statements

### 6.1 Create

```
create Person @alice { name: "Alice", email: "a@x.io", age: 34, tags: {"eng"} };
create Person { name: "Bob", email: "b@x.io" };
create @n { title: "Loose note", body: "no kind" };   // kindless entity
create @m { title: "Standup",
            room: "B2" doc "Building and room" };     // kindless, field with doc
create Person @carol { name: "Carol", email: "c@x.io" }
  add hobby : text = "chess" doc "Main hobby";        // kinded entity plus its own extra field
```
- `@handle` after the kind is optional. It binds the new entity to a session handle.
- **With a kind**, the `{ … }` part may only contain declared fields. `create Person { name: "Carol", emial: "c@x.io" }` fails (did you mean `email`?). Extra fields go in `add` clauses, which follow the same rules as `update … add` (§6.2) and are only allowed on extensible kinds.
- **Without a kind**, every field in `{ … }` is new and has type `any`. Use an `add` clause instead to give one a specific type.
- `doc "..."` after a value sets the field's documentation. Declared fields take their doc from the kind.
- `create` returns the new entity's ID.

### 6.2 Update

```
update @alice set age = 35, email = "alice@x.io";
update @alice unset age;                             // age is optional (int?)
update @alice set age += 1;                          // numeric
update @alice set tags ++= {"ops"}, tags --= {"eng"}; // set add and remove
update @alice set name ++= " Jr.";                   // text append
update @alice set ::labels ++= {"vip"};              // metadata
update @n set title = "Renamed note";               // existing field on a kindless entity
update @n set titel = "x";                           // ERROR: no field 'titel' (did you mean 'title'?)

// Adding and removing fields on one entity
update @alice add hobby : text = "chess" doc "Main hobby";
update @n add pinned : bool = true, scores : list<int> = [1, 2];
update @n add attrs = {k: "v"};                      // type omitted: the field is 'any'
update @n set scores[0] = 10, attrs["k"] = "w";      // list index, map key
update @alice drop hobby;
update #01JD8X3K7M0000Q2W5ZB9N4T6R set age = 36;
```
- `+=`, `-=`, `*=` and `/=` are **numeric only**.
- `++=` appends text, bytes or lists, adds to sets and merges into maps. The right side is always a value of the same type (a set to add to a set, a list to append to a list).
- `--=` removes from lists (every equal element), sets (members) and maps (by key; the right side is a set of keys).

**Statement forms:**
- **`set`** changes the value of a field that already exists, either declared by the kind or added earlier. It never creates fields.
- **`add name [: type] = value [options]`** creates a new field on this entity only:
  - It fails if a field of that name already exists, or if the entity's kind is `closed`.
  - If the type is omitted, the field's type is `any`.
  - Options are the same as for kind fields (§5.1): a doc string, `check`, `history`, and `on delete` for refs. `unique` and `default` aren't allowed, because they only make sense across a kind.
- **`drop name`** removes a field this entity added. Declared fields can't be dropped per entity. Use `unset` for optional ones, or `alter kind … drop`.
- **`unset name`** removes the value of an optional declared field, or of an added field, so the field reads as absent.
- `describe` shows which fields come from the kind and which the entity added. In queries, `p.hobby::declared` is `true` for declared fields and `false` for added ones.
- Adding or dropping fields needs `write` permission on the entity's kind.

### 6.3 Delete

```
delete @alice;
```
- What happens to relations is set **per relation type** (decided, §5.2):
  - By default (`cascade`), deleting an entity also deletes relations of that type touching it or its fields.
  - If any relation touching the entity or its fields has a type marked `on delete restrict`, the delete fails and the error lists them.
  - Either way, relations never dangle.
- The delete also fails if a `restrict` ref points to the entity (types.md §4.12).
- `explain delete @alice;` shows what a delete would remove or what would block it, without running it.

### 6.4 Relate and unrelate

```
relate @alice ~knows{since: 2019}~> @bob;
relate @alice ~knows~> @bob as @k;                 // handle to the relation itself
relate @alice.address ~located_in~> @springfield;  // from a field
unrelate @alice ~knows~> @bob;                     // all 'knows' relations between them
unrelate @k;                                       // one specific relation
```
- Several relations of the same type between the same two nodes are allowed. Each has its own ID.

### 6.5 Queue operations

```
push @alice.inbox "hello" priority 5;
push @worker.jobs @task1;                          // fifo, lifo, ring: no priority
pop @alice.inbox;                                  // returns the value, or null if empty
pop 10 from @alice.inbox;                          // up to 10 values
pop @worker.jobs wait 30s;                         // waits up to 30s for an item, then null
```
- **`wait <duration>`** makes `pop` block until an item arrives or the time runs out (decided 2026-11-22). This lets workers wait for jobs without polling.
  - Waiters on the same queue are served in the order they started waiting.
  - Not allowed inside a transaction.
  - The maximum wait is a server setting (default 15 minutes).
  - The wait can be cancelled by the client (protocol.md §12).
`peek`, `len` and reading in pop order are functions (§8.4), so they can be used in queries without changing the queue.

### 6.6 Data changes inside pipelines

Any pipeline can end with a data step, which applies to every row:

```
find Person p | where p.age > 120 | update p set ::labels ++= {"check_age"};
find Person p | where p::labels has "spam" | delete p;
find Person p | follow p ~knows r~> Person f | where r.since < 1990 | unrelate r;
find Worker w | where w.idle | pop w.jobs as job | show w.name, job;
```

---

## 7. Queries (pipelines)

A query starts with `find` and continues with steps, each beginning with `|`. Every step takes a stream of **rows** (named variables) and produces a new stream.

### 7.1 `find`: the starting point

```
find Person p                  // every Person entity
find p                         // every entity, any kind (full scan; explain warns)
find Person p, Company c       // all pairs (explain warns)
find @alice as p               // start from a handle
find #01JD... as x             // start from an ID
find Person p in [@alice, @bob]
```

### 7.2 Steps

| Step | Purpose | Example |
|---|---|---|
| `where <cond>` | keep rows where the condition is true | `\| where p.age > 30 and "eng" in p.tags` |
| `follow <pattern>` | move along relations, adding a variable | `\| follow p ~knows~> Person f` |
| `match <pattern>` | require a graph shape (joins on shared variables) | `\| match p ~works_at~> Company c <~works_at~ f` |
| `let x = <expr>` | compute a new variable | `\| let full = p.first ++ " " ++ p.last` |
| `group by <expr> as x` | group rows; later steps can use aggregates | `\| group by p.city as city` |
| `having <cond>` | filter groups | `\| having count() > 5` |
| `sort <expr> [asc\|desc], …` | order rows | `\| sort p.age desc, p.name` |
| `distinct [<expr>, …]` | remove duplicate rows | `\| distinct f` |
| `skip n` / `limit n` | paging | `\| skip 20 \| limit 10` |
| `show <expr> [as name], …` | choose output columns | `\| show p.name, f.name as friend` |
| `bind <var> as @handle` | save exactly one result to a session handle | `\| bind p as @alice` |
| `union ( <pipeline> )` | add rows from another pipeline | `\| union (find Bot b \| show b.name)` |
| `update`, `delete`, `unrelate`, `pop`, `push` | data changes (§6.6) | |

If there is no `show`, the output is every bound variable. Entities are shown with their ID, kind and all fields.

### 7.3 Graph patterns

```
p ~knows~> Person f              // outgoing 'knows' to a Person
p <~manages~ Person boss         // incoming 'manages'
p ~knows~ f                      // either direction
p ~knows|likes~> f               // either relation type
p ~*~> x                         // any relation type
p ~knows r~> f                   // bind the relation as r (r.since, r::id)
p ~knows{since: 2019}~> f        // relation property must equal this value
p ~knows*1..3~> Person fof       // 1 to 3 hops
p ~knows*~> fof                  // 1 or more hops (capped by session setting max_depth, default 10)
p.address ~located_in~> City c   // starting from a field
p ~mentions~> x.title            // ending at a field
```
- The kind after an arrow is optional. Leaving it out matches any kind.
- **Multi-hop paths never visit the same entity twice** (no cycles).
- `follow` and `match` can bind the whole path: `| follow path pth = p ~knows*1..3~> f`, then `len(pth)` and `nodes(pth)`.
- **`follow` vs `match`:** `follow` can only add new variables at the end of a pattern. `match` can connect variables that are already bound and so check whole shapes (triangles, shared neighbours).

### 7.4 A complete example

```
// Engineers over 30 and their friends-of-friends at the same company
find Person p
  | where p.age > 30 and "eng" in p.tags
  | follow p ~works_at~> Company c
  | follow p ~knows*2~> Person fof
  | match fof ~works_at~> c
  | distinct p, fof
  | sort p.name
  | show p.name, fof.name as colleague, c.name as company
  | limit 50;
```

---

## 8. Expressions

### 8.1 Operators, highest precedence first

| Level | Operators |
|---|---|
| 1 | `.` field, `::` metadata, `[ ]` index or key, `->` inside a doc, `( )` call |
| 2 | unary `-` |
| 3 | `*` `/` `%` |
| 4 | `+` `-` (numbers only), `++` (join text, bytes or lists; union of sets; merge maps), `--` (remove from lists, sets or maps) |
| 5 | `=` `!=` `<` `<=` `>` `>=`, `in`, `not in`, `has`, `is`, `between … and …`, `matches`, `starts with`, `ends with`, `contains` (these don't chain: `a < b < c` is a syntax error) |
| 6 | `not` (so `not a = b` means `not (a = b)`) |
| 7 | `and` |
| 8 | `or` |
| 9 | `=>` (builds a pair) |

- `x in coll` / `coll has x` test membership. `has` is the same test written the other way round, which reads better for sets: `p.tags has "eng"`.
- `x is int`, `x is null`, `x is Person` (kind check) test types.
- `matches` uses Go regular expressions (RE2 syntax): `p.email matches ".*@x\\.io$"`.
- **Inside documents:** `p.config->server->port`, `p.config->"key with space"`, `p.items->[0]`. Each format defines what the steps mean, and formats with a shape give typed results (§8.6).

### 8.2 Typing rules
- Arithmetic follows types.md §10.3. `int` with `float` is an error until one side is cast: `cast(p.age as float) * 1.5`.
- Each operator means one thing (decided): `+` always adds numbers, `++` always joins. `"a" + "b"` and `1 ++ 2` are type errors, and the message suggests the other operator. With `any` values, the same rule is checked at run time.
- Comparisons between different numeric types are exact and need no cast (types.md §11.1).
- `<`, `>` and friends between different type groups (for example text with int) are a type error. Where possible it's reported before the query runs, otherwise at run time on the first row.

### 8.3 Null rules
- `null = null` is true; `null != 1` is true.
- `<`, `>`, `<=`, `>=` with a null side are false.
- Arithmetic and functions with a null argument return null, except `coalesce`, `exists`, `typeof` and `is`.
- `where` treats null as false.
- **Absent vs null:** reading a field that doesn't exist gives null. `exists(p.age)` is false when the field is absent and true when it's present, even if it holds null.

### 8.4 Built-in functions (core set; full list in the reference appendix later)

| Group | Functions |
|---|---|
| General | `typeof(x)`, `cast(x as T)`, `coalesce(a, b, …)`, `exists(x)`, `id(x)`, `if(cond, a, b)` |
| Text | `len`, `lower`, `upper`, `trim`, `substr(t, start, n)`, `split(t, sep)`, `join(list, sep)`, `replace`, `search(field, "query")` (full-text) |
| Numbers | `abs`, `round(x, digits)`, `floor`, `ceil`, `min`, `max`, `sqrt`, `pow` |
| Time | `now()`, `today()`, `year(t)`, `month(t)`, `day(t)`, `add_months(d, n)`, `truncate(t, 1h)` |
| Collections | `len`, `keys(m)`, `values(m)`, `first(l)`, `last(l)`, `slice(l, a, b)`, `sort(l)`, `contains` |
| Queues | `peek(q)`, `len(q)`, `items(q)` (all elements in pop order, without popping) |
| Graph | `degree(p, "knows", "out")` (direction `"out"`, `"in"` or `"both"`), `nodes(path)`, `rels(path)`, `len(path)` |
| Docs | `parse(doc)` (the parsed value tree), `valid(text, format)`, `inner_text(e)` (text inside an XML element) |
| Aggregates (after `group by`, or over the whole stream) | `count()`, `count(x)`, `sum`, `avg`, `min`, `max`, `collect(x)` (into a list), `collect_set(x)` |

### 8.5 Parameters
`$name` values are supplied separately by the client (Go API, CLI `\set` command). Use them instead of building query text from user input; that prevents injection.


### 8.6 Paths inside documents
`->` steps into a `doc` value. The steps work on the parsed form (types.md §6.2), which is cached, and each base format defines what a step means. A step that finds nothing gives `null`, the same as reading an absent field.

| Base format | `->name` or `->"name"` | `->[n]` | Other |
|---|---|---|---|
| json | object key | array position (from 0) | `->[expr]`: a text expression is a key, an int is a position |
| xml | first child element with that name | nth child node (elements and text) | `->@name` or `->@"name"`: attribute value (text) |
| csv / tsv | column by header name (header option only) | row, then column | |

- **XML** paths start at the root element, so `p.config->server->@port` reads `<config><server port="80">`. A path that ends on an element gives the element record (`record{name, attrs, children}`), which can be read further with `.` (`…->server.children`). `inner_text(e)` joins all the text inside an element. Namespaced names use the `{uri}local` form as a quoted step: `->"{urn:x}item"`.
- **CSV/TSV:** without a header, `->[2]->[0]` is row 2, column 0. With the header option, each row is a map, so `->[0]->email` works.
- **Result types:**
  - For a **base format** the result is `any`. Its type is checked at run time.
  - For a **derived format** the result has the type declared in the format's shape. For `doc<invoice>`, `->total` is `decimal(12,2)` and `->lines->[0]->qty` is `int`, so type errors are reported before the query runs, and a step the shape doesn't declare is an error.
- Paths in `define index` (`config->server->port`) use the same rules.

---

## 9. Transactions

```
begin;
  update @alice set balance -= 100.00d;
  update @bob   set balance += 100.00d;
commit;

begin read only;     // consistent snapshot for several reads
rollback;
```
- **Snapshot isolation:** every statement inside a transaction sees the database as it was at `begin`, plus the transaction's own changes.
- A statement outside `begin`/`commit` is its own transaction (auto-commit).
- If two transactions change the same data, the one that commits second fails with error `conflict`. It is safe to retry, and the Go client offers an automatic retry helper.
- Definition statements (`define`, `alter`, `drop`) can run inside transactions and roll back like data changes.

---

## 10. History and time travel

Only fields with history turned on (kind or field option `history`, §5.1) keep old values.

```
history @alice.salary;                                  // all stored versions with timestamps
find Person p as of t"2026-01-01T00:00:00Z" | show p.name, p.salary;
```
- In an `as of` query, fields without history return an error if read. That's better than silently returning today's value for a past date.
- Entities that didn't exist yet at that time aren't returned. Deleted entities whose kind has history are returned as they were.

---

## 11. Users, roles and permissions

```
create user ramon password $pw;
alter user ramon password $new_pw;
alter user ramon add key "3f9a…c41e" as "laptop";   // 9auth fingerprint (protocol.md §3.4)
alter user ramon drop key "3f9a…c41e";
create role analyst;
grant read on database sales to analyst;
grant write on kind sales.Order to analyst;
grant define on database sales to analyst;
grant analyst to ramon;
revoke write on kind sales.Order from analyst;
drop role analyst;
show users;  show roles;  show grants for ramon;  show keys for ramon;
```

| Permission | Allows |
|---|---|
| `read` | queries, `describe`, `history` |
| `write` | create, update and delete entities; queue operations |
| `relate` | create and remove relations. Needs `relate` on the source's kind and `read` on the target's kind |
| `define` | `define`, `alter` and `drop` for kinds, relations, formats and indexes |
| `admin` | everything, including users, roles, grants and databases |

- Permissions are granted at **server**, **database** or **kind** level. A database-level grant covers all its kinds.
- Kindless entities fall under database-level permissions.
- Every user has an implicit role named after themselves.
- The first server start creates an `admin` user (details in the protocol and server specs).

---

## 12. Introspection

```
describe @alice;                    // all fields with types, docs, IDs; relations in and out; metadata
describe kind Person;               // schema, docs, indexes, entity count
describe relation knows;
describe #01JD...;                  // whatever the ID belongs to
show kinds;  show relations;  show formats;  show indexes;
show stats;                         // database size, counts per kind and relation type
show stats kind Person;             // value distributions used by the planner
show sessions;                      // admin: who is connected and what's running
explain find Person p | where p.age > 30;            // planned steps, indexes used, estimates
explain analyze find Person p | where p.age > 30;    // runs it; shows actual counts and timings
```

---

## 13. Results

- Every statement returns a **result**: zero or more rows with named, typed columns, plus a summary (rows affected, time taken, warnings).
- Rows are **streamed**: the client receives them as they're produced and can cancel at any time.
- Column values are PolyDB values. When a whole entity, field or relation is returned, it's sent as a structured value containing its ID, kind or type, fields and metadata. The protocol spec defines the exact form.

---

## 14. Errors

Each error has a **code**, a **message** and, where it applies, the **position** in the statement (line and column).

| Code | When |
|---|---|
| `syntax` | the statement can't be parsed |
| `type` | wrong types, for example `int + float` or a value that doesn't fit the field |
| `constraint` | `unique`, `check`, required field, `restrict` ref or relation, relation maximum, `add` on a closed kind or of an existing field |
| `unknown_field` | `set`, `unset` or a `create` field list names a field that doesn't exist (with a suggestion) |
| `not_found` | unknown database, kind, field, handle or ID |
| `conflict` | transaction conflict; safe to retry |
| `permission` | not allowed for this user |
| `limit` | timeout, row limit, depth limit or size limit exceeded |
| `internal` | a bug or storage failure |

Error messages suggest fixes where they can: "did you mean `find`?" for `FIND`, or "add `cast(x as float)`" for mixed arithmetic.

---

## 15. Grammar

### 15.1 Notation
EBNF (ISO 14977 style):
- `=` defines a rule, `;` ends it, and `|` separates alternatives.
- `[ … ]` is optional, `{ … }` repeats zero or more times, and `( … )` groups.
- `"…"` is a literal token, and `(* … *)` is a comment.

Rules apply after comments and whitespace have been removed (§1.1–1.2). Any quoted word that isn't in the reserved list (§1.5) is a contextual keyword: it acts as a keyword only where the grammar expects it, and is an ordinary identifier everywhere else.

### 15.2 Tokens

```ebnf
ident        = ident_start { ident_part }
             | "`" { ? any character except "`" ? } "`" ;     (* quoted identifier *)
ident_start  = ? Unicode letter ? | "_" ;
ident_part   = ident_start | ? Unicode decimal digit ? ;

handle       = "@" ident ;                         (* "@@" is reserved *)
param        = "$" ident ;
id_lit       = "#" 26 * crockford_char ;

digits       = digit { [ "_" ] digit } ;
int_lit      = digits | "0x" hex_digits | "0b" bin_digits ;
bigint_lit   = digits "n" ;
float_lit    = digits "." digits [ exponent ] | digits exponent | "nan" | "inf" ;
exponent     = ( "e" | "E" ) [ "+" | "-" ] digits ;
decimal_lit  = digits [ "." digits ] "d" ;
duration_lit = dur_part { dur_part } ;
dur_part     = digits [ "." digits ] ( "ns" | "us" | "µs" | "ms" | "s" | "m" | "h" ) ;

text_lit     = '"' { text_char | escape } '"'
             | "r`" { ? any character except "`" ? } "`" ;  (* raw text *)
escape       = "\n" | "\t" | "\r" | "\\" | '\"' | "\u{" hex_digits "}" ;
bytes_lit    = 'x"' { hex_digit } '"' | 'b64"' { base64_char } '"' ;
time_lit     = 't"' ? RFC 3339 timestamp ? '"' ;
date_lit     = 'd"' ? YYYY-MM-DD ? '"' ;
```

**Lexer rules:**
- The **longest match** wins. `5ns` is a duration and `5n` a bigint; `->`, `<~`, `~>`, `++`, `--`, `++=`, `--=`, `::` and `..` are single tokens.
- A number immediately followed by a unit or suffix letter is one token: `250ms`, `19.99d`. A float needs digits after the dot, so `1..3` lexes as `1` `..` `3`.
- Typed literal prefixes (`r`, `x`, `b64`, `t`, `d`) only apply when **immediately** followed by a quote or backtick. `d "x"` with a space is the identifier `d` followed by a text literal.
- Numeric literals have no sign. `-7`, `-inf` and `-5s` are unary minus applied to a literal.
- Raw text can't contain a backtick. Use a normal text literal for that.

### 15.3 Statements

```ebnf
script         = { statement ";" } ;
statement      = db_stmt | session_stmt | define_stmt | alter_stmt | drop_stmt
               | data_stmt | pipeline | txn_stmt | security_stmt | introspect_stmt ;

(* ----- Databases and session ----- *)
db_stmt        = "create" "database" ident
               | "drop" "database" ident
               | "use" ident ;
session_stmt   = "set" ident "=" expr ;

(* ----- Definitions ----- *)
define_stmt    = "define" ( kind_def | relation_def | format_def | index_def ) ;

kind_def       = "kind" ident { kind_option } "{" { field_def [ "," ] } "}" ;
kind_option    = doc_string | "closed" | history_opt ;

relation_def   = "relation" ident "from" endpoint "to" endpoint { rel_option }
                 [ "{" { field_def [ "," ] } "}" ] ;
endpoint       = ident | "any" | "field" ;
rel_option     = doc_string
               | "max" ( "out" | "in" ) int_lit
               | "on" "delete" ( "cascade" | "restrict" ) ;

format_def     = "format" ident ":" ident [ doc_string ] "{" { field_def [ "," ] } "}" ;

index_def      = [ "unique" | "text" ] "index" ident "on" ident
                 "(" index_key { "," index_key } ")" ;
index_key      = ident { "->" doc_step } ;

field_def      = ident ":" type { field_option } ;
field_option   = doc_string
               | "unique"
               | "default" expr
               | "check" expr
               | "on" "delete" ( "dangle" | "restrict" )
               | history_opt ;
history_opt    = "history" [ "keep" expr ] ;
doc_string     = [ "doc" ] text_lit ;

(* ----- Types (types.md §2) ----- *)
type           = base_type [ "?" ] ;
base_type      = "bool" | "int" | "bigint" | "float"
               | "decimal" [ "(" int_lit "," int_lit ")" ]
               | "text" | "bytes" | "time" | "date" | "duration" | "id"
               | "ref" [ "<" ident ">" ]
               | "image" | "any"
               | "doc" "<" ident ">"
               | "list" "<" type ">"
               | "array" "<" type "," int_lit ">"
               | "set" "<" type ">"
               | "map" "<" type "," type ">"
               | "pair" "<" type "," type ">"
               | "queue" "." ( "fifo" | "lifo" ) "<" type [ "," "max" int_lit ] ">"
               | "queue" "." "ring" "<" type "," int_lit ">"
               | "queue" "." "priority" "<" type [ "," ( "high" | "low" ) ]
                 [ "," "max" int_lit ] ">"
               | "record" "{" { field_def [ "," ] } "}"
               | "union" "<" type { "|" type } ">" ;

(* ----- Changing and dropping definitions ----- *)
alter_stmt     = "alter" ( alter_kind | alter_relation | alter_user ) ;
alter_kind     = "kind" ident ( "add" field_def
                              | "drop" ident
                              | "alter" field_def
                              | "rename" "to" ident
                              | "set" ( "closed" | "extensible" ) ) ;
alter_relation = "relation" ident ( "add" field_def
                                  | "drop" ident
                                  | "alter" field_def
                                  | "rename" "to" ident
                                  | "set" rel_option ) ;
alter_user     = "user" ident ( "password" expr
                              | "add" "key" text_lit [ "as" text_lit ]
                              | "drop" "key" text_lit ) ;
drop_stmt      = "drop" ( "kind" | "relation" | "format" | "index" | "user" | "role" ) ident ;

(* ----- Data ----- *)
data_stmt      = create_stmt | update_stmt | delete_stmt | relate_stmt
               | unrelate_stmt | push_stmt | pop_stmt ;

create_stmt    = "create" [ ident ] [ handle ]
                 "{" [ init { "," init } ] "}" { add_clause } ;
init           = key ":" expr [ "doc" text_lit ] ;
key            = ident | text_lit ;
add_clause     = "add" add_field { "," add_field } ;
add_field      = ident [ ":" type ] "=" expr { add_option } ;
add_option     = "doc" text_lit
               | "check" expr
               | "on" "delete" ( "dangle" | "restrict" )
               | history_opt ;

update_stmt    = "update" target update_action { update_action } ;
target         = handle | id_lit | param | ident ;     (* ident: a pipeline variable *)
update_action  = "set" assignment { "," assignment }
               | "unset" ident { "," ident }
               | add_clause
               | "drop" ident { "," ident } ;
assignment     = lvalue assign_op expr ;
lvalue         = ( ident | "::" ident ) { "." ident | "[" expr "]" } ;
assign_op      = "=" | "+=" | "-=" | "*=" | "/=" | "++=" | "--=" ;

delete_stmt    = "delete" target ;
relate_stmt    = "relate" node_ref edge node_ref [ "as" handle ] ;
unrelate_stmt  = "unrelate" ( node_ref edge node_ref | target ) ;
node_ref       = target [ "." ident ] ;                 (* entity, or one of its fields *)
push_stmt      = "push" field_ref expr [ "priority" expr ] ;
pop_stmt       = "pop" [ expr "from" ] field_ref [ "wait" expr ] [ "as" ident ] ;
field_ref      = target "." ident ;

(* ----- Queries ----- *)
pipeline       = "find" source [ "as" "of" expr ] { "|" step } ;
source         = kind_var { "," kind_var } [ "in" expr ]
               | ( handle | id_lit | param ) "as" ident ;
kind_var       = [ ident ] ident ;                      (* [Kind] variable *)

step           = "where" expr
               | "follow" [ "path" ident "=" ] pattern
               | "match"  [ "path" ident "=" ] pattern
               | "let" ident "=" expr
               | "group" "by" expr "as" ident { "," expr "as" ident }
               | "having" expr
               | "sort" sort_key { "," sort_key }
               | "distinct" [ expr { "," expr } ]
               | "skip" expr
               | "limit" expr
               | "show" show_item { "," show_item }
               | "bind" ident "as" handle
               | "union" "(" pipeline ")"
               | update_stmt | delete_stmt | unrelate_stmt | push_stmt | pop_stmt ;
sort_key       = expr [ "asc" | "desc" ] ;
show_item      = expr [ "as" ident ] ;

(* ----- Graph patterns ----- *)
pattern        = pnode { edge pnode } ;
pnode          = [ ident ] ident [ "." ident ]          (* [Kind] var, or var.field *)
               | handle [ "." ident ]
               | id_lit ;
edge           = "~"  rel_spec "~>"                     (* outgoing *)
               | "<~" rel_spec "~"                      (* incoming *)
               | "~"  rel_spec "~" ;                    (* either direction *)
rel_spec       = ( ident { "|" ident } | "*" ) [ ident ] [ brace_lit ] [ depth ] ;
depth          = "*" [ int_lit [ ".." int_lit ] ] ;

(* ----- Transactions ----- *)
txn_stmt       = "begin" [ "read" "only" ] | "commit" | "rollback" ;

(* ----- Security ----- *)
security_stmt  = "create" "user" ident "password" expr
               | "create" "role" ident
               | "grant"  ( perm { "," perm } "on" scope "to" ident | ident "to" ident )
               | "revoke" ( perm { "," perm } "on" scope "from" ident | ident "from" ident ) ;
perm           = "read" | "write" | "relate" | "define" | "admin" ;
scope          = "server" | "database" ident | "kind" [ ident "." ] ident ;

(* ----- Introspection ----- *)
introspect_stmt = "describe" ( target | ( "kind" | "relation" | "format" | "index" ) ident )
                | "show" show_what
                | "explain" [ "analyze" ] statement
                | "history" field_ref ;
show_what      = "databases" | "kinds" | "relations" | "formats" | "indexes"
               | "users" | "roles" | "sessions"
               | "grants" [ "for" ident ]
               | "keys" [ "for" ident ]
               | "stats" [ "kind" ident ] ;
```

### 15.4 Expressions

Precedence is lowest first, matching §8.1.

```ebnf
expr           = pair_expr ;
pair_expr      = or_expr [ "=>" or_expr ] ;
or_expr        = and_expr { "or" and_expr } ;
and_expr       = not_expr { "and" not_expr } ;
not_expr       = "not" not_expr | cmp_expr ;
cmp_expr       = add_expr [ cmp_tail ] ;               (* comparisons don't chain *)
cmp_tail       = ( "=" | "!=" | "<" | "<=" | ">" | ">=" ) add_expr
               | [ "not" ] "in" add_expr
               | "has" add_expr
               | "is" [ "not" ] ( "null" | type | ident )
               | [ "not" ] "between" add_expr "and" add_expr
               | "matches" add_expr
               | "starts" "with" add_expr
               | "ends" "with" add_expr
               | "contains" add_expr ;
add_expr       = mul_expr { ( "+" | "-" | "++" | "--" ) mul_expr } ;
mul_expr       = unary { ( "*" | "/" | "%" ) unary } ;
unary          = "-" unary | postfix ;
postfix        = primary { "." ident | "::" ident | "[" expr "]" | "->" doc_step } ;
doc_step       = ident | text_lit | "[" expr "]"
               | "@" ( ident | text_lit ) ;     (* xml attribute *)

primary        = literal
               | ident | handle | param | id_lit
               | "::" ident                            (* metadata of the update target *)
               | ident "(" [ expr { "," expr } ] ")"   (* function or aggregate call *)
               | "if" "(" expr "," expr "," expr ")"
               | "cast" "(" expr "as" type ")"
               | "exists" "(" expr ")"
               | "(" expr ")"
               | list_lit | brace_lit ;

literal        = "true" | "false" | "null"
               | int_lit | bigint_lit | float_lit | decimal_lit | duration_lit
               | text_lit | bytes_lit | time_lit | date_lit ;
list_lit       = "[" [ expr { "," expr } ] "]" ;
brace_lit      = "{" [ brace_items ] "}" ;
brace_items    = expr { "," expr }                     (* set *)
               | entry { "," entry } ;                 (* map or record *)
entry          = ( ident | expr ) ":" expr ;            (* bare ident = text key *)
```

### 15.5 Rules the grammar doesn't express
These are checked after parsing:
- **Kind and field options** may each appear once. `unique` and `default` aren't allowed on `record` fields or in `add` clauses.
- **`create`:** with a kind, `{ … }` may only name declared fields. Without a kind, `add` clauses give typed fields.
- **`relate`** edges must name exactly one relation type and can't have a variable or depth (`~knows{since: 2019}~>` is fine, `~knows*2~>` isn't).
- **Patterns in `follow`** must start at an already-bound variable or handle. Patterns in `match` must share at least one bound variable with the rows so far (otherwise `explain` warns of a cross product).
- **Steps after `group by`** may only use the group names and aggregates.
- **A data step** (`update`, `delete`, `unrelate`, `push`, `pop`) ends the pipeline, except `pop … as`, which may be followed by `show`.
- **`target` as a bare `ident`** is only valid inside a pipeline, where it names a variable.
- **`grant` and `revoke`:** if a list is followed by `on`, it's a list of permissions; otherwise it's a role.
- **`->@name`:** after `->`, `@name` is an XML attribute step, not a session handle. Steps must be valid for the document's format (§8.6); for derived formats this is checked before the query runs.
- **`pop`:** `pop 10 from q` versus `pop q`: when `from` follows the first expression, that expression is the count.

---

## 16. Open questions

None.

### Decided (2026-11-22)
1. No persistent entity names in v1. Use `unique` fields; the CLI gets a startup script (`~/.polydbrc`) for binding usual handles. `@@` is reserved.
2. No kind inheritance in v1. `extends` and `trait` are reserved; traits are the preferred direction if field reuse is needed later.
3. Relation types support `max out N` and `max in N`. No minimums in v1.
4. Kinds are **extensible** by default: declared fields are enforced, and single entities may `add` their own extra fields. `closed` kinds forbid that. `set` never creates fields anywhere; new fields are always created with `add`. (Revised the same day: this replaces "closed by default", keeping typo protection while allowing per-entity fields as req.md describes.)
5. Relation deletion is set per relation type: `on delete cascade` (default) or `restrict`.
6. `+` is numeric only; `++` joins and `--` removes. In `update`, `+=` is numeric, and `++=` / `--=` work on text and collections.
7. Doc paths: each base format defines its own `->` steps (XML gets element-name and `@attr` steps), and derived formats give typed results from their shape (§8.6).
