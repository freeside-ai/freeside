# Verify at Handoff, Not on a Base Move

Issue #2629; plan revision 110. The owner directed on 2026-11-04: "a PR
should be verified before hand-off, and each time it's handed off again
after that if it's modified. But it shouldn't chase a moving main."

Chose a check at handoff, plus a fresh check whenever Freeside changes the
head, over re-verifying and re-reviewing a pull request each time its target
branch advances. Handoff is publication, first or successor. At handoff the
daemon checks the unchanged head against the tip current then; after that,
only a changed head starts another round. Between handoff and merge nothing
in Freeside checks the PR against the latest target branch, and the plan
says so: only the merge-time checks the target repository enforces cover
that gap, and Freeside neither requires nor verifies them, until Freeside
has a landing step.

## What This Overturns

- **Revision 89's pre-publication re-entry** (#2058,
  `2026-10-18-0940-pre-publication-base-advance.md`). An advance before
  publication sealed a re-entry, verified and re-reviewed the prospective
  merge, and counted that round toward `hard_round_limit`.
- **The successor re-entry** (#2111,
  `2026-10-21-1232-successor-base-advance-reentry.md`). The same for a
  feedback return or remediation continuation, through
  `freeside.publication-successor/v6`.
- **The base-advance half of #502's readiness re-entry** (owner decision of
  2026-10-02, `2026-10-02-0222-readiness-reentry-authority.md`). A ready
  item invalidated by `base_advanced` re-earned readiness against the new
  base. The `head_changed` half stays: a head someone else pushed is still
  verified and reviewed before Freeside calls it ready.
- **The Section 5.16 base-advance staleness watch** and the base-freshness
  fact it wrote on `ready_for_final_review` items
  (`2026-08-01-2310-durable-scheduler.md`).

## What Changed

The earlier decisions assumed review and verification evidence goes stale
when the base moves, so Freeside had to keep the evidence current until the
human merged. Three things changed that:

- **The owner's rule names the moments that matter.** Freeside answers for
  the head it hands off and for each head it changes later, not for every
  commit that lands on the target branch while a person decides.
- **This repository already works this way.** Its own handoff audits the PR
  against the tip at handoff and leaves later advances to the lander
  (`2026-10-23-0921-audit-only-handoff.md`). Freeside's product rule now
  matches the flow it was built under.
- **Chasing cost more than it bought.** Each re-entry spent a review round
  toward `hard_round_limit` on a head Freeside had not changed, and the
  watch marked ready cards stale after every merge on the target branch,
  against Section 3.2's interruption budget. Where the target repository
  requires checks on an up-to-date branch, they already catch a merge that
  breaks; where it doesn't, the owner accepts the gap until a landing step.

## How Handoff Checks the Head

When the tip at handoff is the base the evidence binds, the daemon
publishes. When it differs, the daemon builds the prospective merge of the
unchanged head into that tip, verifies the merge under the admitted recipe,
records the tip and merge it checked, and publishes the unchanged head. It
does not rerun review. A textual conflict or a failed verification of the
merge is a conflict, resolved before handoff by a remediation at the current
tip whose new head is verified and reviewed again. Until that remediation
exists in code, a conflict ends the run with the existing `publish_blocked`
item.

A pass still records the base it ran against, as evidence. The re-gate
compares proofs with the candidate head and the requirement set, never with
the current tip, so a later advance invalidates nothing. When the tip has
moved, only a proof of the merge into that tip satisfies the handoff check,
and the tip stays fixed until the publication lands, so the check never
chases a further advance. A publication binding that names a base SHA, such
as the closure approval, names the checked tip and fails closed on an older
one.

## Rejected Options

- **Audit only, with no verification of the merge.** The owner's rule says
  verify at handoff, and a clean textual merge can still fail the build.
- **Re-review the merge at handoff.** Review covers the head. The merge adds
  nothing Freeside wrote, and a re-review would bring back the round cost
  this revision removes.
- **Keep the watch as information only.** With nothing acting on an advance,
  the fact would only mark every ready card stale after each merge. The
  card shows the tip its handoff checked instead.

## Revisit When

Freeside gains a landing step. The handoff check and the landing check then
become one mechanism, and the gap between handoff and merge closes there.
