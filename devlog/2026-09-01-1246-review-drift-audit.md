# Review Drift Audit

Chose a daemon-run drift audit inside the review loop over the owner's manual
check because the landed loop can pass every gate one finding at a time and
still end over-built (#1054; plan revision 45, Section 7). The contract is
#1048; the behaviour follows in #1049 through #1053.

## Decisions

Decider: user, 2026-09-01, in chat. This note carries the reasoning; the plan
carries the names, defaults, and routing.

1. **Reuse `review_diminishing_returns` with new causes, not a new
   `AttentionType`.** The human's decision has the same shape either way:
   finish now, apply the batch and finish, or continue under policy. On a
   drift card carrying a validated reversal list, continuing runs the
   proposed simplification round, which is how the human chooses "simplify"
   with the landed actions; on any other drift card it runs an ordinary
   review round. A new type would
   grow the attention vocabulary (#936) and add a card for no new decision.
   The new causes are `growth_without_blockers` (the deterministic floor) and
   `drift_audit` (a parked model verdict). The card payload carries the
   verdict and the reversal list, so one model cause is enough.
2. **Ship the deterministic floor before the model-backed audit.** Diff
   metrics and the growth-without-blockers stop rule cost nothing to run, and
   they measure how often the audit would fire before it costs anything.
   Rejected: the model pass first, which spends inference before the firing
   rate is known.
3. **Auto-route `over_hardened` into one simplification round; park a second
   verdict.** The existing ceilings hold: a critical or high finding's fix is
   never undone without a second adjudication or an AttentionItem, and low
   confidence parks. `drift_audit_route` flips between `auto` and `park`
   without a code change. Rejected: park-by-default, which turns every
   verdict into an interruption and defeats the wave-8 charter of running
   unattended.

## Choices the Plan Text Makes

The issue left these open. The revision records them so the owner can veto
them before #1048 implements them.

- **Growth knob name:** `review.drift_growth_streak_before_attention`,
  mirroring `low_value_streak_before_attention`; off when unset.
- **Two causes, not one:** `growth_without_blockers` and `drift_audit`. #1048
  says "a new `ReviewDiminishingCause` member" (singular); the difference is
  surfaced on #1048.
- **`drift_audit_route` default `auto`:** the plan names auto-routing as the
  design and the flag as the way to flip it. #1053's evidence decides the
  shipped default.
- **Confidence threshold:** the audit reuses the adjudication confidence
  threshold (bounded below at `medium`), so `low` always parks and no second
  threshold enters policy.
- **Principle 4 changes:** one clause names the whole-change judgment beside
  yield.
- **One bound for routing, the verdict count:** only the run's first
  `over_hardened` verdict can auto-route, and a parked first verdict still
  counts. This satisfies both owner statements (one automatic simplification
  round; a second verdict parks) without two overlapping rules.
- **Convergence before audit within a round:** when any deterministic stop
  cause fires, including the new growth rule, the audit does not run that
  round. The card's cause is never ambiguous, and no verdict can route past a
  stop policy already required.
- **`continue_under_policy` runs the proposed simplification round only when
  the card carries a validated reversal list.** The Section 4 action set is
  unchanged. A `stuck` verdict has no reversal list, and an `over_hardened`
  list can fail the route gate, so on those cards the action keeps its landed
  meaning: one ordinary next review round, no simplification work.
- **Auto-routing needs a remaining review round.** The simplification round's
  re-review counts toward `hard_round_limit`, and the landed controller
  refuses a round past the limit, so at the limit the verdict parks.

## Verification Findings

The recurrence walk in `store.EvaluateReviewConvergence` collects every
`fixed` disposition in the convergence segment, not each finding's latest
disposition. A reversed fix would trip `fixed_recurrence` on re-emission
unless the walk changes. The plan text states the latest-disposition rule as
a requirement and names the routing unit (#1051) as the place it changes.

A reversal also has nowhere to live in the landed store. Finding dispositions
are immutable, keyed by finding and round, and the write re-runs the binding
that the round's review record lists the finding
(`daemon/internal/store/review.go`). The earlier `fixed` row cannot be
overwritten, and the remediation round that performs the reversal does not
list the finding, so a plain second `declined` row is unwritable. The
contract unit therefore carries a supersession record bound to the run, the
reversing round, the superseded disposition, the `DriftAudit` digest, and the
authority that ordered the reversal, and the recurrence rule reads a finding's
effective latest disposition through it. The authority binding is what keeps
the audit proposal-only: without it, reconstruction could not tell an
authorized reversal from a proposed one, and the model artifact would become
disposition authority. Reconstruction re-proves the authority and fails
closed.

The audit is also the second site that consumes the approved specification,
which Finding Adjudication previously reserved to the adjudicator. Section
5.13's agent inventory and annotation-site list now name the drift auditor,
and the exclusivity sentence is scoped to the dispatch that routes findings.
The audit proposes reversals and routes nothing, so the invariant it protects
still holds.

## Revisit When

#1053 reports the verdict agreement rate. Flip `drift_audit_route` to `park`
if reversal lists are wrong at a rate the owner will not accept unattended.
