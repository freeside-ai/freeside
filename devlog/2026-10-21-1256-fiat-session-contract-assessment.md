# The Creating Or Fiat Session Records Contract Assessments

Chose to have the session that creates a `kind:contract` unit, or that
receives its `Plan #N` or `Handle #N`, record its missing contract
assessments, over keeping a separate spine step with planning failing
closed. The spine step has no trigger when fiat follows creation. On
2026-10-21 #2115 was created and given `Plan` fiat while #2111 held an
active claim; the pair had no assessment, so no planning reservation could
be written, and the planner could not edit #2111's Dependencies field. The
planner stopped, and nothing would have restarted it. (#2119; the practice
already existed in the other direction, when an owner-assigned session
reclassified #2111 and recorded its own assessments against #2071, #2073,
and #397.)

The changed assumption: every contract unit reaches a session through a
spine sweep. Owner fiat on a freshly created unit skips the sweep, so the
spine's assessment never ran.

What changes is who writes the record, not the ordering:

- **The assessor** is the spine, or a session that created one of the two
  units or holds owner fiat for one of them. Such a session fills the
  record's assessor slot with `fiat #N` (N the unit it was assigned) or
  `created #N` (N the unit it created). Any fiat session for the unit may
  assess, not only the first, so a `Handle #N` after `Plan #N`, or a
  planner after its own scope edit, can reassess instead of stalling.
- **The fiat session's set** is every contract unit holding an active claim
  or planning reservation. The spine keeps its wider set (every scheduled
  or active contract unit) when it schedules a unit.
- **A planner writes the lines before its reservation.** A first verdict of
  any kind on an unassessed pair restricts nothing, because the pair
  already serialized; recording independence only relaxes. So that write
  needs neither a reservation nor the `exclusive-with` edit protocol, which
  still governs replacing an existing independence record. An
  implementation session's allowed mutations gain the same lines.

Unchanged: the cap of two, the serialization of unassessed pairs, the
record's form, and the two authorization doors. Fiat still never bypasses
contract ordering.

Two further rules came from a fresh-context review of the first draft. Both
are the agent's choices, open to the owner's review:

- **A recorded `starts-after` stops blocking planning in both directions.**
  The issue's planner proposed that a partner this unit starts after leaves
  the planning-reservation conflict set, so #2115 can be planned while
  #2111 is claimed. Dropped one way only, it inverts priority: a
  dependent's reservation could force its prerequisite's planner to
  release, or block the prerequisite's claim. So the drop is symmetric, and
  a reservation does not block a claim on a unit it is recorded to start
  after. This touches the claim gate the issue listed as a non-goal, but
  only for a reservation by a unit that cannot start before the claimant
  merges; a claim still blocks on every claimed partner without an
  independence record. A partner recorded `exclusive-with`, or still
  unassessed, stays in the set and blocks the reservation.
- **No independence against a partner mid-planning.** A partner holding a
  planning reservation may be rewriting its scope. An independence record
  written after its scope edit escapes the scope-edit withdrawal and could
  let two conflicting units run together. So an assessor re-reads the
  partner just before writing, and against a reserved partner records only
  `starts-after` or `exclusive-with`, which order or separate the pair and
  stay safe on a stale scope.
- **A claimed partner never becomes the dependent.** A first verdict needs
  no edit protocol only because it restricts nothing. Recording a claimed
  partner as starting after the unit being placed would restrict work
  already under way, which Session Start step 4 never rechecks. So against
  a claimed partner the new unit starts after it, is `exclusive-with` it,
  or is independent of it.

Rejected: a spine step with planning failing closed. It keeps the spine as
the only assessor but stalls every create-then-fiat unit until someone
notices and runs the spine by hand. The spine is a role a session takes,
not a running service, so "the spine will do it" names no actor.

Revisit when the spine role becomes a running service or an automated
assessor exists.
