# Earlier-Attempt Failure Supersession Records Prose, Not a Typed Fact

Work unit: #1127. When a later attempt of a production campaign is admitted,
the daemon now supersedes every open execution-failure recovery card an
earlier attempt left behind, appending which attempt superseded it and why.

## Chose a `Reason` Sentence Over an `attempt_supersession` Fact

The retired card records the superseding attempt and reattempt reason as a
sentence appended to its daemon-authored `Reason`, not as a typed field on
`AttentionItem`.

- A typed `attempt_supersession` fact (mirroring `ReadinessInvalidation`)
  would change `daemon/internal/domain` and `api/openapi.yaml`, making this a
  `kind:contract` unit that serializes behind the Wave 8 contract chain
  (#1134) and blocks its dependents (#1128, #1129).
- No client reads the superseding attempt as data today. The Mac client row
  already renders "This execution failure item is superseded." for any
  non-open execution-failure item (`AttentionDisplay.swift`), so prose in the
  reason is enough for the operator-facing history with no app change.
- The two existing supersession precedents (`supersedeBlockedHold`,
  `releaseProductionQuarantine`) also record no typed superseding-cause field;
  prose keeps this fix consistent with them.

Revisit when a client needs the superseding attempt as a field, or when
#1134's lifecycle partition wants to read it. Adding the fact then carries
this rule with it; it becomes a `kind:contract` change at that point.

## Chose an Id-Prefix Filter Over a Quarantine-Reason Match

The sweep supersedes open execution-failure items whose id begins with
`execution-failure-` (the prefix every real recovery card shares:
`productionFailureItem`, `productionDeliveryRefusalItem`, and the
specification failure/spec-revision cards). The #1127 plan proposed the
inverse: keep items whose `Reason` is not a quarantine notice, via
`productionQuarantineNoticeFor`.

Two facts found while grounding the code defeat both obvious alternatives and
favour the id prefix:

- A delivery-refusal card carries **nil** `ExecutionFailure` facts when its
  admission record is absent (`production_workflow.go`), so an
  `ExecutionFailure != nil` filter would wrongly skip a real recovery card.
- `productionQuarantineNoticeFor` does not recognise the
  specification-discussion or legacy-discussion quarantine prefixes, so the
  plan's reason match would wrongly sweep a discussion-marker notice open on
  the shared specification run.

The id prefix separates a recovery card from every synthetic marker notice
(whose ids carry a `-quarantined-` segment, never `execution-failure-`)
without depending on the optional facts or on tracking the full
quarantine-reason set. The contract's requirement is only "leave quarantine
notices alone," which this satisfies more completely than the proposed
mechanism.

Revisit when a recovery card is minted under an id that does not start with
`execution-failure-`, or when the quarantine notice id scheme changes.
