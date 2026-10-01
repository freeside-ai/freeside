# Raise a Notice for Work an Admission Refusal Keeps Holding

Work unit #766. A refusal that doesn't clear on its own (a reconfigured
backend, a re-proof that keeps failing) held a run indefinitely with no
operator signal. The engine now raises a `work_held` notice once such a hold
is 15 minutes old and resolves it when the hold ends.

## Chose a `system_health` Advisory on the Task Over a `blocked` Item

The notice is an existing `system_health` item with advisory posture, a
`task` subject, the `acknowledge` action, and the diagnostic code
`work_held`. Rejected:

- **A `blocked` item.** It needs a new wait kind and a store gate, both
  shared-contract changes, and it would say the operator must decide
  something. Nothing waits on a decision: the engine keeps retrying.
- **A run subject.** Run supervision reads the run-scoped open-item list
  and treats any open item with a requested decision as
  `attention_required`, which ends a supervised run. A self-resolving
  advisory must not do that. The task subject keeps the item out of that
  list; the reason text names the run. The owner's scope note on #766
  (2026-09-07) chose the task subject for the same held-work-is-a-task
  reason.
- **A blocking posture.** The hold is already the consequence of a refused
  admission. A blocking item would refuse every other run too.

The diagnostic code is a pattern-checked string, not an enum, so `work_held`
changes no contract. The stall notice added `invocation_stalled` the same
way.

## Chose to Read the Hold Row Back for the Notice Only

Until now the engine wrote `run_hold_observations` and never read it. The
row is the only record of when the current cause began (`first_observed_at`
survives while the cause is unchanged), so the notice reads it. Rejected: an
in-memory "held since" in the engine, which a daemon restart would reset and
so delay or repeat the notice.

The read is confined to raising and resolving the notice. No workflow,
recovery, or publication decision reads the hold. The row stays untrusted
projection: an unreadable row leaves an open notice standing and logs a
warning, and never fails a reconcile pass.

## Chose One Raise Point and One Resolve Sweep

- **Raise** runs inside `observeRunHold`'s transaction, which dispatch and
  both collectors already share. No path has its own copy.
- **Resolve** is a sweep in `Reconcile` after dispatch and collect. A hold
  ends in several places (the collectors' clear, any milestone, a different
  cause replacing the row), so hooking each one would scatter the logic.
- **Identity** is the run plus the hold's start. One span has one item ID,
  which is what makes the notice raise once across passes and restarts.

## Finding: A Stopped Task Leaves Its Hold Row Behind

Stopping a task fences its run, so the engine stops retrying, but nothing
clears the hold row. A test showed the notice would then stay open with no
way to close it, because `acknowledge` records the decision and leaves a
`system_health` item open. The sweep therefore also resolves a notice whose
task is stopped, and the raise skips a stopped task. The issue contract's
Clear criterion doesn't list this case; it was added because the alternative
is a notice that says "Freeside keeps retrying" about work that isn't.

The stale hold row itself is unchanged here. What a hold records is #435's
territory and a non-goal of this unit.

## Refute Pass

An independent reviewer tried to break the change before commit. It found no
blocking defect. Outcomes:

- **Confirmed, fixed:** the tests didn't cover the raise-side stopped-task
  skip, the records variant of the run-scoped item list, or the item's
  status after an acknowledgement. All three are now asserted.
- **Declined: a hold row that stops being refreshed keeps its notice open.**
  Traced to one case: during an operator stop, dispatch rewrites the hold to
  `operation_stopped` for production, specification, discussion,
  remediation, and feedback intents (which resolves the notice) but not for
  the plain invocation kind. The stale notice is bounded by the stop and is
  accurate again on resume. Resolving on a stale `last_observed_at` was
  rejected: the same span could never raise again after the resume.
- **Declined: the sweep can fail a reconcile pass on a corrupt attention
  row.** `ListOpenAttentionItems` fails closed on column divergence. The
  unattended admission gate already fails every pass on the same row, and a
  corrupt item table is a data-integrity failure that should be loud.
- **Disproved by trace:** no refusal-class hold is written outside
  `observeRunHold`; the ID scheme has no reachable collision, because a
  second span starts at least 15 minutes after the first one's start.
- **Allowed by decision:** `impairs: unattended_admission` on an advisory
  item is a new pairing, and the app shows the field. It is accurate: the
  refusal is what keeps this work from being admitted.

## Accepted Limits

- **15 minutes is a fixed constant**, three times the stall interval. The
  owner can change it; there is no flag or policy key.
- **Alternating causes never alert.** A run that flips between two refusal
  reasons faster than the threshold restarts its span each time.
- **Dispatch stops at the first refusal**, so only that run gets a hold and
  a notice. These refusals usually apply to the whole daemon, so one notice
  alerts.
- **The notice is not a push.** It reaches the operator through attention
  sync like every other item.

Revisit when a contract unit adds a typed fact for a held run (#1643 and
#980 work nearby): carry the run, reason, and start as facts instead of
reason text. Revisit the threshold when exit runs show real refusals that
clear on their own after more than 15 minutes.
