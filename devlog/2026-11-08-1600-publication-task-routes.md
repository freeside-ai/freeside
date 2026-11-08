# A Publication Task's Route Is a Named Enum

#2855 names the four paths `reconcileTask` sends a production publication
task down. `productionPublicationRoute` lists them, `route()` picks one from
the task's fields alone, and `reconcileTask` switches on it at the three
points where its `if` chain used to branch. Behavior is unchanged. This is
link 1 of the publication-states chain the plan comment on #2761 proposed;
the source note is `2026-11-07-1213-publication-file-split.md`.

## Decision

- **Four routes, first match wins:** in-place re-entry
  (`reconcileReentryTask`), retirement of a legacy unpublished-head
  re-entry (`retireReentryTask`), remediation continuation
  (`startPublicationContinuation`), and forward. The order is the old `if`
  order: in-place re-entry before retirement, and both before
  continuation. A v5 or v6 successor with no producing invocation re-enters
  in place (no builder makes one today, but `validateReentry` doesn't
  check the version, so a stored row can), and a continuation riding a
  re-entry takes the re-entry's route.
- **Kept two checks as checks, not routes.** The completed-publication
  check reads the store and runs in front of every route. Scope-conflict
  recovery reads the producer's agent claims and runs in front of every
  route but in-place re-entry; an answered scope conflict returns nothing
  and the task goes on to its route. A route is what the task's
  fields alone decide. Rejected: the issue's first draft, which listed both
  as routes. Folding a store read into `route()` would make it impure and
  hide the fall-through.
- **Kept remediation out of the routes.** `binding.remediation` comes from
  the binding, not the task, and branches inside the forward path.
- **Chose three switches at the original dispatch points over one switch
  at the top.** Each side effect keeps its place: the completed-publication
  refusal still runs before in-place re-entry, the scope-conflict
  quarantine release still runs before retirement, and the checkout and
  import still run before a continuation starts. One top-level switch would
  need the checkout and import extracted into a function first, which
  #2858 owns, and an extraction would also move the hold exits that
  `hold_exit_enumeration_test.go` keys by `reconcileTask`'s name and file.
- **Each switch has no `default`,** so the `exhaustive` linter makes a new
  route name its arm at all three points. An arm for a route an earlier
  switch already returned on returns a plain internal error naming the
  route. Rejected: wrapping it in `domain.ErrParentKeyMismatch`, which
  callers read as a data mismatch and quarantine; an unreachable arm is a
  programming error, not bad data.
- **Computed the route once per pass.** Between the first and last
  switches, the only write to the task is scope-conflict recovery setting
  `task.scopeDecision`, which `route()` doesn't read.

## Refute-First Findings

Tier A by path (`production_publication.go`,
`production_publication_task.go`). Each claim below tried to show the
change wrong.

- **"Some task now takes a different path."** Disproved by check: an
  exhaustive comparison of `route()` against the old `if` chain over every
  combination of successor presence, nine versions (the seven real ones,
  empty, and an unknown one), every origin plus empty and an unknown one,
  reentry set or not, producer set or not, and continuation set or not
  (1,296 tasks) found no disagreement.
- **"The table test wouldn't notice a reordering."** Disproved by check:
  swapping the first two `route()` cases fails the two v5 and v6 rows
  without a producer, and dropping the continuation case fails the
  continuation row.
- **"A new route could slip past a dispatch point."** Disproved by check:
  with a fifth constant added, daemon lint (golangci-lint v2.15.0, the CI
  pin) reports `exhaustive` at all three switches.
- **"A side effect moved relative to the scope-conflict quarantine
  release, or a continuation skips the completed-publication refusal."**
  Disproved by reading: the completed-publication check is untouched and
  still tests the fields directly, and each switch sits where its `if`
  was.
- **"Scope-conflict recovery could change the route mid-pass."** Disproved
  by reading: it writes only `task.scopeDecision`.
- **"The docs say every route passes scope-conflict recovery."** Confirmed
  by the fresh-context review and fixed: in-place re-entry dispatches
  before it, and `route()`'s doc comment now says so.

## Revisit When

- Scope-conflict recovery, or anything between the first and last switch,
  starts writing `Successor`, `continuation`, or `ProducingInvocationID`.
  Then computing the route once is wrong.
- #2858 extracts the checkout and import. The three switches may then
  collapse into one.
