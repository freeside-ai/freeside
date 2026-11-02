# A Usage Append Raises the Server Revision

Chose to treat a usage observation append as a client-visible write (#1145).
`AppendUsageObservations` moves from `InternalTx` to `WriteTx`, so a
`WriteInternal` callback can no longer call it, and each append that inserts a
row raises the server revision once. The run's `entity_version` stays put:
`billable_cost_so_far` is a projected fact read from other rows, like
`completion` and `superseded_by`, and it refreshes with `as_of_revision`. The
rule covers any projection over usage rows, so a later task rollup inherits it,
and plan §5.14 states it for every observation that feeds a projection.

## Rejected Options

- **A per-run `entity_version` or spend version with no revision bump.** A
  client cannot detect it. The revision (`/sync/revision`) is its only
  invalidation signal, and the store invariant forbids a non-bumping
  transaction from touching synchronized state.
- **Bounded staleness.** Plan §5.14 (revision 77) requires observation state to
  reach clients on its own, never waiting for unrelated activity. The churn
  fear behind this option does not hold either; see below.

## Premises Corrected at Planning

- Usage is appended once per collected result, closed-review poll, abandoned
  shadow review, or recorded export, not per token, so the extra revisions are
  bounded by those events. The closed-review poll and the abandoned shadow
  review have no other bump beside them, so each first insert there costs
  clients a bootstrap of its own; that is the point of the rule.
- Five of the seven append sites already ran inside `Write`, one more than the
  issue counted. Only `appendUsageObservations` and
  `abandonStartedShadowReview` bypassed it.

## Replays Do Not Raise the Revision

The implementation plan accepted one extra revision per replay. The code showed
otherwise: `TestUsageObservationsPersistAfterStageCollect` pins that a duplicate
collect leaves server state unchanged, and a collect repeats for the same
terminal, including when `authenticateProductionTerminal` re-reads one already
recorded. An unconditional bump would raise the revision on each repeat, and
every client would run a bootstrap for a figure that did not change.

Chose to roll the transaction back through `errReplay` when an append inserts
nothing, the idiom `engine.go` already documents for idempotent passes, over
loosening that test or adding a conditional bump to `Store.Write`. The five
sites that append inside a larger `Write` keep their behavior: `recordExportOnly`
still raises the revision on a replay, so plan §5.14 states the rule for an
append and leaves this rollback to the helper. An append with no agent binding
to attribute also inserts nothing and rolls back the same way.

Revisit when a lane records its result and its usage in one transaction, which
would retire the standalone append and this rollback.
