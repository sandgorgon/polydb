# What stays the same across versions

PolyDB follows [semantic versioning](https://semver.org). A version is
`MAJOR.MINOR.PATCH`, for example `1.4.2`:

- **Patch releases** (1.4.2 → 1.4.3) only fix bugs.
- **Minor releases** (1.4 → 1.5) add things. Everything that worked on
  1.4 keeps working on 1.5.
- **Major releases** (1.x → 2.0) may change anything. They are rare, and
  the CHANGELOG says what to do when upgrading.

This page lists what "keeps working" means for each part of PolyDB
throughout version 1.

## Your data

| What | The promise |
|---|---|
| The data directory | Any 1.x reads a data directory written by an earlier 1.x. If a minor release ever needs to change how data is stored, the CHANGELOG says so, the change happens when the server first opens the directory, and earlier versions then refuse it with a clear message rather than misreading it. Take a backup before upgrading. |
| Backups (`backup database`) | Any 1.x restores a backup made by any earlier 1.x. |
| Exports (`export database`, `export security`) | Any 1.x imports an export made by any earlier 1.x, in either format (JSON Lines or native). An export is also the way to move data to a later major version. |

Every stored format carries a version number, so PolyDB never guesses:
data it can't read is refused, never silently misread.

## The query language, Rhizome

- A statement that runs on 1.x runs on every later 1.x and gives the same
  result, except where the earlier result was a bug.
- New statements, functions, types and options may be added. New words
  are added only as *contextual* keywords: they are special in one
  position only, so they can't break a name you already use. The only
  reserved words are those listed in spec/rhizome.md §1.
- Error **codes** (`limit`, `permission`, `not_found`, …) stay the same.
  Error **messages** may be reworded, so programs should check the code,
  not the text.
- Defaults that affect results (such as `max_depth`) don't change within
  1.x. Resource limits (`max_memory`, connection limits, …) may get
  larger defaults.

## The network protocol

- The protocol has its own version, 1.0 for PolyDB 1.0. Every 1.x client
  works with every 1.x server, whichever is newer: the two agree on the
  version they share when they connect.
- Later minor versions only add things an older peer can ignore.

## The Go packages

Three packages are public and follow the rules above:

- `github.com/sandgorgon/polydb`: the application interface (`DB`, `Tx`,
  `Rows`, …) and the embedded database (`Open`, `OpenMemory`)
- `github.com/sandgorgon/polydb/client`: connecting to a server
- `github.com/sandgorgon/polydb/value`: PolyDB's value types

Within 1.x:

- Code that compiles against 1.x compiles against every later 1.x, with
  the usual Go exceptions: code that embeds PolyDB's types in its own
  structs, uses unkeyed struct literals, or compares function values can
  be affected by additions.
- **The interfaces `DB`, `Querier`, `Tx`, `Stmt`, `Rows` and `Uploader`
  may gain methods.** They are meant to be used, not implemented
  outside PolyDB. To wrap one, embed it in your own type, so
  new methods pass through. `BlobSource` is meant to be implemented and
  won't change.
- Struct types may gain fields and options may gain settings.
- Everything under `internal/` may change in any release; Go stops
  programs outside PolyDB from importing it.
- PolyDB supports the two most recent Go releases, as Go itself does.
  Version 1.0 needs Go 1.26 or later.

## The `polydb` command

- Commands, flags and configuration file settings keep their names and
  meanings. Settings may be added. A setting removed in a later major
  version is first deprecated, with a warning, in a minor release.
- The shell's `tsv` output keeps its shape, for scripts. The `table`
  output and messages are for people and may change.
- `--trace` output is for debugging and may change.

## What isn't covered

- Performance: releases aim to get faster, but timings aren't promised.
- Behavior that the documentation calls a bug, or that contradicts it.
  Fixing those may change results; the CHANGELOG lists each such fix.
- Security fixes, which may break things that were never meant to work,
  for example a permission check that was missing.
