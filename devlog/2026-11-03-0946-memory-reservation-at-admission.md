# Memory Reservation at Admission (#1598)

#1598 bounds every ward container's memory by a per-instance budget (plan
§5.4, Machine Capacity). It changes contracts (a hold reason, an API field, a
migration) and scheduling policy, so AGENTS.md requires a note. The issue and
its plan comment hold the contract. This note records how the implementation
met it, where it departed, and the split the owner's size rule triggered.

## Decisions

- **The reservation follows the slot it rides with.** An execution or review
  reservation holds exactly while its admission holds its usage-pool slot
  (`executionSlotsHeld`, `reviewSlotHeld`), because both end at the same proof
  that every container is absent. A verification job takes no slot, so its
  reservation carries an explicit `released_at`, set when its room returns
  with absence proved or when a confirmed task stop proves it. Rejected:
  release columns for every kind. They would duplicate the slot predicates
  and could drift from them.
- **The gate runs inside the admission transaction, before the outbox mark.**
  `RecordExecutionAdmissionReserving` checks memory right after the pool gate.
  The execution slot predicate counts outbox `dispatching`, so a gate that ran
  after the mark would see its own launch as already holding memory.
- **An execution row is released only when its run has ended for good.** A
  dispatch that went back to pending keeps its row, because a replay's
  transaction rolls back on `errReplay` and would lose a fresh reservation.
  The replay passes the gate again as a holder.
- **The order is a durable wait row, not the hold.** Holds are observations,
  and a gate reads facts, so `launch_waits` records each memory-refused launch
  with its task's submission time. The gate counts live waits ahead of the
  launch and deletes waits whose run can no longer launch. A refusal for any
  other reason leaves the order, so a launch waiting on its pool never holds
  back another pool's work. Rejected: deriving the order from hold rows. Holds
  are paced and cleared for display, and a gate must not depend on them.
- **A wait lapses unless its launch keeps being refused.** A launch can stop
  being pursued while its run stays open: its review fails for good and
  escalates, its task is blocked, or its shadow arm is abandoned. The wait
  records its latest refusal, a pursued launch refreshes it once a minute,
  and the gate drops a wait not refreshed for five minutes. Rejected: leaving
  the order on each such path, which misses every path added later; and
  per-kind liveness facts, which would tie the gate to three subsystems'
  state. A lapse costs a wait only its place in the order, never the budget.
- **A held reservation keeps the overhead it reserved.** A replay after a
  runtime or daemon upgrade changed the overhead passes under the stored row,
  because its VM may already run at that size. A conflict there would fail
  every dispatch pass. A released row reserved again takes the current
  overhead.
- **The start refusal is composition-gated.** `dev` and `ephemeral` refuse to
  start without a budget only with `-driver claude` or the deputy. The other
  compositions launch no VM, so they run unbudgeted, or reserve with zero
  overhead (`RuntimeNone`) when given a budget. Rejected: an unconditional
  refusal, which would break every scenario and fake start.
- **A launch that can never fit gets a stop-only notice and is skipped.** The
  dispatch loop returns any unclassified error for the whole pass, so one
  oversized launch would have stalled every dispatch behind it. The launch
  now gets an `execution_failure` notice (prefix `launch-memory-refused-`)
  naming the size, the overhead, and the budget, or the unrecorded runtime
  version. It stays pending, and a later start whose budget fits admits it and
  retires the notice. The notice reuses the quarantine notice machinery, so
  it offers stop only. The card that also offers retry and discuss arrives
  with the second half.
- **Policy is checked against the budget where the daemon knows it.** The
  start checks `-manual-submission-config` projects and every default launch
  class against the bound. `freesided submit` and the deputy's prelaunch run
  outside the daemon and know no budget, so they check sizes without one.
  The daemon's launch-time refusal covers what they let through. Under an
  unrecorded runtime version, the start checks nothing and leaves that to the
  launch's notice, as the plan asks.

## Split

The owner's rule was to split along the recorded seam if the 1,500 to 2,000
line estimate proved low. The first half alone came to about 2,100 authored
lines, so it ships alone. The seam moved in one way. The grant order and the
never-fits refusal ship in this half: the order is part of the gate's
arithmetic (the hold's `ahead`), and the refusal had to keep one oversized
launch from stalling the pass. The second half,
[#2503](https://github.com/freeside-ai/freeside/issues/2503), is the
memory-kill card's `retry_larger_size` action at the next ladder size, with
`ExecutionFailureFacts.offered_size` and `ProductionAttempt.WriterMemoryMiB`.

## Refute-First Review

A fresh-context reviewer tried to refute the gate, the wait order, and the
hold read back from its columns (the delete path and the decoded-row trust
boundary). Outcomes:

- **Confirmed and fixed:** a stale wait blocking newer launches (the lease
  above); a replay under a changed overhead conflicting on every pass (held
  reservations keep their overhead); the writer's reservation computed before
  its admission was known to name an identity (now computed in the admitting
  transaction, for an identity only); a review under an unrecorded runtime
  raising no notice (the routed review waits under it; the shadow arm is
  abandoned); a machine_capacity hold left beside the notice (cleared).
- **Disproved by a check:** a second reservation for one launch, double
  counting, release before the ward's proof of absence, a cyclic order, and a
  partly present or mismatched hold row read back as valid. Each has a test
  or a reading recorded in the PR.
- **Allowed by decision:** launches in flight at the upgrade hold no
  reservation until they end, a one-time window no worse than the unbounded
  state before; and a verification at the head of the order can leave freed
  memory idle until its next retry, at most 30 seconds.
