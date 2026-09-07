# Bounded Recovery Grace for a Stranded Dispatched Invocation

Work unit: #1181. Frees the execution slot a dispatched invocation holds
forever when its runtime object dies between the ward export and the
export-record commit (observed live in wave-7, ~7 hours of accepted-no-work
with nothing logged).

## Decision

Chose a **bounded recovery grace in the stage driver** over three alternatives.
Once recovery of an `exported`/`import_pending` intent has failed retryably for
longer than a grace period, the driver commits a failed outcome naming the last
error and the retained export directory. The `execution_outcomes` row frees the
identity's slot, and the existing production-lane failure path
(`collectTerminal` -> `recordProductionTerminal`) raises the `execution_failure`
card with `discuss` and `stop`. Each deferral is logged (warn while deferring,
error at abandonment). The engine also records a run-hold observation, and logs,
when it skips an attempt on a mutable admission-policy refusal, closing the
second silent path.

Rejected:

- **A separate reaper.** The collection pass already visits every attempt each
  pass; a second detector would duplicate it and would have to guess whether a
  runtime object is "provably absent."
- **Excluding the invocation from the capacity count.** The count is derived
  from durable records by design, and excluding a still-recoverable execution
  would let a second one start against the same identity.
- **An operator abandon command.** The daemon has no operator command surface
  today, and none is needed once the state ends on its own.

The `running`-phase handoff window keeps its unbounded retry: ward owns teardown
recovery there, and the driver still refuses to terminalize an intent whose
writer teardown ward has not proven. That window is #385 (`exclusive-with`).

## Abandoned-Outcome Usage Is Authenticated, Never Re-Extracted

The abandoned outcome reports the provider usage the invocation incurred, but
only usage that a recovery pass already authenticated. The import-failure path
extracts usage after `authenticateReleasedExport` and `validateReleasedExport`
have passed, and stashes it in the deferral; abandonment commits that stored
value. Rejected re-extracting from the decoded export at abandonment: the intent
is a reconstruction boundary, and a retryable authentication failure reaches the
abandonment path with the export unverified, so re-reading its evidence would
let a corrupted intent select an attacker-chosen transcript and inject
measurements into durable cost telemetry. Absent authenticated usage the failed
outcome carries none; the freed slot and the operator's `execution_failure` card
do not depend on it.

## The Grace Is in Memory

`Config.RecoveryGrace` (default 30 minutes) is tracked in a per-invocation map,
not a durable timestamp. A daemon restart restarts the clock, so a persistent
fault takes at most one more grace to terminalize. Chose this over a durable
timestamp because durability would touch the intent file format and its golden
for no material gain: a persistent fault re-accumulates within one grace either
way, and a transient fault that clears is exactly what the grace is meant to
tolerate. If review wants durability, that is a scope change to raise, not a
silent widening.

The 30-minute default is a judgment call: long enough to outlast a transient
git, disk, or network fault across many reconcile passes; short enough that an
identity's only slot is not lost for a working day.

## Trade Accepted

A fixable environment fault (disk full, git remote down for an hour) now ends
the attempt as `failed` after the grace instead of waiting indefinitely. The
operator gets the card, the export directory stays on disk, and a retry mints a
new attempt. That trade is the point of the unit.

## Revisit When

- The reconcile cadence or the `import_pending` import path changes materially.
- #385 lands a durable retry record this could share (then reconsider the
  in-memory grace).
- Operators report the 30-minute default is too short for their environment
  faults or too long for slot recovery.
