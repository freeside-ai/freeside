# Feedback After a Handoff Conflict: Admit at the Sealed Tip the Request Names

Issue #2699 closes a Known Limit of
`2026-11-05-2336-handoff-conflict-remediation.md` (#2646). After a v7
`handoff_conflict` cycle publishes a head written on the sealed tip, a
return to the agent on that item was admitted at the operator's configured
base while its input's base and candidate patch came from the tip. The work
is Tier A: it decides the base a writer is admitted at.

## Decisions

- **The request selects the base; a sealed v7 authority proves it.** An
  operator-feedback invocation, or a remediation not already under a v7
  authority, is admitted at the base its request names when, and only when,
  a `handoff_conflict` authority in the run's successor chain names that
  same tip. For feedback the request's base is the returned published
  candidate's, through `FeedbackPublicationParent`'s retry and answer
  ancestry; for a remediation it is the request's own base. The chain is
  read through `PublicationSuccessorChain`, which re-proves every authority
  against its sealed handoff check, so the request never admits a commit by
  itself. Repository, repository ID, and branch stay the run's production
  admission's, and a derived base that differs fails closed with
  `ErrRepositoryIdentityMismatch`, as #2646 does for its own remediations.
  Code: `handoffConflictTipAdmission` in `daemon/internal/engine/admission.go`.
- **The cycle's remediations are included, not only the feedback.** A
  feedback cycle's remediation request names `admittedBaseSHA()`, which is
  the feedback's replay base, the tip. The remediation export check in
  `production_publication.go` requires the remediation's admitted base to
  equal it. Admitting the feedback at the tip alone would move the failure
  to the first remediation of that cycle.
- **Engine-package tests were not written; the integration harness carries
  every case.** A sealed v7 authority exists only through the store's gate,
  which needs the handoff check, its workflow audit, and the prior review.
  The store's fixtures for them are package-internal, and the integration
  harness builds them through the real flow. The retry, the
  repository-identity refusal, and the derived-base negative are
  integration tests for that reason.

## Corrected Impact

The #2646 note says the feedback's patch "does not fit the feedback's
workspace", which reads as degraded output. The cycle actually fails
closed: the export's observed base is always the admitted base, and the
feedback successor's gate requires that base to equal the returned
candidate's, so the successor never seals and the feedback's head never
publishes. `TestReturnToAgentAfterAHandoffConflictPublishes` reproduced it
on the tip before the fix: recording the feedback's export returned
`ErrParentKeyMismatch`.

## Rejected Options

- **Always admit at the base the request names.** Simpler, and right
  whenever a candidate's base differs from the configured one. Rejected
  because it would also change what happens after an operator restarts the
  daemon with a different `-base-sha`, which #2699 leaves alone, and it
  would make the request, an engine-written record but not an authority,
  the sole source of a writer's base. The sealed-authority match keeps the
  change confined to tips a handoff conflict proved.

## Refute-First Findings

A fresh-context review tried to admit a commit no v7 authority names and to
break attempt determinism, and found no Tier A break. Outcomes:

- **A specification discussion can share the feedback prefix.** Confirmed:
  its ID is `inv-` plus a client command ID. The rule now also requires the
  operator-feedback production stage, as `productionDeliveryInvocation`
  does. The same fix covers a feedback invocation with no intent, which
  engine admission tests construct: it names no candidate, as a remediation
  without an intent already does.
- **The negative test could not detect a skipped chain check.** Confirmed:
  its candidate's base equalled the derived base. Replaced by a test whose
  engine restarts on a newer base, where skipping the check moves the
  feedback; checked by removing the check and watching the test fail.
- **An answer to a remediation's question keeps the derived base.**
  Allowed: such an answer has no published parent, so
  `FeedbackPublicationParent` finds none and the contract's rule does not
  reach it. Whether that path needs the tip is outside #2699.
- **A maintenance task's repair tip is overridden by the sealed tip.**
  Allowed: #2646's own remediations already do so, with hydration off.
- **Disproved:** an earlier cycle's remediation cannot name the tip, since
  a conflict means the tip differs from that cycle's base; a parent from
  another run fails closed; a run with no successors reads an empty chain;
  the feedback's own successor is unsealed at admission; and an attempt is
  admitted once, from an immutable request, while a v7 seal never goes
  away, so restarts derive the same base.

## Revisit When

A cycle other than a return to the agent or its remediations starts from a
candidate written on a sealed tip, or a writer's base becomes a per-run
record. Either would replace this request-plus-authority derivation.
