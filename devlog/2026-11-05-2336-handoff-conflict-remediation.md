# Handoff Conflict Remediation: A v7 Successor at the Sealed Tip

Issue #2646 implements plan revision 110's remediation at the current tip
(§7; `2026-11-05-0951-verify-at-handoff-not-on-base-advance.md`). A textual
conflict or a failed merge verification at handoff no longer ends the run: a
remediation at the tip the handoff check sealed writes a new head, which is
verified, reviewed, and handed off again. This meets the "Revisit when"
condition of `2026-11-05-1530-handoff-check-implementation.md`. The work is
Tier A: it decides the base a writer is admitted at and what reaches GitHub.

## Decisions

- **A new commandless successor, v7 `handoff_conflict`.** Chose a new
  origin and version over reusing the retired v5 and v6 re-entries or
  re-keying the handoff check per head. v5 and v6 keep their head and admit
  no remediation, and the store no longer seals them. A commandless
  successor keyed on the conflicted publication's ready item reuses the
  existing identities, task key, chain, and publisher paths. `Reentry` holds
  the sealed tip and the conflicted head; the new fields are the cause and a
  first-publication flag, both omitted on every older version, so stored
  records keep their bytes.
- **The tip comes from the sealed handoff check row.** The store gate
  re-derives the authority from that row (same tip and head, a moved tip, no
  proved merge), never from `LatestRunWorkflowAuditTip`, which keeps #2073's
  provenance question out of this unit. It also proves the prior review's
  round, head, and evidence base, that the predecessor never published, and
  the first-publication flag against `ready_item_pr_bindings`. The engine
  additionally requires the prior review to be the run's latest in the
  sealing write; the gate cannot, because it re-runs on every read after
  later rounds exist.
- **Seed through admission, not the driver.** `admitAttempt` replaces the
  derived base commit with the sealed tip for the cycle's remediations, after
  `derive`, as it does for a maintenance task, and fails closed when the
  derived repository, ID, or base ref differs from the run's production
  admission. The stage driver seeds from the admitted base, so no driver
  change is needed. Hydration stays on.
- **The first remediation answers the conflict, not a review.** It runs on
  the prior review's round and invocation, on the conflicted head, with no
  adjudication and no findings. `AllowsRemediation` admits exactly that
  request for v7, plus later rounds at the tip; the remediation and producer
  authenticators skip the adjudication checks only for that request, derived
  from the sealed authority. The remediation input's `adjudication` became an
  omitted pointer so that round can carry none; ordinary inputs keep their
  bytes and digests (pinned by a test).
- **One write seals, ends, and starts.** Sealing the authority, marking the
  conflicted task's row done, and committing the remediation's stage, input,
  invocation, and intent are one store write. A crash before it reruns the
  pass, which reuses the sealed tip and, for a failed verification, the
  stored checkpoint, so nothing is audited or verified again. After it, the
  conflicted task never reconciles again and the dispatch lane admits the
  remediation. No separate seal-recovery path exists. The conflicted task's
  row ends without a terminal, since the new cycle records its own; the
  invocation acceptance scan takes a row a sealed `handoff_conflict`
  authority names as its predecessor as handled.
- **The remediator gets bounded, quoted data.** A textual conflict carries
  at most 200 conflicted paths, each quoted and cut at 1 KiB. A failed
  verification carries the verifier's steps and the last 32 KiB of its
  transcript, made valid UTF-8, with the digests of both source artifacts.
  The input is checked against its sealed authority before dispatch, and the
  third byte-stable instruction and the prompt both call that content data,
  never instructions.
- **Refusals kept, each on today's block reason.** No remediation starts
  when the head is someone else's (a `head_changed` parent), the hard round
  limit has no room for the remediation's review, the task re-evaluates a
  blocked run's trust, the round's remediation identity is already taken, or
  the input would exceed the delivery cap. The handoff's other stops (image,
  rebuild trust, materialization, memory limit, head already the tip, a
  rewritten branch, a merge that passed with candidate findings) block as
  before; a code change at the tip cannot fix them.
- **The block texts stay.** `PublicationBlockHandoffConflict` and
  `PublicationBlockHandoffVerification` still say Freeside cannot yet
  remediate. Stored items are recognized by their exact text, so a reword
  needs legacy mappings; that is follow-up work.

## Departures From the Plan Comment

- **`admittedBaseSHA` needed no change.** The remediation's replay records
  the tip as its observed base, so the existing derivation already returns it.
- **The foreign-head check reduces to a `head_changed` parent.** An in-place
  re-entry reconciles before the handoff and never reaches it, so only a
  sealed parent can carry a head no Freeside invocation produced. The gate
  refuses that parent instead of calling `ReadyHeadIsForeign`.
- **The first remediation bypasses `prepareRemediationIntent`.** That path
  answers an adjudicated review; this one answers a conflict, so it has its
  own builder and persist step under the same identities.

## Known Limits

- **A trust re-evaluation task keeps blocking.** Remediating there would
  need the re-evaluation's completion semantics; out of scope.
- **A return to the agent after a v7 publication starts at the configured
  base.** The admission override covers the v7 cycle's own remediations.
  An operator-feedback invocation on the published item is a new cycle,
  admitted at the configured base, while its candidate patch is taken
  against the tip, so the patch does not fit the feedback's workspace.
  Follow-up: #2699.
- **The first round's review metrics are a gap.** The round answers no
  review, so `reviewRoundMetricsGap` records none for it.
- **The published review history keeps the tip's rounds only.** v7 is not an
  unpublished-head re-entry, so the disposition history's single-base rule
  drops rounds reviewed at the older base from the pull request body.
- **The review-round display omits the first round's remediation.** The
  display pairs a remediation with the review at its base; this one is at
  the tip, its review at the evidence base. Cosmetic.
- **A tip that changed dependency manifests is untested.** Image selection
  runs before the base override and takes no base, so it picks the image as
  for any attempt of the run, and hydration stays on. If that image cannot
  serve the tip's manifests, hydration fails the attempt before the agent
  starts, and the remediation ends as any failed remediation does.

## Refute-First Findings

A fresh-context refute-first review found no Tier A break: the admission
override, the adjudication skip, and the publisher paths are each confined
to the v7 first round or the sealed tip. Its other findings and their
outcomes:

- **A merge that passed with candidate findings was remediated.** Fixed:
  only a failed verification starts a remediation. Candidate findings are
  policy findings (control-plane content, import integrity) that no code
  change at the tip answers, so that case still blocks as before.
- **A request without an adjudication was refused only by a failed lookup.**
  Fixed: the remediation authenticator refuses it explicitly outside a
  handoff conflict's first round.
- **States the run's records contradict return errors, not blocks.** Kept:
  a latest review of another head, an unreadable report, or a second
  transcript is a daemon fault, and the verifier writes one transcript per
  run. The doc comment now says so.
- **The store gate does not prove the cause** against a failed checkpoint
  or the absence of a handoff stop. Declined: only the engine writes the
  record, and the database is trusted (Assurance Tiers).
- **Tests for a failed and an unchanged remediation, and restarts after
  admission and import.** Added. The remaining blocks (a trust
  re-evaluation, a taken identity, an oversized input) stay untested at
  integration level; each is one early return.

## Rejected Options

- **Remediate at the evidence base and merge again.** The conflict is with
  the tip; a head written on the old base meets the same conflict.
- **Re-key the handoff check per head.** The check row is write-once per
  publication by design; a new publication under its own authority gets its
  own row without weakening that.

## Revisit When

The block texts are reworded, or a trust re-evaluation needs to remediate at
the tip instead of blocking.
