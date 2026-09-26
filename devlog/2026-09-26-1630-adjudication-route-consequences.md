# Say What Accepting a Finding-Adjudication Route Does

Issue #1551. Trust-boundary work: the store re-derives the
`finding_adjudication` recommendation from the stored adjudication artifact
and hides any stored recommendation that doesn't match. The issue body holds
the owner's contract, including why no API schema field carries the
consequence; this note records the implementation decisions under it.

## Decision

Chose one domain function, `FindingAdjudicatorRecommendationReason(entries)`,
over the old constant. The engine writes its output into the source record,
and the store's `ResolveAgentJudgment` calls it on the artifact it already
loads. The text is a pure function of the content-addressed entries (route
and finding ID only, never model prose), so the re-gate still compares two
derivations of the same authenticated input and a forged item gains nothing.

- **Legacy recommendations go dark, by choice.** An item opened before this
  change stores the old constant, which no longer matches the derivation, so
  the client hides its recommendation until the item is decided or
  superseded. The item stays decidable. Rejected: also accepting the legacy
  constant in the store, which would keep a second derivation alive in the
  trust gate for a transient population of open items.
- **The card's words come from the engine's behavior, and a test pins it.**
  `AdjudicationRoute.ParksRun` and `AcceptFindingAdjudication` restate what
  `executeFindingAdjudication` does: parking routes record nothing, a
  `dispute` returns before any disposition or remediation, and any
  `remediate` dispatches the remediator. `TestAcceptingParkingRouteRecordsNothing`
  pins the single-finding parking case. A change to the engine's route
  handling has to change these too.
- **Mixed cards say the parked finding is dropped, and stop there.** A
  card with `remediate` and a parking finding dispatches the remediator and
  leaves the parked finding without a disposition, so its context and
  recommendation say Park-routed findings get no disposition and aren't
  fixed by accepting. They claim nothing about readiness after the
  re-review: the code reads as keying off the next round, which would drop
  the finding, but no test exercises it. That drop contradicts
  `docs/plan.md` (a required incompatible finding parks the run) and is an
  engine defect outside this unit. Follow-up: #1576.
- **The item reason quotes no model text.** It renders as a daemon fact, so
  it carries route labels, finding IDs, the stored finding locations (the
  reviewer's, which the card already shows as daemon facts), and the run's
  declared paths. The Discuss reply, which is the adjudicator's own message,
  keeps the rationale.
- **Labels exist twice.** The daemon's `AdjudicationRouteLabel` and the app's
  `AttentionDisplay.label(_ route:)` carry the same strings, each pinned by
  its own test. Nothing checks them against each other.

Revisit when a client surface needs the consequence as structured
per-finding data (the issue's schema trigger), or when the engine gives a
parking route an effect of its own (#1550).
