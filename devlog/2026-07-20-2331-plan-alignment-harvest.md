---
run: manual
stage: plan-alignment-harvest
date: 2026-07-20
branch: docs/plan-alignment-harvest
---

# Harvest the Intro Review Into Plan-Alignment Edits

Owner decisions, made in session on the #208 harvest and recorded here.
Declared paths: `docs/plan.md`, `docs/history/decisions.md`, `AGENTS.md`,
`README.md`, plus this note. Source material: PR #192's review threads and
final revision, and the prior note
`2026-07-19-1011-intro-doc-layering.md` (frozen on #192's merge).

## Dispositions

Each item from #208, with its decision. "Adopt" means the intro's settled
formulation enters the plan (revision 15); the intro itself is untouched.

1. **Thesis — adopt (owner choice).** The canonical one-liner now grants
   autonomy ("grants agents the autonomy to turn work items into
   evidence-backed pull requests"), carried by plan §1, README, and
   AGENTS.md with their existing person conventions ("me" / "a human").
   Rejected alternative: keep "turns a software work item into an
   evidence-backed pull request" and revert the intro later; rejected
   because the PR #192 first-principles reframe made autonomy-as-means the
   deliberate lead, and the plan should say what the owner means.
2. **Objective vocabulary — adopt (owner choice).** Plan §1's measure is a
   positive return (useful, correct work worth more than the attention,
   maintenance, money, and risk it costs), superseding "net personal
   leverage after maintenance": the return formulation names all four cost
   terms instead of only maintenance. "Personal-leverage tool" stays as
   identity; only the measure sentence changed. Claim 1 gains its
   numerator (work per unit of attention rising against a passively
   logged, normalized baseline); "attention falls" measured the wrong
   thing when throughput grows. The consistency sweep aligned the same
   class in place: the §11 exit criterion ("attention materially below
   baseline") and the §12 kill criterion ("does not materially reduce
   attention") now use the per-unit measure, since flat attention with
   higher throughput must count as success, not failure. Review widened
   the same class once more: §4 and §8 called open-to-decision time "the
   product metric", which read as the governing measure; both now name it
   the headline attention-latency metric with the §1 per-unit measure
   governing.
3. **Auto-merge — adopt (owner choice, continuous with the decision
   recorded on PR #192 thread and the prior note).** Non-goal 1 drops
   "never auto-merges"; human merge is the current accountability
   checkpoint and narrow, risk-bounded automatic merge stays deliberately
   open. Rejected alternative: keep the absolute; rejected twice by the
   owner ("I don't want to dismiss the possibility out of hand").
4. **Oversight — adopt.** New §3.5, appended so no existing §-reference
   renumbers: oversight is non-optional and deliberately frictionless,
   with §8/§9 named as its designed instruments.
5. **Standing-grant promotion criteria — adopt into §3.1**, not §5.12:
   §3.1 is where repetition-to-policy promotion already lives, so the
   criteria (low risk, stable preconditions, bounded downside, never
   repetition alone) define "eligible" at its point of use. Rejected
   alternatives: §5.12 (initiators declare policy, they do not define
   promotion), and dropping the criteria (they were cut from the intro
   for altitude, not rejected on substance).
6. **Durability fallback — adopt into §5.9.** "Anything that cannot be
   safely retried waits for me" states the fail-toward-attention side of
   effectively-once that reconciliation-and-retry language alone left
   unspecified. The earlier "supported, reconcilable" qualifier stays
   dropped; §5.9's authority table already scopes which systems are
   supported.
7. **Harness routing — adopt.** §8 names the routing inputs (task class,
   quality, latency, usage, cost, consistent with §8's usage-as-observed-
   telemetry rule) and counts today's manual provider balancing in the
   attention accounting; non-goal 5's deferrals now open on recorded
   outcomes rather than the vaguer "explicitly earned". The intro's cut
   staging sentence is not resurrected.
8. **Measurement as designed work — adopt.** §1 marks the four claims as
   necessary gates with cost and maintenance still deciding the return;
   claim 3 is verified by conformance and adversarial tests, never read
   off telemetry; §9 Measurement adds normalization by volume and risk
   and maintenance accounting.
9. **Trust-profile drift claim — verified, no edit.** The intro's "stops
   opening pull requests" matches §5.5: the profile is digest-bound and
   drift fails closed at the publish path, which is where pull requests
   originate. Recorded so the check is not re-run.

## Revisit When

- Any risk-bounded automatic-merge class is actually proposed: that is a
  material plan change plus an intro re-check, per the prior note's
  standing condition.
- Routing evidence accumulates enough to propose automatic switching:
  non-goal 5's door opens through the ordinary plan-change gate, not by
  reinterpretation.
