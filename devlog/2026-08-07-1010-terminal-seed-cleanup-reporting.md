# Terminal Seed Cleanup Reporting

Issue #550 changes failure handling on the terminal seed-cleanup path (a
delete/cleanup path), so it carries the destructive-path refute-first record
even though the deletion behavior itself is unchanged. PR #592.

## Decision

Chose reporting the cleanup failure at `Warn` (operator-filterable, with the
invocation, run, and root-relative undeletable target: the daemon-derived name,
never the mutable seed-root pathname; see Refute-First Findings) over the prior bare `_ =`
discard at all nine call sites, and over returning the error to callers,
because a seed root that cannot be reaped otherwise accumulates full repository
checkouts with zero operational signal, while a terminal commit must stay
best-effort on cleanup and must not block on it.

Chose warning deduplication keyed by the failing error's identity (a
`seedMu`-guarded `seedCleanupWarned` map from invocation to last-warned error)
over the alternatives: attempting cleanup only once per invocation would drop
the retry that lets a transient failure converge; warning on every attempt
would let one undeletable checkout flood operator logs because `Inspect` and
`Collect` are idempotent terminal reads the engine calls repeatedly; and
keying by invocation alone would suppress a *different* sibling target's failure
after partial repair, because cleanup stops at the first undeletable name.
Keying by error identity keeps each distinct unresolved checkout reported once:
the same failing path is suppressed, a changed one warns anew, and a full
success clears the entry. The `RemoveAll` retry is always retained.

Chose suppressing the closed-driver sentinel (`errSeedCleanupAfterClose`) over
warning on it, because an in-flight terminal read that reaches cleanup after
`Driver.Close` niled the seed root is a benign shutdown race that defers the
seed to the next process, not a leaked checkout; pre-report it was discarded,
so warning would be a new false operational signal.

Chose skipping the permission-based failure test under root
(`os.Geteuid() == 0`) over adding a production `RemoveAll` fault-injection seam,
because a `chmod 0o500` fixture cannot hold as root (removal succeeds, the
warning never fires, the assertion spuriously fails), and adding a production
abstraction solely for testability is not warranted when CI runs unprivileged.
This matches the daemon's existing skip-as-root fixtures (`janitor_journal`,
`exec/fake`, `export`).

## Refute-First Findings

- **Confirmed:** the `d.seedFS.RemoveAll(name)` argument is byte-identical to
  before; only the error-wrap string and the discard-vs-report handling
  changed. Each of the nine swapped sites kept its position and control flow
  (report-only, no new returns), so what is deleted and when is untouched (the
  issue non-goal holds).
- **Confirmed and fixed (review, P2):** repeated idempotent `Inspect`/`Collect`
  re-attempted cleanup, so a persistent failure logged once per read; the old
  test missed it by counting records before its `Collect` call. Deduped per
  invocation; the test now drives repeated terminal reads and asserts a single
  warning.
- **Confirmed and fixed (review, P2):** the closed-driver sentinel would have
  reported an expected shutdown race as an undeletable checkout. Suppressed and
  covered by a root-independent test that forces the sentinel directly.
- **Confirmed and fixed (review, P2):** the first dedup keyed by invocation
  hid a sibling checkout's failure after partial repair (repair `runID`, then
  `runID-import` fails but is silenced). Re-keyed the dedup by error identity so
  each distinct unresolved checkout warns once; a test repairs the primary seed
  and asserts the sibling's distinct failure surfaces.
- **Confirmed and fixed (review, P2):** the failure warning joined the target
  onto `d.seedRoot`, re-introducing the mutable pathname the root-scoped design
  otherwise avoids. The removal goes through the fd-pinned `seedFS`, but a
  renamed-and-symlinked seed root (the adversarial case
  `TestTerminalSeedCleanupIsRootScopedAndPhaseGated/root_replacement` already
  covers) would make the joined path resolve to an unrelated outside checkout
  and misdirect operator remediation at a tree cleanup never touched. Now
  reports the root-relative name; `os.Root`'s own error is already root-relative,
  so the two reporting tests assert the warning names the daemon-derived target
  and never leaks `d.seedRoot`. Rejected keeping the absolute join: its
  usefulness in the common case does not justify a message that actively points
  at the wrong tree in the swap case, on a path whose whole purpose is accurate
  cleanup reporting.
- **Rejected by verification:** the phase-guard errors (invalid or nonterminal
  phase) still warn, because they indicate a caller bug rather than a benign
  condition; callers only invoke the reporter under a terminal-phase guard, so
  those paths are not shutdown races.
- **Deferred:** the same read-only-directory fixture class exists in
  pre-existing sibling tests this change does not touch
  (`driver_test.go:864,1393`; `recovery_test.go:328,430`); guarding those is
  tracked in #594.

Revisit when `cleanupTerminalSeed` grows a new benign not-a-failure error (the
suppression enumeration must extend to it), or the seed-root lifecycle changes
so that a cleanup after close can indicate a real leaked checkout rather than a
deferral.

Follow-up: #594.
