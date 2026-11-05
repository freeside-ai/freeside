# Package Layout Sections Are Checked Against Their Files

Issue #572. Agents orient in a `daemon/` package by reading the "Layout, by
concept" list in its `doc.go`. Six of the seven lists had drifted, all by
omission (`domain` named 24 of 109 files), and nothing compared a list with
the directory. This unit corrected the lists, added sections to `engine` and
`verify`, and added `daemon/internal/doclayout`, a test-only package that
fails when a list and its directory disagree.

## Decisions

- **A section opts a package in; a fixed set keeps required sections from
  disappearing.** The test checks every `doc.go` whose package comment has a
  paragraph starting with `Layout` followed by a list, and holds a fixed
  list of nine packages (`domain`, `engine`, `exec`, `exec/fake`,
  `exec/fake/forge`, `export`, `importer`, `verify`, `ward`) that must keep
  one. Chose this over requiring a section in every package because most
  packages are small enough that a list adds nothing, and over opt-in alone
  because deleting a stale section would then silence the test instead of
  fixing it.
- **The test reads the directory, not a build context.** `os.ReadDir` counts
  `_darwin.go` and `_linux.go` files on every platform; `go/build` would
  hide whichever files the host's tags exclude, and the list would pass on
  one OS and fail on the other.
- **Lists are parsed as gofmt doc-comment lists, and a malformed item
  fails.** The test uses `go/doc/comment`, so a list is whatever gofmt
  renders as one. An item whose text does not start with a `.go` name is a
  failure rather than a skip, so a typo cannot hide a file. Several files
  may share one item (`a.go, b.go, c.go description`), which keeps a
  concept's family together.
- **`store`, `publish`, and `signet` moved to #2553.** Their 207 files would
  have pushed the PR past the 1,000-line review budget. #2553 adds them to
  the required set.

## Consequences

- A PR that adds, renames, or deletes a file in a package with a layout
  section fails `TestLayoutSectionsMatchFiles` until it updates the list.
  That is the intended behavior, not flakiness; the message names the
  package and file.
- The test checks names, not descriptions. A wrong description passes, so a
  reviewer still samples descriptions against the code.

## Tier A Bar

Four edited `doc.go` files sit in Tier A directories (`ward`, `export`,
`importer`, `verify`), so the unit is Tier A by path. The refute-first claim
to disprove was that the diff changes code there. Checked: `git diff -U0`
over those directories shows only `//` comment lines added or removed. No
stored authority or untrusted input is involved, so there is no re-check or
tamper test.

Revisit when gofmt's doc-comment list syntax changes, or when a package
outgrows a flat list and wants nested groups the parser does not read.
