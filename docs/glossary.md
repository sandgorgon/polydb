# Glossary

Every PolyDB term, in alphabetical order, with where to read more.

**Admin.** The permission to do everything. Every server has a built-in
user called `admin` that always has it. See [operations](operations.md#users-and-permissions).

**Aggregate.** A function that summarizes many rows into one value:
`count()`, `sum`, `avg`, `min`, `max`, `collect`. Used with `group by`
or over a whole query.

**Backup.** A copy of one database at one moment, made while it runs,
and restored as a new database. See [operations](operations.md#backups).

**Blob.** A large value (long text, bytes or an image) kept in a
separate store and streamed in pieces. In Go, `*polydb.Blob`.

**Constraint.** A rule data must follow: `unique`, `check`, a required
field, a relation maximum, `on delete restrict`. Breaking one is a
`constraint` error.

**Database.** A named, separate collection of data on a server. A
session works in one at a time (`use`).

**Decimal.** An exact number type for money and quantities: `19.99d`.
Unlike `float`, it never rounds `0.1 + 0.2` wrongly.

**Doc.** A field type for structured text (`doc<json>`, `doc<xml>`,
`doc<csv>`) that is kept exactly as written but can be read inside with
`->` paths.

**Embedded.** Running PolyDB inside your own Go program instead of as a
server. See [embedding](embedding.md).

**Entity.** One stored thing (a person, an order), with fields, an ID,
metadata and relations. Like a row in a table.

**Explain.** Putting `explain` before a statement shows how it will run
without running it; `explain analyze` runs it and reports what happened.

**Export.** A portable copy of a database (or a query's result, or the
users) in a file. See [operations](operations.md#export-and-import).

**Extensible / closed.** Whether entities of a kind may carry extra
fields of their own (extensible, the default) or not (closed).

**Field.** A named value on an entity, such as `name` or `email`. Fields
have their own IDs and can carry a documentation note.

**Fingerprint.** A long hexadecimal string identifying a server's (or a
computer's) key. Checking it proves you're talking to the right server.

**Follow.** The query step that walks along relations:
`| follow p ~works_on~> Project j`.

**Format.** A description of a document type, such as a JSON shape or
CSV columns, that gives `doc` fields typed paths.

**Graph.** Data seen as things (nodes) connected by links (edges). In
PolyDB, entities are the nodes and relations the edges.

**Handle.** A name starting with `@` (`@ana`) for an entity, valid
until the session ends. Set by `create Person @ana {…}` or `bind`.

**History.** Old values of a field, kept when the field (or its kind) has
`history` turned on. Read with `history @x.field` or `find … as of t"…"`.

**ID.** The permanent unique identifier of an entity, field or relation,
written like `#01M89MMRDB000000MHP57692N1`.

**Index.** A lookup structure that makes finding entities by a field's
value fast. A **text index** does the same for word search.

**Kind.** A description of a type of entity: its fields, their types and
rules. Like a table definition.

**Match.** The query step that checks a graph shape between variables
already found (for example, "works at the same company").

**Metadata.** Facts about an entity rather than its content. PolyDB
keeps the ID, kind, created and updated times, version and owner; you
can set **labels** (a set of tags) and **props** (a map of extra facts)
on any entity. Read with `::` (`p::created`, `p::props.source`). See
the [tour](rhizome-tour.md#metadata).

**Null.** "No value". A `?` after a type (`int?`) allows it.

**Parameter.** A value passed separately from a statement's text,
written `$name` in the statement. The safe way to use input from users.

**Permission.** What a user may do: `read`, `write`, `relate`, `define`
or `admin`, on the server, a database or a kind.

**Pipeline.** A query's chain of steps joined by `|`, each taking the
rows from the one before.

**Queue.** A field type holding items in order (first-in-first-out,
last-in-first-out, ring or by priority), changed with `push` and `pop`.

**Ref.** A field value pointing to another entity (`ref<Person>`), like a
foreign key. Compare with a relation.

**Relation.** A named, directed link between two entities (or fields),
with its own ID and optional properties: `@ana ~works_on~> @site`.

**Rhizome.** PolyDB's language, for defining, changing, querying and
inspecting data. See the [tour](rhizome-tour.md).

**Role.** A named set of permissions that can be given to users.

**Session.** One connection's state: the database in use, settings,
handles and open transactions.

**Shell.** The interactive prompt started by `polydb`. See the
[CLI](cli.md#the-shell).

**Transaction.** A group of changes that happen together or not at all,
between `begin` and `commit`.

**Unix socket.** A connection point that is a file on the server's
computer, used instead of the network by local programs.

**Upload.** Sending a large value to the database as a stream before the
statement that stores it.

## Where next

- [Concepts](concepts.md): how these fit together
- [FAQ](faq.md)
