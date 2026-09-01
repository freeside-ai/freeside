# Retire the Alternate-Profile Action (Plan Revision 44)

**Work unit:** plan revision 44; #936 is re-scoped to the enum removal.
**Origin:** planning #936 at `b51d86ef` found that the action's premise, an
approved set of alternate publication profiles the daemon can enumerate, has
no source in the plan or the daemon (the release comment on #936 records the
grounded finding). The owner then decided the action's fate and, wider, what
Freeside's CI trust boundary is for.

## Decisions

1. **`publish_blocked` drops `choose_alternate_profile`** (§4, §9, §11).
   Plan v4 meant it as a per-item pick among pre-approved trust profiles
   whose `pr_execution` mode differs (`audited_same_repo`, `fork_untrusted`,
   `local_only`), so verified work could still get out when the
   same-repository CI path was blocked. The owner's call: which path a
   repository uses is configuration settled at onboarding, and fork versus
   same-repository follows from whether Freeside can push, so nothing is
   chosen per run. Offering the choice on a blocked run is where the weaker
   path gets picked under pressure. Only `trust_profile_drift` among the
   four trust rules could be helped by a different path; its fix is
   re-approving the repository's configuration on the Mac, then
   `rerun_trust_evaluation` (#419). Rejected: building the approved set
   (multi-profile approval, fork publication, local-only handoff); a
   one-item picker over the current profile on drift; leaving the action
   pending. (User, 2026-09-01.)
2. **Fork publication is a plain publication feature, deferred** (#1042).
   No trust meaning; triggered by the first repository the operator can't
   push to. (User.)
3. **Direction recorded, not enacted: the CI audit is out of scope; protected
   paths are the control** (#1041). Freeside pushes agent-written code
   without a human seeing it first, so the agent must not change dangerous
   code unsupervised: anything CI or verification executes, or anything that
   governs Freeside. §5.8's protected path classes do that. Auditing whether
   a repository's CI runs PR code with secrets is the repository's posture,
   the same for any contributor, and not an agent control plane's job.
   Freeside also should not behave differently for repositories the operator
   owns versus ones they don't, beyond what GitHub forces. Deciding what
   counts as executed code stays mostly mechanical (workflow `run:` and
   `uses:` targets, recipe commands, the standard build-script list); an
   agent classifier, if ever added, may only add a block, never clear one.
   This is a material §5.5/§5.8/principle-6 change and gets its own
   revision after the workflow-audit, onboarding, and doctor code is read.
   (User, 2026-09-01.)

## Verification Findings That Changed the Work

- Code reading at `b51d86ef`: the store holds one activated profile per
  repository (`LatestTrustProfile`), `onboard` hardcodes
  `audited_same_repo`, `fork_untrusted` and `local_only` exist only as enum
  members, `PublishBlockFacts` has no offer field, and no producer ever
  offers the action. So retiring it changes no rendered card.
- No open wave-7 unit references §5.5, §5.8, trust profiles, or drift, so
  the deferred reframe blocks nothing in wave 7; the first collision is the
  wave-8 onboarding work.
- The plan-text provenance: the phrase entered with plan v4 (`27fa9f2d`,
  correction batch item 5) alongside §5.5's `pr_execution` modes; nothing
  after v4 defined the set.

Revisit when a real repository the operator cannot push to is onboarded
(#1042), or when #1041 is planned and the executed-surface class is defined.
