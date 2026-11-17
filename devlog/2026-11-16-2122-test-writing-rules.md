# Daemon Test-Writing Rules From the 2026-11-16 Audit

Work unit: #3256 (tracker #3267, Daemon test suite reduction). The rules
live in `daemon/README.md` (Testing conventions), with one bullet each for
test shapes and test removal in AGENTS.md (Daemon Coding Conventions).

## Evidence

The owner-directed, read-only daemon test audit of 2026-11-16 ran at
`754d60844`, with timings from CI run 43883005458. Between 2026-11-01 and
2026-11-16, 1,830 daemon tests were added. Seven shapes recurred, ordered by
cost:

1. A test per add-only migration (thirteen were written), already covered by
   the migration snapshot test, the drop-declaration test, the stored-state
   corpus, and every template store.
2. A new store test that migrates from empty when only migration tests need
   to.
3. A "survives reopen" or "survives process kill" test per record type,
   proving SQLite's durability once per table.
4. A data-rewrite migration test that never retires, which the baseline
   squash (#3252, `devlog/2026-11-16-1313-migration-baseline-squash.md`)
   gives an end.
5. One refusal asserted at two or three layers.
6. Tests of test doubles and harnesses beyond the shared contract suite.
7. Kill matrices and loops over every enum member where a few rows reach
   every distinct state.

The sibling units #3253, #3252, and #3255 cut the instances the audit chose
to remove or trim; any left drain under the ratchet. Each carries its own
test list and justifications; the audit's full ledger was not committed.

## Decisions

- **Write rules for new tests, beside the removals.** Removing tests without
  changing what gets written does not hold at 1,830 tests in fifteen days.
  Chose written rules over relying on review alone, so a reviewer has a
  stated bar to cite.
- **A ratchet with no mechanical check.** The rules bind new and
  substantially revised tests, like the other Daemon Coding Conventions; an
  existing instance drains through its own unit. Rejected a lint or script
  check now: each shape needs judgment to tell from its exception (an
  add-only migration from one that rewrites rows, a per-table reopen test
  from one of state the store computes on open, a redundant refusal from a
  Tier A gate's own test), so a check would either miss the shape or flag
  the exceptions.
- **Every rule names its exception.** The exceptions keep what other rules
  require: a Tier A gate's tests at its own layer (Assurance Tiers), the
  durable transition matrix's two crash sides per boundary, the per-member
  round-trip for an enum column with no CHECK list (Adding a Migration), the
  raw-open tests the Template store paragraph lists, and the tests of shared
  helpers whose failure would let other tests pass silently.
- **The owner's removal rule is recorded with the rules.** A test may go
  when a named remaining test still catches the failure
  (`superseded by <test>`), or when missing it is acceptable for a stated
  reason (`accepted: <reason>`). Removing a Tier A gate test stays Tier A
  work. Recording it here lets later cleanups use the same bar as this
  audit.

Revisit when a rule proves checkable, for example when a reviewer can
state it as a pattern with no exceptions; then propose a mechanical check
as its own unit.
