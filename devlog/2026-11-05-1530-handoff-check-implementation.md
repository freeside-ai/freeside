# Handoff Check: Sealed Tip, Gated Merge Proof

Issue #2644 implements the rule in
`2026-11-05-0951-verify-at-handoff-not-on-base-advance.md` (plan revision
110). This note records how the daemon holds that rule and what it chose to
keep from the cycle it replaces. The work is Tier A: it decides what may
reach GitHub.

## Decisions

- **A sealed store row, not fields on the task.** Chose a
  `publication_handoff_checks` row (migration 0126) keyed by run and
  publication over carrying the tip and merge on the publication task or
  successor. The tip is written once from a recorded workflow audit and never
  re-resolved; a second seal with another tip is an immutable conflict. The
  merge is written once, only against a recorded check proof that names this
  head, tip, and merge. A row outlives the task that wrote it, so recovery
  and a crash after the seal reuse the same tip instead of auditing again,
  which is what keeps the check from chasing the target branch.
- **The gate re-checks the row where authority is spent.** The row is
  ordinary state, but the decision to publish re-reads it inside the
  publication decision transaction (`publish.validateHandoffCheck`) and never
  trusts the candidate's copy. It requires the copy to equal the row, the
  candidate's base to be the sealed tip, and, for a moved tip, a merge proof
  bound to the readiness's own clean-verification resolution and the
  candidate's recipe. The gate treats the tip as moved when any readiness
  proof binds another base, not only when the tip differs from the evidence
  base, so evidence earned at a stale base still needs the merge.
- **The gate runs on a newly reserved intent only.** As the base check it
  replaces did, a reservation made before the restart is recovered under the
  decision it was made with. Re-gating a reserved intent could strand a PR the
  forge already holds. For the same reason, an intent an earlier daemon
  committed before handoff checks existed (pending, with no sealed tip)
  skips the handoff and recovers at the base it was decided against.
- **A tip newer than the pass's checkout retries; only a later fetch can
  call it rewritten.** The pass fetches the base branch before it verifies
  the head, and the audit seals the tip after, so a push in between seals a
  tip the checkout lacks. That pass retries. A checkout fetched after the
  seal that still lacks the tip means the branch was rewritten, and the run
  blocks rather than re-resolve.
- **Verify the merge with the re-entry verifier.** The merge runs through the
  same verification, image binding, rebuild gate, and checkpoint machinery
  the readiness re-entry uses, under its own checkpoint key and invocation.
  A retry reuses the stored checkpoint, so a crash after verification does
  not verify twice. No review runs.
- **Retire queued v5 and v6 cycles; do not run them.** A task an earlier
  daemon queued to re-enter before publication ends on `publish_blocked`
  with a third new text, `PublicationBlockReentryRetired`. Running it under
  the old rule would spend a review round the owner's rule removes; running
  it under the new rule would need a candidate the old cycle never formed.
  The store refuses to seal a new v5 or v6 successor, so nothing reaches the
  retired path except a row written before the upgrade. A retired cycle
  whose publication intent that daemon already committed holds instead of
  blocking, since this daemon can neither finish nor cancel that intent. A
  block that daemon wrote before stopping recovers against the authorizing
  checkpoint, taking a failed re-entry verification's evidence from the
  stored item.
- **Keep the legacy reads.** Stored v5 and v6 rows still decode, their
  disposition history still reads, the three revision-89 block texts still
  map to `base_advanced` and recover, and `AdmitsSuccessorReentry` stays for
  the read gate. Deleting them would quarantine runs an earlier daemon
  finished honestly.

## Departures From the Plan Comment

- **A third retired-path text.** The plan named the conflict and
  verification texts; retirement needed its own so a person can tell a
  retired run from a failed merge.
- **Image stops do not name their clause.** A handoff whose merge the
  rebuild gate refuses, whose tree cannot be materialized, or whose tip no
  image serves blocks with the generic base-advanced text. The removed cycle
  did the same. Naming the clause needs a reason the block facts can carry;
  that is follow-up work (#2658), not part of this unit.
- **The store does not check the merge's git parents.** It trusts the check
  proof the daemon recorded for the merge it built. The proof binds head,
  tip, and merge SHA; the daemon's own database is trusted.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong before commit.

- **Confirmed and fixed:** a push between the pass's fetch and the audit
  ended the run for good; a retired cycle and a pre-upgrade intent could
  block over a committed intent; an unfinished legacy block would fail every
  pass. Each fix has a test that fails without it, except the legacy-block
  reasons, whose items an earlier daemon wrote and the harness cannot forge
  with a valid decision surface.
- **Disproved by a check:** no path publishes at a moved tip without a merge
  proof bound to head, tip, repository, and base ref; the seal never takes
  another tip; a crash after seal, verification, or proof converges.
- **Allowed by decision:** a rerun after a trust block reuses the sealed tip
  and merge. The tip is fixed for the publication by design. The gate's
  recipe and resolution comparisons are unreachable through the store's own
  proof gates in tests and stay as defense in depth.

## Rejected Options

- **Re-audit the tip on every pass and accept the newest.** That is the
  chase the owner's rule forbids, and it would let a crash between
  verification and publication publish against a tip nothing verified.
- **Check the handoff in the engine only.** The engine is upstream of the
  publisher; a candidate assembled on another path would skip it. The gate
  sits in the decision that grants the publication.

## Revisit When

Freeside gains remediation at the current tip. A conflict or failed merge
then remediates instead of blocking, and the retired-path text can go once no
stored row can reach it.
