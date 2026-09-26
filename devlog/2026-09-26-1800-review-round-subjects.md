# Say What Each Review Round Reviewed

Issue #1552. Contract change: `RunReviewRound` gains two required, nullable
facts, `subject` (what produced the reviewed head) and `remediation` (the
remediation this round's findings started). The issue body holds the owner's
contract; this note records why the facts take this shape and how the daemon
proves them.

## Decision

Chose two projected facts on the review round over a new `RunMilestoneKind`
or a role field on `RunMilestone`. Both rejected options change stored domain
rows and need a migration, while every fact the client needs is already
recorded: the execution export binds each invocation to the head it produced,
the dispatch intent names the invocation's kind, the remediation intent binds
a remediator to the round, base, and head it answers, and the adjudication
item carries when it was decided. The client draws the adjudication step on
the milestone rail from `remediation.decided_at`.

- **The producer is proved by the export, not by the round's position.** The
  engine reviews exactly the head of the export its publication task names,
  so a head exported once in the run proves its producer. A head exported
  twice (a no-op remediation) or not at all (a successor after the base
  advances, or a legacy run) leaves `subject` null. Rejected: "round 1 is the
  implementation", which a legacy or successor round would make false.
- **Null `subject` never means implementation.** The client names only the
  head in that case. A wrong claim about which change a review judged is
  worse than a missing one, and this is the confusion the unit fixes.
- **The kind comes from the authenticated dispatch intent, never the ID.**
  The remediator runs in a stage with the implementer's name, and only its
  ID prefix differs; clients must not parse IDs, and the daemon classifies
  through `AuthenticateInvocationDispatchIntent` instead of the prefix.
- **A review-failure retry keeps its producer.** A retry reviews the same
  head the failed round did, and that head still has exactly one producer,
  so `subject` is `implementation` (or whatever produced it). The issue's
  acceptance list expected null here; that contradicts the field's own
  definition in the same contract (null only when the producer is unproven),
  so the definition won. Two rounds reading "implementation" at one head
  read as a repeat, which is what a retry is.
- **An intent bound to another round's head yields null, not an error.** The
  engine treats such an intent as not superseding the round
  (`remediationSupersedesReview`), and the projection applies the same rule.
  An intent under the round's remediation key with a different kind is
  inconsistent storage and fails closed.
- **A decoded intent's lineage is re-gated before either fact uses it.**
  Dispatch authentication binds only the intent's own invocation, run, stage
  and round. Its review invocation, base, head, finding IDs and adjudication
  digest are decoded claims, so both `subject.remediates_round` and
  `remediation` first check them against the named review record and
  adjudication, as `AuthenticateSuccessorProducer` does, and fail closed on a
  mismatch.
- **Workflow identities moved to domain.** `RemediationInvocationID`,
  `RemediationStageID`, and `ProductionFindingAdjudicationItemID` now live in
  `domain` beside `ProductionBlockedItemID`, and the engine's helpers call
  them, so signet re-derives keys without a second copy of the format.

Revisit when a workflow produces a reviewed head without an execution export
(the successor path would then need its own producer record), or when
delivered `remediation_invocation_requested` outbox rows start being pruned,
which would erase `remediation` for old runs.
