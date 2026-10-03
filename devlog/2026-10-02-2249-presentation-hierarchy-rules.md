# Amend Plan §9 For The Approved Visual Hierarchy

Plan revision 78 changes §9 so it agrees with the visual hierarchy the owner
approved on 2026-10-02
([owner-decision note](2026-10-02-2212-approved-visual-hierarchy.md),
[handoff](../docs/design/visual-audit-2026-10-02/README.md), decisions D03
and D06 to D09). That note records what the owner approved. This note records
how the plan text was changed and what was left alone.

## Scope: Four Card Types, Edited In Place

Chose in-place edits scoped to `agent_question`, `ready_for_final_review`,
`review_dispute` and `finding_adjudication` over two alternatives:

- **A general rewrite of Layering was rejected.** The owner reviewed these
  four cards. The handoff limits "less framing" to D06 to D09 and excludes
  the diminishing-returns, recovery and pairing treatments. A general rule
  would restyle cards nobody reviewed.
- **A separate "approved exceptions" appendix was rejected.** Each changed
  rule would sit far from the invariant it keeps, and a reader of one table
  row would not see that the row had an exception.

Each changed sentence carries a `(revision 78)` marker and states its retained
invariant beside it, so the client units (#1732, #1033) can read one row and
know both what moved and what must not.

## Decisions Made While Writing The Text

- **`finding_adjudication` stops being the model for recommendation-led
  cards.** §9 used to say its composition generalizes to every type with a
  recommendation. The approved card puts the finding cards first and the
  item's recommendation beside the batch action below them, so it is now the
  one exception. The general rule (a recommendation leads with its reason)
  stands on its own for the other types.
- **The "may fold" rule names what can never fold, not only what can.** The
  handoff warns that a production snapshot carries more significant facts
  than the clean reference fixture. The text lists the warnings that stay
  visible (stale or base-advanced, degraded or waived, missing capability,
  consequence, commit-plan) and requires a mixed group to be split. Without
  that list, "routine coordinates" could grow to cover a whole facts module.
- **The invocation-ID change is limited to the same four card types.** D03's
  on-demand explanation was approved on those cards. Other types keep the
  invocation ID in the label until a reviewed design says otherwise.
- **§7 changed by one sentence.** It said human-facing adjudication "leads
  with a recommended route and why". With the rationale now inside each
  finding card's disclosure, that sentence would contradict §9. It now lists
  the parts an adjudication must carry and leaves their placement to §9. The
  required content is unchanged. Leaving §7 alone was rejected because two
  sections would then disagree about the same card.
- **Passed-check values are not named in §9.** The final-review card already
  collapses passed rows behind a disclosure, and §9 never forbade it. D07
  keeps every check name, count and value reachable, so the plan needed no
  new rule for it.

## #1141's Governing-Record Question

#1141 asked which record governs the finding lead: §9, which showed the
finding's facts, or #1107 row 01, which bought the first-viewport budget by
collapsing them. D09 is the owner's answer on visibility: the card shows the
exact finding message. So §9 is the record that changes, and it now says so.

The budget is a separate question and is unchanged: the action region stays
within 520pt of the card top at a 560pt card and `.large`
(`DecisionLayoutBudgetTests`). Screenshot approval did not relax it. #1141
measured an earlier message-only shape at 490pt with a one-line fixture
message. The approved card is a different shape and real messages wrap, so
it needs its own measurement with realistic text. If it does not fit, the
measurements go to the owner through #1141. No agent raises the threshold,
shortens the fixture or hides the message again.

## What The Amendment Changes For Existing Tests

One existing invariant narrows. `factsNeverRenderBelowTheActions` holds for
every Phase 1 type today. On the four card types it becomes "no verdict,
exception or decision restriction renders below the actions", and routine
coordinates may. It is unchanged for every other type. The PR body classifies
the remaining composition assertions by test name.

Revisit when the dispute snapshot always carries both positions (the
one-claim fallback then has no case), when another card type gets a reviewed
hierarchy (extend the list by name, do not generalize the rule), or when
#1141's measurement shows the approved finding card cannot meet the budget.
