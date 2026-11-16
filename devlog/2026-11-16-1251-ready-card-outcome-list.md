# The Ready Card Shows the Run's Outcome List

Plan revision 128 has the `ready_for_final_review` card list what the run
did around its pull request: each follow-up issue or issue comment it
filed, posted, or proposed and where it stands, and each finding it
declined or deferred with its reason. #3149 (merged in #3200) serves the
list on the run timeline; #3264 draws it on the card. The owner split the
two on 2026-11-16, following #3149's plan.

## Placement

- **Chose Recorded Context, after the review yield,** over leading the
  card with the list ahead of the actions. The list reports what happened
  and decides nothing. It carries no verdict, exception, or restriction on
  the merge, so plan §9 (revision 78) gives it no first-viewport space,
  and it reads as history beside the review yield it extends.
- **Chose to name it in the closed fold's summary line** (`outcomes`, after
  `review yield`), so the operator sees the list is there without opening
  anything.
- **The findings' message and reason draw under an unverified keyword.**
  They are the reviewer's and the agent's words. The posts are daemon
  records and draw as plain facts.

## Materiality

Naming the list in §9's "Below" column adds a commitment about what the
ready card shows, so it is a material plan change (plan §9, materiality),
not wording. It rides in the card's PR because the plan change is that PR's
direct subject (AGENTS.md, Document Gating). The row names no fields; the
API schema and the card's code hold them.

## Data Path

- **Chose to pass the run timelines into the card as values** over giving
  `DecisionDetailView` the sync coordinator. The card already takes synced
  values this way (`usagePools`), and its other callers (System Health and
  Settings) have no timeline to give. The root view passes the timelines,
  the run snapshots and cursors that key a reload, and the coordinator's
  load call.
- **The card loads its run's timeline as the run timeline page does:** on
  open and on every canonical replacement, keyed the same way. Only a
  final review whose run the coordinator lists loads anything, since the
  coordinator prunes the timeline of an unlisted run.

## Revisit When

The owner wants the list ahead of the actions, or a later unit makes an
entry actionable from the card (a per-finding action). Either changes
§9's "Leads with" column and the first-viewport budget, not just this row.
