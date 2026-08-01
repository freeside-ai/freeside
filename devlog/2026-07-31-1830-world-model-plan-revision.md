# Plan Revision 25: World Model, Proposals, and Judgment Calls (+ Review-Stage Ownership)

Work unit: #420 (canonical draft in the issue body). Folds the plan-change
portion of #427 at the owner's 2026-07-31 reopen. This note carries the
revision's rationale, the two review tracks' disposition summaries, the
owner decisions that would otherwise exist only in chat, and the one fork
deliberately left open.

## Decisions

- **Chose to reopen the review-closed #420 draft and fold #427's
  review-stage change into one revision PR, over a second separately gated
  plan PR** (user, 2026-07-31 spine deferral sweep on both issues), because
  the review-stage change overturns statements the same revision already
  touches (§5.3, §7, §11) and two serialized plan PRs over the same
  sections would force an artificial ordering with no independent review
  benefit; the fold is marked as post-closure content riding the plan PR's
  ordinary external review.
- **Chose Freeside-invoked local Codex review as the production review
  gate, over the GitHub-native Codex review trigger** (user; evidence on
  #427), because the 2026-07-31 live run falsified the trigger assumption
  behind §5.3's CodexGitHubReview-primary statement: automatic review never
  started for App-authored PRs; an App-authored `@codex review` was
  recognized but rejected at account resolution; the same request from the
  connected human account reviewed the exact head immediately (the
  restriction is the requesting commenter's identity, not the PR author);
  and reviews are head-bound, so every remediation push needs another valid
  trigger the App cannot produce.
- **Rejected a human-PAT trigger as the production dependency** (user, via
  the #427 run report), because it binds unattended operation to one
  person's account linkage, token lifecycle, quota/billing, continuity, and
  human attribution. Native review stays recordable as best-effort extra
  evidence only; it never satisfies the review requirement.
- **Chose to demote rather than remove the native-review watch**: observed
  native activity is still recorded post-publication evidence (#427
  non-goal), so no evidence source is discarded.
- **Owner decisions carried from the draft**: the initiative view ships
  minimal (a deterministic projection) in 1B.2; plain-English scheduling
  lands before any conversational surface, CLI-first, deferred past the 1B
  exit; cross-vendor driver selection is not choosable by inference.
- **Sequencing intent confirmed by the spine sweep on #427**: #427
  implementation → production runs with Claude implementing and Codex
  reviewing → #397 Claude review promotion → #408 Codex execution routing,
  so Codex-implements/Codex-reviews never becomes the default pairing.

## Open Fork: The Review Anchor (Deliberately Unresolved)

Presented for owner decision at the PR, not resolved silently (user
instruction): either the required review pass anchors **pre-publication**
(implement → verify → review → clean: publish; the PR opens already
reviewed, forge checks still gating merge) or it stays **PR-anchored**
post-publication as §11's 1B chain currently reads. The trigger
falsification forces neither; a Freeside-invoked reviewer can review either
surface. Recorded lean: pre-publication with forge checks still gating
merge. The plan (§7) carries the fork explicitly; the §11 chain keeps the
PR-anchored shape until the owner resolves it.

## Review Provenance and Disposition Summaries

Two tracks closed the draft before the reopen; their full round-by-round
exchanges lived in the drafting sessions, and this note carries what the
work unit durably recorded (the #420 body's Review Provenance section is
the canonical statement):

- **External refute-first review (Codex), 2026-07-31**, against draft
  revisions 1–6: all blocking findings accepted and closed in the draft
  (the issue objective counts seven rounds; the draft provenance records
  six against revisions 1–6 plus the post-independent-review amendment).
- **Independent fresh-context adversarial review, 2026-07-31** (separate
  agent, given only the plan and the draft): one blocking finding —
  unattributable automation descendants in Freeside-seeded but unprofiled
  repositories — and five important findings, all incorporated; the
  blocking finding became §5.17's demote-all-label-intake-to-propose rule.
- **R14 (review-stage ownership) is not covered by those closed rounds**:
  it was folded after closure from #427's live-run evidence and per-pass
  contract, and rides the plan PR's ordinary external review.

The "Plainly" layers in the draft are non-binding; the Contract text
governs, and the plan carries only the binding content.

## Rejected Alternatives (Revision Content)

Recorded in the draft's review history and carried here so they are not
re-litigated blind: open-ended effect kinds (rejected for a closed
registry with trusted constructors); semantic-content occurrence identity
(rejected: deliberate repeats must stay distinct); auto_start intake for
Freeside-origin or unattributable issues (rejected: propose-only);
ad hoc per-subsystem timers (rejected for the single durable scheduler);
"related PR merged" completing a work unit (rejected: exact recorded
binding only); flattened readiness booleans (rejected: ReadyClean vs
ReadyDegraded is load-bearing).

Follow-up: #427.

Revisit when: the review-anchor fork is resolved (expected at or before
#427's implementation); multi-project onboarding is real (lane/seam-map
generalization); the 1B exit arrives (per-run authorization cost accepted
in lieu of consent grants); Codex execution goes live (#397's promotion
value, already strengthened by the #427 evidence).
