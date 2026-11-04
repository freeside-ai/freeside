# Package Rules Name Their Exceptions

Decision record for #573. Two package comments stated rules the code
contradicts. The `exec` package comment said the package holds "no
provider code, no I/O", and the store's `entities.go` said "no SQL is ever
assembled at runtime". Both comments now keep their rule and name, by file,
where it doesn't hold and why.

## Decisions

- **Chose to amend the `exec` charter over moving the code out.** The owner
  delegated the choice to the planning parent session, which decided on
  2026-11-04. `ComposeCodexReviewInstructions` (`review_instruction.go`)
  lives in `exec` because the review contract carries its
  `ReviewInstructionBinding`, and the engine, the ward, and `freesided`
  preflight must produce the same bytes and digest. `materialize.go` lives
  there because `MaterializingStageDriver` is the one production start
  seam that verifies input digests before a real driver commits an
  invocation intent. Rejected: moving either one. Moving an exported
  function out of a shared contract package is a `kind:contract` change,
  which doesn't belong in a doc-only unit; the charter now says so.
- **Gave `usage_claude.go` the same treatment.** `ExtractClaudeUsage` parses
  Claude's transcript usage envelope for both `exec/claude` and the ward's
  review sources. It arrived on 2026-08-29, after the 2026-08-06 audit
  behind #573, so the owner's decision didn't name it; planning applied the
  same rule.
- **Kept "no persistence" in the charter.** No file in `exec` persists
  anything, so the rule is still true as stated.
- **Named both store sites that build SQL from runtime values, over the
  issue's "one exception".** Restore copies every table generically.
  SQLite can't bind an identifier, so the copy quotes table names read from
  `sqlite_master` after `safeTableName` checks them, and drops guard
  triggers by constant names. The fresh-context review found the second
  site: `store.go`'s `openWritableDB` and `openReadOnlyDB` format the
  configured busy timeout as an integer into a `busy_timeout` DSN pragma,
  which modernc's `newConn` runs as `pragma ...` on each connection. The
  issue's audit missed it because it searched `gosec` suppressions, which
  don't flag a DSN string. Rejected: narrowing the rule to "statements the
  store executes", which would hide a site a reader should know about.
  Every other concatenation in the store joins `const` strings, which Go
  folds at compile time.
- **Classified the unit Tier A by path, though it changes no behavior.**
  `daemon/internal/store/entities.go` is on the AGENTS.md Tier A list, so
  the unit kept the refute-first pass, a fresh-context review, and this
  note.

## Refute-First Findings

Each claim was checked against the code at `0267e409`:

- **The diff changes only comments: confirmed.** Filtering the daemon diff
  for added or removed lines that aren't comments or blank prints nothing.
- **`restore.go` is the only store file building SQL from runtime values:
  disproved by review.** `store.go`'s busy-timeout pragma is a second site,
  now named in the comment. The only `//nolint:gosec` suppressions over
  runtime SQL sit in `restore.go` (`pairing.go`'s covers constant SQL).
  Every other concatenation next to a SQL literal uses a `const` operand,
  and the placeholder list in `restore.go`'s insert is built from the
  column count, not from data.
- **The `DROP TRIGGER` names are constants: confirmed.**
  `outboxPublicationInsertTrigger` is a `const`, and
  `appendOnlyDeleteGuards` lists only `const` names.
- **Each `exec` exception's reason holds: confirmed.** The callers named
  for `ComposeCodexReviewInstructions` and `ExtractClaudeUsage` are the
  only non-test callers, and `MaterializingStageDriver`'s own comment
  calls it the one production start seam.
- **`materialize.go` also holds provider code: allowed by decision.**
  `EffectiveVendorInstructionDelivery` gives a Claude-only default
  delivery. It resolves a default for a contract field over domain enums,
  the same for materialization and durable reconstruction, and parses no
  provider format, so the charter treats it as contract logic rather than a
  fourth exception.

Revisit when another file in `exec` takes on provider code or I/O, or a
third store site builds SQL from runtime values: the comment must then
name it or the code must move.
