# PolyDB Type System Specification

Status: **Agreed** (2026-11-21)
Related: [plan.md](../plan.md) sections 1–2

This document defines every value type PolyDB stores, how values compare and sort, how text is normalized, and how values are encoded in binary. The Rhizome language spec and the wire protocol spec both build on it.

Words in **MUST** / **MUST NOT** / **SHOULD** / **MAY** are requirements on the implementation.

---

## 1. Principles

1. **Every value has exactly one type.** There is no silent change of type on write. A field declared `int` never ends up holding a `float`.
2. **Few implicit conversions.** Only safe, lossless widenings happen automatically (section 10). Everything else needs an explicit cast.
3. **Deterministic everywhere.** Equality, ordering, set and map iteration, and encoding are fully deterministic. The same value always encodes to the same bytes.
4. **Text is always valid, NFC-normalized Unicode.** Invalid UTF-8 is rejected, never stored.
5. **Large values are handled transparently.** Big byte strings, images and collections are stored out-of-line, but they behave exactly like small ones in the language.

---

## 2. Type expressions

How types are written in Rhizome and in this spec:

```
type      = base | base "?"                       -- "?" makes it optional (may be null)
base      = scalar | media | structured | collection | queue | composite | "any"
scalar    = "bool" | "int" | "bigint" | "float" | "decimal" [ "(" P "," S ")" ]
          | "text" | "bytes" | "time" | "date" | "duration" | "id" | ref
ref       = "ref" [ "<" KindName ">" ]
media     = "image"
structured= "doc" "<" FormatName ">"
collection= "list<" type ">" | "array<" type "," N ">" | "set<" type ">"
          | "map<" type "," type ">" | "pair<" type "," type ">"
queue     = "queue.fifo<" type [ "," "max" N ] ">"
          | "queue.lifo<" type [ "," "max" N ] ">"
          | "queue.ring<" type "," N ">"
          | "queue.priority<" type [ "," ( "high" | "low" ) ] [ "," "max" N ] ">"
composite = "record" "{" { name ":" type [ doc-string ] } "}"
          | "union" "<" type { "|" type } ">"
```

Examples: `text?`, `list<int>`, `map<text, list<float>>`, `queue.ring<text, 100>`, `record{ x: float, y: float }`, `union<int | text>`.

---

## 3. Null and optional values

- `null` is a single value meaning "no value".
- A type is **not nullable** unless written with `?`. Writing `null` into an `int` field is an error; writing it into `int?` is allowed.
- `any` accepts every value, including `null`.
- Fields an entity adds itself (on a kindless entity, or beyond what its kind declares) have the type given when they are added, or `any` if none is given (see rhizome.md §6.2).
- **Absent vs null:** a field that doesn't exist on an entity is *absent*. A field that exists and holds `null` is *null*. Rhizome can tell them apart (`exists(e.f)` vs `e.f = null`).
- Null comparisons use **two-valued logic**: `null = null` is true, and `null` sorts before every other value (section 11). There is no SQL-style "unknown" third value.

---

## 4. Scalar types

### 4.1 `bool`
`true` or `false`.

### 4.2 `int`
- Signed 64-bit integer.
- Overflow in arithmetic is an **error**. Values never wrap and never silently become `bigint`.

### 4.3 `bigint`
- Arbitrary-precision signed integer. Go: `math/big.Int`.
- Limit: 4096 bytes of magnitude (about 9,860 decimal digits).

### 4.4 `float`
- IEEE 754 binary64.
- `-0.0` is normalized to `+0.0` on write.
- `NaN` is allowed, normalized to a single canonical NaN, and equals itself (unlike IEEE). This keeps equality, sets and indexes consistent.
- `+Inf` and `-Inf` are allowed.

### 4.5 `decimal`
- Exact base-10 number: an arbitrary-precision integer coefficient plus a scale (number of digits after the point).
- `decimal(P, S)` constrains a field to at most P total digits and exactly S digits after the point. Writes are rounded to S using **round half to even**; values exceeding P digits are an error.
- Plain `decimal` has no constraint, up to 1,000 digits total.
- Equality ignores trailing zeros (`1.50 = 1.5`). Display preserves the stored scale.
- The standard library has no decimal type, so `value/` implements one on `math/big.Int`.

### 4.6 `text`
- A sequence of Unicode code points stored as UTF-8.
- **Validation:** invalid UTF-8 (including encoded surrogates) is rejected with an error.
- **Normalization:** every text value is converted to **NFC** (`golang.org/x/text/unicode/norm`) on write, everywhere: field values, map keys, set elements, identifiers and query literals. Two strings that look identical therefore compare equal.
- **Length** (`len`) counts code points after normalization. Substring and length limits also work in code points. Grapheme functions (counting what users see as one character) are not in v1 (decided). They may be added later alongside the TUI without changing existing behaviour.
- **Ordering** is by code point (equivalent to comparing NFC UTF-8 bytes). This is the only text ordering in v1 (decided). Language-aware ordering (collation) may be added after v1.0. The key-encoding type group byte (section 12.2) leaves room for collated text keys.
- Limit: 64 MiB per value. Values over 16 KiB are stored out-of-line (section 13).

### 4.7 `bytes`
- An arbitrary byte string. Not validated or normalized.
- Optional **media type** attribute (for example `application/pdf`). It's metadata only: it isn't validated against the content and doesn't affect equality.
- Limit: 4 GiB per value. Values over 16 KiB are stored in the blob store (section 13).

### 4.8 `time`
- An instant in time, independent of time zone. Stored as UTC seconds since 1970-01-01 (signed 64-bit) plus nanoseconds (0–999,999,999).
- Range: years 0001–9999.
- Input with a time-zone offset is converted to UTC; the original offset isn't kept. Converting to and from local time is the application's job (decided). PolyDB has no zoned-time type. An application that needs the original zone can store it in a separate `text` field.
- Text form: RFC 3339, for example `2026-11-21T14:30:00.5Z`.

### 4.9 `date`
- A calendar date with no time and no time zone, years 0001–9999. Text form `2026-11-21`.

### 4.10 `duration`
- A signed length of time in nanoseconds (64-bit), about ±292 years.
- Text form: `1h30m`, `250ms`, `-5s` (same as Go's `time.ParseDuration`).
- Calendar amounts like "1 month" aren't durations; they're handled by date functions in Rhizome.

### 4.11 `id`
- A 128-bit PolyDB identifier. Every entity, field and relation has one.
- Layout (big-endian):

  | Bits | Meaning |
  |---|---|
  | 48 | milliseconds since 1970-01-01 UTC |
  | 16 | node ID (0 on a single machine; reserved for multi-node) |
  | 64 | random, with the top bits used as a per-millisecond counter so IDs from one node always increase |

- IDs created on one node sort in creation order. This keeps new data close together on disk.
- Text form: 26 characters of Crockford base32, for example `01JD8X3K7M0000Q2W5ZB9N4T6R`.

### 4.12 `ref` and `ref<Kind>`
- A value that points to an entity or a field by its `id`. `ref<Person>` must point to an entity of kind `Person`.
- **A ref is a value, not a relation.** Relations are the graph's edges: they have their own IDs, types and properties, are indexed in both directions and are followed by graph traversal. A ref is simply data stored in a field, like a foreign key. It is useful inside collections, for example a `queue.fifo<ref<Task>>` of work items.
- **What happens when the target is deleted is set per field** (decided) with an `on delete` option:

  | Option | Effect |
  |---|---|
  | `dangle` (default) | The delete goes ahead. The ref keeps the old ID, and reading it through the ref returns `null`. `exists(ref)` tells you whether the target still exists. |
  | `restrict` | The delete fails while any field with this option still points to the target. |

  Example: `owner : ref<Person> on delete restrict`.
- Refs inside free-form (`any`) fields and inside collections always use `dangle`.
- `restrict` needs a reverse index (target ID → fields pointing at it). That index is kept only for fields that use `restrict`, so `dangle` fields cost nothing extra.

---

## 5. Media type: `image`

An `image` value has these parts:

| Part | Type | Notes |
|---|---|---|
| `format` | text | `png`, `jpeg`, `gif`, `webp`, `bmp` or `tiff` |
| `width`, `height` | int | in pixels |
| `data` | bytes | the original encoded file, unchanged |

- **On write**, the header is decoded (`image.DecodeConfig`) to check the format and read the dimensions. If the data isn't a supported format, the write is rejected; store it as `bytes` instead.
- Decoders: standard library for png, jpeg and gif; `golang.org/x/image` for webp, bmp and tiff. webp is decode-only, so PolyDB can't create new webp images, but it can store and read them.
- The original bytes are never re-encoded, so images round-trip exactly.
- Rhizome can read the parts directly: `p.photo.width`, `p.photo.format`.
- Equality compares `data` only.
- Thumbnails and other derived images are out of scope for v1.

---

## 6. Structured text: `doc<format>`

The requirement is text "with a key on how to interpret it". The **format** is that key.

A `doc<F>` value is text that is guaranteed to be valid in format `F`.

- **Storage:** the original text is kept exactly as written (after NFC). It is never reformatted.
- **Validation:** checked on every write; invalid documents are rejected.
- **Querying:** Rhizome can read inside a document with path expressions (syntax defined in the Rhizome spec). The parsed form is cached, and fields inside documents can be indexed.

### 6.1 Formats

A format is a named entry in a database's catalog. There are two kinds:

1. **Base formats** define syntax. Each one is Go code that validates text and parses it into a tree of PolyDB values.
   - Built in: `json`, `xml`, `csv`, `tsv`.
   - More can be added in Go through the engine API (`engine.RegisterFormat`) by building a custom server. They can't be defined in Rhizome, because parsing a new syntax needs real code.
2. **Derived formats** add rules on top of a base format, and *are* defined in Rhizome. For example:

   ```
   define format invoice : json "Customer invoice" {
     number : text
     total  : decimal(12,2)
     lines  : list<record{ sku: text, qty: int }>
   }
   ```

   A `doc<invoice>` must be valid JSON *and* match that shape.

### 6.2 How each built-in format maps to values

| Format | Becomes |
|---|---|
| json | object → `map<text, any>`, array → `list<any>`, number → `int` if whole and in range, else `float`, plus string → `text`, bool, null |
| xml | element → `record{ name: text, attrs: map<text,text>, children: list<any> }`, text node → `text` |
| csv / tsv | `list<list<text>>`; table formats read rows as maps or records (§6.3) |

A **derived format based on json** reads numbers exactly where its shape expects a decimal: `"total": 2.50` in a `decimal(12,2)` field is `2.50`, not the float 2.5 (decided 2026-11-22). Elsewhere, numbers follow the json rules above.

### 6.3 Table formats

A derived format on `csv` or `tsv` is a **table format** (Rhizome spec §5.3). Its fields declare columns rather than a document shape, and the `header` option says the first line names the columns:

| Declared | Parsed value |
|---|---|
| neither | `list<list<text>>`, as the base format |
| `header` only | `list<map<text, text>>`, keyed by column name |
| columns | `list<record{ columns }>`, with each cell converted from text to its column's type |

Columns are found by `at n` (position from 0), by header name, or by declared order. Undeclared columns are ignored. Empty or missing cells take the column's `default`, or null for an optional column, or `""` for a required text column; otherwise the document is invalid.

---

## 7. Collections

| Type | Ordered? | Duplicates? | Size |
|---|---|---|---|
| `list<T>` | yes, by position | yes | grows and shrinks |
| `array<T, N>` | yes, by position | yes | exactly N elements, always |
| `set<T>` | no (iterated in sort order) | no | grows and shrinks |
| `map<K, V>` | no (iterated in key sort order) | keys unique | grows and shrinks |
| `pair<K, V>` | n/a | n/a | exactly one key and one value |

Rules for all collections:

- **Element types:** `T` may be any type, including other collections and `any`. `list<any>` holds mixed types.
- **Set elements and map keys** must be a *keyable* type: `bool`, `int`, `bigint`, `float`, `decimal`, `text`, `bytes`, `time`, `date`, `duration`, `id`, `ref`, or a `pair`/`record`/`array` made only of keyable types. Lists, sets, maps, images, docs and queues can't be keys.
- **Iteration order** of sets and maps is the sort order of their elements or keys (section 11). It doesn't depend on insertion order, so results are reproducible.
- **Arrays** are created at full length. Unset elements are `null`, so the element type must be nullable or every element must be given on creation.
- **Equality** is deep: two collections are equal if they have the same type family and equal contents in the same order (for lists and arrays) or the same members (for sets and maps).
- **Nesting** is limited to 32 levels.
- **Size:** up to 2³² − 1 elements.

---

## 8. Queues

| Type | Pops | When full |
|---|---|---|
| `queue.fifo<T>` | oldest first | unbounded unless `max N` is given; then push fails |
| `queue.lifo<T>` | newest first (a stack) | same as fifo |
| `queue.ring<T, N>` | oldest first | push **overwrites the oldest** element |
| `queue.priority<T>` | by priority number, direction set per queue (below); equal priorities pop oldest first | same as fifo |

- **Operations:** `push`, `pop`, `peek`, `len` and `clear`. `pop` and `peek` on an empty queue return `null` instead of failing.
- **Priority** is a signed 64-bit integer given at push time (`push q x priority 5`).
- **Direction is set per queue** (decided) and is fixed once the queue is created:
  - `queue.priority<T>` or `queue.priority<T, high>`: **larger numbers pop first** (the default).
  - `queue.priority<T, low>`: **smaller numbers pop first**. This suits scheduler queues that use a timestamp as the priority, so the earliest due item pops first.
  - To change direction, rebuild the queue into a new field.
- Queues can also be read without consuming them, in pop order (for inspection).
- **Concurrency:** a `pop` is atomic. If two transactions pop from the same queue at once, each gets a different element or one of them retries. The transaction design (`txn/`) must handle queues specially so a busy queue doesn't cause constant conflicts. This is flagged as a design task for Phase 4.

---

## 9. Composite types

### 9.1 `record{ ... }`
- A fixed set of named fields, each with its own type and an optional doc string.
- Fields marked `?` may be null.
- Records can be nested inside fields, collections and other records.
- They differ from entities: a record is a value inside one field. It has no ID and can't take part in relations. For something that needs relations, use an entity.

### 9.2 `union<A | B | ...>`
- The value is exactly one of the listed types.
- The stored value remembers which type it is, so `union<int | float>` holding `1` stays an `int`.
- Rhizome can check the type (`typeof(v)`, `v is int`).

### 9.3 `any`
- Holds any value with its type attached. Used for free-form fields and mixed collections.

---

## 10. Conversions

### 10.1 Implicit (automatic, lossless only)

| From | To | When |
|---|---|---|
| `int` | `bigint`, `decimal` | arithmetic with that type, or writing to a field of that type |
| `bigint` | `decimal` | same |
| `text` literal | `doc<F>` | writing to a `doc<F>` field; validated, error if invalid |
| `text` literal | `time`, `date`, `duration`, `id` | writing to a field of that type; parsed, error if invalid |
| `ref<K>` | `ref` | always |
| any value | `any`, or a `union` that includes its type | always |
| `T` | `T?` | always |

**No implicit `int` ↔ `float` conversion** (decided). Integers above 2⁵³ can't be represented exactly as floats, so mixing them always needs an explicit cast. This also applies to writes: storing `3` into a `float` field is an error; write `3.0` or `cast(3 as float)`.

### 10.2 Explicit (`cast(v as T)`)

Allowed casts include number ↔ number (with rounding or an error on overflow), text ↔ number, text ↔ time/date/duration/id, bytes ↔ text (UTF-8 validated), `doc<F>` → its parsed value, and `any` → a specific type (error if it doesn't match). The full table is in the Rhizome spec.

### 10.3 Mixed arithmetic
- `int op int` → `int` (error on overflow); `int op decimal` → `decimal`; `bigint op int` → `bigint`; `bigint op decimal` → `decimal`.
- `float` only combines with `float`. `int op float`, `bigint op float` and `decimal op float` are all **errors** until one side is cast. Mixing exact and approximate numbers silently causes precision and money bugs.
- **Comparisons are different:** `=`, `<` and the rest work across all numeric types without a cast, because they compare exact mathematical values (section 11.1). Nothing is converted, so nothing is lost. `p.age > 30.5` is valid even when `age` is an `int`.

---

## 11. Equality and ordering

### 11.1 Numbers compare by value
All numeric types compare by their exact mathematical value: `1 = 1.0 = 1.00 = bigint 1`. The comparison never converts an integer to a float, so `9007199254740993` (2⁵³ + 1) is correctly greater than the float `9007199254740992.0`. NaN is the largest number.

### 11.2 Total order across all values
Sorting and indexes need an order for *every* value, including mixed `any` fields. Values are first ordered by type group, then by value within the group:

1. `null`
2. `bool` (false < true)
3. numbers (`int`, `bigint`, `float`, `decimal` compared together by value)
4. `text` (code point order)
5. `bytes` (byte order)
6. `date`
7. `time`
8. `duration`
9. `id`
10. `ref`
11. `doc` (by format name, then text)
12. `image` (by data bytes)
13. `pair`, `array`, `list` (element by element; shorter first if one is a prefix of the other)
14. `record` (field by field, in declared order)
15. `set` (element by element in sort order)
16. `map` (entry by entry in key order)
17. queues (in pop order)

Using `<` or `>` between values of different groups in a `where` clause is a **type error** in Rhizome. The cross-type order exists for sorting and indexes, not for comparisons people write.

---

## 12. Binary encodings

There are two encodings with different jobs.

### 12.1 Value encoding
Used for storing values and sending them over the wire.

- Each value is a **tag byte** followed by its payload. Lengths and integers use varints.
- It is compact and fast to decode. It is *not* order-preserving.
- Encoding is **canonical**: each value has exactly one encoding. For example, set elements and map entries are written in sort order, and floats are normalized first.
- Tags:

  | Tag | Type | Payload |
  |---|---|---|
  | 0x00 | null | none |
  | 0x01 / 0x02 | false / true | none |
  | 0x10 | int | zigzag varint |
  | 0x11 | bigint | sign byte + length + magnitude bytes |
  | 0x12 | float | 8 bytes big-endian |
  | 0x13 | decimal | scale (varint) + bigint coefficient |
  | 0x20 | text | length + UTF-8 bytes |
  | 0x21 | bytes | length + bytes (+ optional media type) |
  | 0x22 | blob reference | content hash + length (for out-of-line values) |
  | 0x30–0x33 | time, date, duration, id | fixed-width fields |
  | 0x34 | ref | target id (+ kind id if constrained) |
  | 0x40 | image | format + width + height + data |
  | 0x41 | doc | format id + text |
  | 0x50–0x54 | list, array, set, map, pair | count + elements |
  | 0x58–0x5B | fifo, lifo, ring, priority | parameters + elements |
  | 0x60 | record | record type id + fields in order |
  | 0x61 | union / any | carries the inner value's own tag |
  | 0x70 | collection reference | pointer to an out-of-line collection (section 13) |
  | 0x80–0x8F | result-only values | entity, field, relation and path values sent in query results; never stored (protocol.md §6.5) |
  | 0xF0–0xFF | reserved | future types |

- The exact byte layout of each payload is documented on `value.AppendValue` (value/encode.go). Notes:
  - `ref` is just the target ID. The `ref<Kind>` constraint belongs to the field's type, not the value.
  - `doc` carries its format by name, and `record` carries its field names. Catalog IDs could replace the names later as a storage optimization.
  - A value in an `any` or `union` field is encoded as itself, because every encoding already carries its type. Tag 0x61 stays reserved.
- **Decoding is strict:** only canonical encodings of well-formed values are accepted (minimal varints, sorted sets and map keys, NFC text, valid images, queues in pop order). This means `encode(decode(b)) = b` always holds, and data from the network can't create invalid values.

### 12.2 Key encoding
Used inside the storage engine for index keys and sorting.

- **Order-preserving:** comparing the encoded bytes gives the same answer as the order in section 11. This lets the storage engine sort and range-scan values without decoding them.
- Only keyable types (section 7) have a key encoding.
- It starts with a byte for the type group (section 11.2) so different types sort correctly against each other. All numeric types share one encoding so that `1` and `1.0` encode identically.
- Techniques: sign-flipped big-endian integers and zero-byte escaping for text and bytes. **Numbers** of every type are written as their exact decimal value, `0.d1d2… × 10^E`: a class byte (−inf, negative, zero, positive, +inf, NaN), the exponent, then the digits in pairs. Every float has a finite decimal expansion, so this is exact and agrees with §11.1 (the float `0.1` sorts just above `0.1d`). The cost is long keys for floats with extreme exponents.
- Keys are self-delimiting (no key is a prefix of another), so composite keys are plain concatenations. The full layout is documented at the top of value/key.go.
- **Long text and bytes:** a value longer than 256 bytes is keyed by its first 256 bytes and the SHA-256 of its content. Keys stay small, and a value in the blob store is keyed without loading it. Two such keys are equal exactly when the values are, and they order correctly against every other key; but two long values with the same first 256 bytes order by hash, not content. Unique constraints and equality lookups are exact. A range or `starts with` lookup whose bound is longer than 256 bytes scans instead of using the index, and reading an index in order (top-N) reads on through values that share their first 256 bytes, so results always follow §11.
- It can't always be decoded back to the exact original type (for example, `1` and `1.0` encode the same). Indexes therefore point back to the stored value, which keeps the real type.

### 12.3 JSON form
Used by exports (rhizome.md §4). It is exact: reading it back gives the value that was written.

- **Plain JSON** where JSON has the type: `null`, `true`/`false`, int as a number, text as a string, list as an array, and a map whose keys are all text not starting with `$` as an object.
- **Every other value** is an object with one key starting with `$` that names its type, plus, for some, plain keys with details:

| Type | Form |
|---|---|
| float | `{"$float": 1.5}`, also `"nan"`, `"inf"`, `"-inf"` |
| bigint, decimal | `{"$bigint": "123…"}`, `{"$decimal": "19.99"}` |
| bytes, image | `{"$bytes": "<base64>", "media": "image/svg+xml"}`, `{"$image": "<base64>"}` |
| time, date, duration | `{"$time": "2026-11-21T14:30:00Z"}`, `{"$date": "2026-11-21"}`, `{"$duration": "1h30m"}` |
| id, ref | `{"$id": "<id>"}`, `{"$ref": "<id>"}` |
| doc | `{"$doc": "<text>", "format": "invoice"}` |
| array, set, pair | `{"$array": […]}`, `{"$set": […]}`, `{"$pair": [k, v]}` |
| other maps | `{"$map": [[k, v], …]}` |
| record | `{"$record": {"name": v, …}}`, fields in order |
| queues | `{"$fifo": […], "limit": 10}`, `{"$lifo": …}`, `{"$ring": […], "capacity": 5}`, `{"$priority": [[prio, item], …], "low_first": true, "limit": 0}`, items in pop order |
| large value in an export | `{"$blob": "<sha256>", "type": "bytes", "len": 123, "media": "…"}`, its content in the export's data records |

- Reading JSON from elsewhere: a number is an int if it is whole and fits in 64 bits, a float otherwise (as for `json` docs), and a whole number beyond 64 bits is an error (write it as `$bigint`).

---

## 13. Large values

- **Byte strings, text and images** over **16 KiB** go to the blob store. They're split into 1 MiB chunks, and each chunk is addressed by its SHA-256 hash, so identical content is stored only once. The field holds a blob reference (tag 0x22). Only whole field values (and their history versions) are stored this way; a large value inside a collection, a record, metadata or relation properties stays inline.
- **Collections and queues** over **1,000 elements** (or 64 KiB encoded) are stored element by element in the key-value store instead of as one blob. Pushing to a queue, adding to a set or changing one map entry then doesn't rewrite the whole collection. (Not implemented yet: deferred until after v1.0, see plan.md.)
- The blob threshold is an engine setting (`engine.Options.BlobThreshold`, at least 4 KiB), the same for every database.
- None of this is visible to queries.

### 13.1 The blob store

- **Segments:** each database keeps chunks in append-only segment files (`databases/<n>/blobs/00000001.seg`, …), separate from the key-value store, so large values are written once instead of being rewritten by compaction, and don't crowd the cache. A segment is sealed at 256 MiB and a new one started. Each chunk in a segment carries its hash, length and a CRC-32C, checked on every read.
- **Records in the key-value store:** a blob record per distinct content (its length and chunk hashes), a location record per distinct chunk (segment, offset, length), and a **use record** for each entity that references a blob from a field or a history version. Writers only add use records, each for its own entity, so writers storing the same content never conflict with each other.
- **Durability:** a transaction's new chunks are synced to their segment before it commits, so a committed reference always finds its data. A crash before the commit leaves unreferenced data at the end of a segment, which a later rewrite reclaims.
- **Reads are lazy.** Reading a large field gives a reference to its content, not the content. A statement loads the content only where it needs it, and passing a value along loads nothing:
  - showing it: the server sends it by lease and reads it from the blob store when the client fetches it (protocol.md §7);
  - storing it in another field or entity: the new use shares the stored content;
  - `len` (bytes have the length; text counts characters a piece at a time), `is null` and other type tests, `typeof`, `count`, `coalesce` and `if`;
  - unique constraints and indexes on the field, whose keys hold the value's start and hash (§12.2), and full-text indexes, which read the text a piece at a time.

  Comparing, searching and cutting read the content a piece at a time, without loading it: `=` and `!=` (by hash when both are stored, otherwise by length and then content), `<`, `<=`, `>`, `>=`, `between`, `sort`, `min`, `max`, `group by` and `distinct` (keyed like an index), `starts with`, `ends with`, `contains`, `matches` and `substr` (which reads only up to the end of its cut).

  Anything else loads the content: functions and operators that build a new value from it (`upper`, `++`, `replace`, conversions and the like), `search()`, and check constraints that use such functions. Loaded content (and `substr`'s result) counts against a per-statement limit, the session setting `max_load` (rhizome.md; 256 MiB by default, a server setting), and a statement that would go over it fails with `limit`. A value loaded twice in a statement is loaded once.

  Updating other fields of an entity doesn't load its large values, and statistics sampling never fetches blob content (it counts distinct values by hash). A reference read in a transaction stays readable after it ends: it holds the segments it reads until it is dropped, and the collector lets dropped references release theirs before it deletes segment files.
- **Uploads** (protocol.md §7) go to the blob store as they arrive. Text is normalized to NFC and checked to be UTF-8 while it streams, and an image's header is checked. A statement given an upload stores it without reading it again, or fails with `type` if it wasn't valid.
- **Collection** runs with the history sweep (hourly by default). An entity that may have dropped a reference (a field overwritten or removed, the entity deleted, history pruned) is marked; the collector checks marked entities against the references they still hold, deletes blobs left without uses and chunks no blob lists, and rewrites sealed segments that are less than half live into the active one. A segment is deleted only once no open transaction's snapshot and no download lease (protocol.md §7) can still read it.
- **Conflicts** settle races between writers and the collector: a writer reusing a blob guards its record, which the collector deletes; a writer reusing a chunk for new content rewrites the chunk's location record, which the collector deletes or moves. Whichever commits second fails and retries.

---

## 14. Limits

| Item | Limit |
|---|---|
| `text` value | 64 MiB |
| `bytes` value | 4 GiB |
| `bigint` | 4096 bytes of magnitude |
| `decimal` | 1,000 digits |
| collection elements | 2³² − 1 |
| nesting depth | 32 |
| field names, kind names, format names | 1–255 code points; letter or `_` first, then letters, digits and `_` (Unicode letters allowed) |
| doc strings | 64 KiB |

---

## 15. Go mapping (package `value`)

```go
// Value is an immutable PolyDB value.
type Value interface {
    Type() Type
    // plus Equal/Compare/Encode/... defined in package value
}
```

| PolyDB | Go representation |
|---|---|
| bool | `bool` |
| int | `int64` |
| bigint | `*big.Int` |
| float | `float64` |
| decimal | `value.Decimal` (custom) |
| text | `string` (always valid NFC UTF-8) |
| bytes | `[]byte` or a lazy blob reader |
| time / date / duration | `time.Time` / `value.Date` / `time.Duration` |
| id / ref | `value.ID` (`[16]byte`) / `value.Ref` |
| image | `value.Image` |
| collections, queues, record | `value.List`, `value.Map`, ... with iterator methods, lazily loaded when stored out-of-line |

---

## 16. Open questions

None.

### Decided (2026-11-21)
- Priority queues: direction configurable per queue (`high` or `low`), default larger-first.
- Text length and substrings in code points only for v1; grapheme functions possibly later.
- Collation: code-point order only in v1.
- No implicit `int` ↔ `float` conversion; explicit cast required.
- Dangling refs: configurable per field (`dangle` default, or `restrict`).
- Time: UTC instants only; applications handle time-zone conversion.

### Clarified during implementation (Phase 1, 2026-11-22)
- **ID layout:** the 64 low bits are a 16-bit per-millisecond counter followed by 48 random bits.
- **Ties within a sort group:** values of different families with equal contents (the list `[1]` and the array `[1]`) are ordered by family, so they are never equal: pair < array < list, and fifo < lifo < ring < priority.
- **Equal set elements of different numeric types** (`1` and `1.0` in a `set<any>`): the one with the lower type code is kept, so the result doesn't depend on insertion order. Duplicate map keys are an error.
- **Queue literals:** a queue is written as a list of its elements in pop order. A priority queue is written as a list of `priority => element` pairs. Equality ignores the size limit and direction, which come from the type.
- **Image and doc literals:** there is no dedicated literal, so they print as `cast(x"…" as image)` and `cast("…" as doc<F>)`.
- **JSON:** a number is an `int` when its value is whole and fits in 64 bits, including forms like `1.0` and `1e3`. A repeated object key keeps its last value. Numbers outside the float range are rejected.
- **XML:** the parsed form is the root element. Whitespace-only text, comments, processing instructions and directives are dropped. Adjacent text is merged. Namespaced names are written `{uri}local`, and namespace declarations keep their `xmlns:p` form.
- **CSV/TSV:** every row must have the same number of fields. Headers and typed columns come from table formats (§6.3).
- **Doc conformance:** a `doc<F>` value must carry format `F` exactly. A derived format's document is not accepted as its base format.
- **Queue fields start empty:** a declared queue field never needs a value on `create`. An entity gets an empty queue of the declared type, including when `alter kind` adds a queue field.
