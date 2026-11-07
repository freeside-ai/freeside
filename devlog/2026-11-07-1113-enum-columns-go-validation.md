# New Enum Columns Validate in Go Only

Work unit: #2763. Mandatory note: an owner decision on migration policy
that would otherwise exist only in chat. The rule lives in
`daemon/README.md` (Adding a Migration); this note records why.

## Decision: No SQL CHECK List on a New Enum Column

The owner decided on 2026-11-07 that a new column holding a Go enum
carries no SQL `CHECK (... IN (...))` list. The domain type's `valid()`
predicate, called from the record's `Validate`, guards the column on
write and on decode,
and a store test round-trips every member of the enum's `AllX` slice.
Existing CHECK lists stay as they are.

Chose Go-only validation for new columns over keeping CHECK lists as a
second database-level guard:

- **A CHECK list turns every new member into a table rebuild.** SQLite
  can't alter a CHECK constraint, so admitting one value means copying
  the table. About 40 migration files carry CHECK IN lists and 13 contain
  table-copy statements; `0077_source_issue_closure_kind.sql` shows the
  deferred-foreign-key handling one rebuild took.
- **A rebuild pays the Tier A bar.** A migration that moves rows or
  rebuilds a table is Tier A (AGENTS.md, Tier A Paths), and two rebuilds
  of one table must be ordered with `starts-after`. Outside the enums
  that live in Tier A files, such as `domain/capability.go`, widening an
  enum is otherwise ordinary Tier B work; the CHECK list is what makes it
  Tier A.
- **The list guards nothing Go doesn't.** The daemon is the only writer
  of its database, and the database is trusted (AGENTS.md, Threat
  Model), so the rule makes the store's own check the guard: it
  validates the value on write and on decode, rejecting an unknown member
  and the zero value. A stored bit that grants authority is re-gated
  against current policy on read anyway, never trusted because a
  constraint admitted it.
- **The `AllX` slice is the one registration point.** A CHECK list is a
  second hand-kept copy of it that drifts until a write fails.
  The round-trip test ties the column to the slice instead.

Chose to leave existing lists alone over removing them in a sweep: each
removal is itself a table rebuild, so a sweep would pay the Tier A cost
the rule exists to avoid. Removing existing lists, even during a
rebuild done for another reason, stays out of scope until the owner
chooses it in a later unit.

## What Is Given Up

- **A SQL migration can write an invalid value.** Without the list, a
  migration's own `UPDATE` or `INSERT` could store a value the enum
  doesn't know. The store's decode then fails loudly on that row (Tier B:
  a bad row is a bug). Review of any migration that writes enum values
  stands in for the constraint.
- **Hand-written SQL against the database gets no guard.** Operators use
  `freesided inspect`, not database edits, so this is not a supported
  path.

## Revisit When

Revisit if a stored enum value ever reaches a decision before its decode
validates it, or if a second writer to the database appears.
