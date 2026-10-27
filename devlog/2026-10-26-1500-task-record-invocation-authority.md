# Task Record: Show an Invocation's Authenticated Terminal

Work unit: #2351. Mandatory note: decoded milestone rows now decide a status
the record prints (a returned-object trust boundary).

## Decision: Adopt #785's Authority Rule in the Record

`freesided inspect task` printed each invocation's observation row as stored.
That row is a paced last-look cache: the engine's terminal path appends a
`terminal_recorded` milestone without touching it, so a published run could
show its implementer `running` indefinitely, which misled the Wave 8 exit
run's operator (#1613). Chose to overlay each invocation's authenticated
terminal fact on its row, keeping the row's `observed_at`, exactly as the
app's run reads do (`devlog/2026-08-14-2322-run-observation-authority.md`).
Writing a terminal observation row on the engine's terminal path stays
rejected for #785's reasons: it would claim a runtime report that never
happened, and it would not heal rows already frozen.

## Decision: Authenticate Only the Status-Bearing Milestones

Chose a new `signet.AuthoritativeInvocationStatuses` that runs the per-kind
checks for `execution_export_recorded`, `execution_outcome_recorded`, and
`terminal_recorded`, the only kinds the overlay reads, and then overlays.
The checks moved into one helper, `authenticateStatusMilestone`, that the
full observation pass also calls, so the two paths can't drift apart.

Rejected:

- **Overlay on raw milestones.** A forged or orphaned milestone would decide
  a printed status, the failure #785's authentication exists to stop.
- **The full `authenticateRunObservation`.** It also checks attention items,
  the hold, and every other milestone kind, so one damaged item would blank
  every run's hold and invocations. The record keeps those faults in the
  sections that own them.

## Decision: Fail the Observation Section Closed

A status-bearing milestone that fails authentication fails the run's
`observation` section: `hold` and `invocations` print null with an
`integrity` error, and printing either status is refused. The conclusion
still reads the store's observation, so it keeps matching `freesided
follow`.

An unreadable run now fails `observation` too, not only `conclusion`. The
store re-gates every run decode against its task and attempt rows, and there
is no ungated run read to authenticate against, so printing the raw rows
there would bring the stale status back. This revised
`TestTaskRecordReportsUnreadableTask`, which pinned invocations reading past
a broken task row; the issue's acceptance had listed every existing record
test as unchanged, and the plan's own step for an unreadable run implied
this one change.

## Refute-First Findings

- **Refactor equivalence: disproved drift.** A scratch differential ran the
  base `authenticateRunObservation` and the refactored one over every read in
  the signet run-observation corpus (55 reads, baselines and forges) and
  compared error text, `ErrParentKeyMismatch`, and `ErrNotFound`: no
  difference.
- **Status pass weaker than the full pass: disproved.** In the same run, all
  seven corpus forges of the three status kinds fail the new function, and it
  never fails a run the full pass accepts; its overlay equals the full pass's
  overlay on every accepted run.
- **A milestone for another run's invocation prints a status: disproved.**
  Every status kind binds the invocation's admission to this run's attempt;
  an invocation that is not an attempt of the run has no binding and fails.
- **A nil milestone pointer panics the overlay: disproved.** The store
  validates every milestone it returns, and validation requires the
  invocation on every kind, the outcome on outcome milestones, and the
  terminal on terminal ones; a terminal can't be `pending` or `running`.
- **Raw and overlaid observations give different conclusions: disproved.**
  `ConcludeRun` reads invocation statuses only for a run with no milestones,
  where the overlay changes nothing.
- **A failed status check blanks the hold, which it never checked: allowed.**
  The hold and the invocations are one section, and the issue's acceptance
  requires both null; `runs_held` already counts only holds that were read.
- **Stall notices lose an owner when `observation` fails: allowed.** Their
  attribution falls back to admissions, which every writer has, so a notice
  goes missing only when the admissions read fails too, and the record names
  both failures.

Revisit when an invocation can carry two authenticated terminal facts that
disagree (#785's trigger), or the engine's terminal path starts writing
observation rows.
