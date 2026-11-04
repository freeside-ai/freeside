# Keep Commitments in the Plan, Mechanism at Its Point of Use (Plan Revision 109)

Chose a content rule for `docs/plan.md`: the plan holds what Freeside commits
to, and mechanism lives where it is used, in package `doc.go` comments,
component READMEs, issue contracts, and decision notes. The test for a
sentence is whether changing it would change what Freeside commits to, or
only how Freeside does it. Moving mechanism out while the commitment stays in
the plan is a wording change, so a shrink unit doesn't trip the materiality
gate. The owner decided this by filing #2520 with the rule on 2026-11-03.
AGENTS.md (Document Gating) carries the full rule; plan §9 states it briefly
and links there, so the two can't drift.

## Why

- **The plan has grown into a mechanism spec.** At revision 108 it runs 7,325
  lines; §5.4 alone is about 1,070 and §5.7 about 750. It names record
  fields, hold reasons, error types, and which components may read a record.
- **Routine implementation churns it.** PR #2496 changed `docs/plan.md` only
  to name the quota park as a second reader of a usage observation. Every
  unit that touches such detail has to reconcile the plan with the code, and
  the plan-change gate fires on edits that commit Freeside to nothing new.
- **Mechanism already has better homes.** A `doc.go` comment or README sits
  beside the code it describes and changes in the same commit; an issue
  contract holds a unit's acceptance; a decision note holds the reasoning.

## The Rule

- **In the plan:** purpose, goals and non-goals, operating principles, safety
  invariants and trust boundaries, authority, component responsibilities,
  interface shapes that bind several components, roadmap and sequencing,
  risks, and the one-line decision log.
- **Out of the plan:** table and column names, record fields, hold reasons,
  error types, reader lists for a record, test names, and per-unit acceptance
  details.
- **The moved text lands at its destination in the same PR**, unless the
  destination already says it, so nothing is lost in the move.
- **"Acceptance criteria" in AGENTS.md's materiality list means plan-level
  criteria**, such as phase exits. Per-unit acceptance belongs to issue
  contracts, so moving it out is not material. This narrows the list only
  as far as the moving-out rule needs.
- **"In" wins a tie.** An item on the out list that is also a safety
  invariant, trust boundary, authority, or shape several components share
  stays. A sentence that mixes a commitment with mechanism is split, not
  moved whole.
- **Size check:** a section past roughly 150 lines prompts a reviewer to ask
  what in it is mechanism. It is a review prompt, not a limit; some sections
  hold that many lines of commitments.

## Rejected

- **Leave the plan as is.** Routine implementation keeps churning it, and
  every unit keeps reconciling with detail that commits Freeside to nothing.
- **Split the plan into many files with no content rule.** Smaller files
  would still hold the same detail, so it would churn in more places, with
  more cross-links to keep true.

## Revisit When

- Shrink units keep finding that the text they want to move is a commitment,
  not mechanism, so the test isn't separating the two.
- Mechanism moved out of the plan goes stale at its destination because no
  reader looks there.
