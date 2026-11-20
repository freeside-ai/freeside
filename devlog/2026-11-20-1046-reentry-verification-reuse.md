# Re-entry Reuses Its Predecessor's Verification Of The Same Input

Issue #3445: an external review cycle on an unchanged pull request verified
the same head on the same base again, 31 minutes of rooms on #2382's exit run
before anything acted on the owner's comments. An in-place re-entry now
reuses the verification its predecessor's ready item holds when that
verification covers exactly the input the cycle would verify. The note is
mandatory: the change decides when a verification verdict stands for a
cycle (Tier A).

## Decisions

- **Reuse only from the cycle's own predecessor ready item.** Chose this over
  keying re-entry checkpoints by head and base across cycles. A shared key
  would let one cycle's row stand for another cycle's task key, and every
  checkpoint reader binds the row to its own task. The cycle still writes its
  own checkpoint under its own key, and names the reused invocation in it.
- **"Same input" is head, base, no prospective merge, and recipe, read from
  the verifier's own report.** The report carries the head, base, evaluated
  commit, recipe, outcome, and findings, so the gate compares those and
  trusts nothing the ready item restates. Only a pass with no findings is
  reused; a failure or findings verify again, as before.
- **The image follows from the run and head, so it is not compared.** A
  cycle that verifies its head binds its image from the run's write-once
  rebuild row for that head, or else the admitted image of the predecessor's
  producer (`resolveVerificationImage`), so an earlier verification of the
  same head in the same run used the same image. Neither the ready item nor
  the report names the image, and reading the predecessor's checkpoint for
  it would mean decoding both checkpoint kinds a predecessor may have.
- **Exclude the repair path, a prospective merge, and an unpinned trust
  profile.** The repair path binds its image through upkeep's gate, outside
  that argument; the issue keeps it out. A cycle verifying a merge verifies
  a commit of its own. The verifier's control-path patterns come from the
  trust profile, which stays fixed across cycles only while the admission
  pins its digest; a legacy admission reading the latest profile verifies
  again.
- **Exclude hand-back cycles.** A hand-back's predecessor binding is the
  latest published ready item at or before its admission, which need not be
  the cycle's own predecessor, and the issue allows reuse from no other
  record. It also adopts a head no Freeside invocation produced, which the
  plan says it verifies in full.
- **Read the predecessor item through the current-policy evidence gate**
  (`GetAttentionItem`), not the history read. Reuse presents that evidence
  as current, which is what the store's gate is for. The recipe-approval
  hold already runs before reuse, so a revoked recipe holds the cycle as it
  does today.
- **Skip the pre-room image gates on reuse.** No room runs, so whether the
  image can serve the base under the running daemon's preparation sources
  is not asked again; the verdict was produced under the gates in force when
  it ran. A fresh verification after a daemon upgrade that changed those
  sources would instead stop the cycle for image upkeep.
- **Accept a verdict from an older verifier.** The report records no
  verifier version, so a cycle after a daemon upgrade reuses what the older
  verifier found. The issue scopes reuse to head, base, and recipe, and a
  verifier change that adds a finding kind matters mostly to new candidates,
  which never reuse.
- **Mismatch is no reuse; a read failure is an error.** A flaky disk must
  not silently fall back to a 31-minute verification and hide the fault, and
  a predecessor blob that no longer matches its digest fails closed.
- **Version the checkpoint (v3) and read the report under the reused
  invocation.** `loadReentryCheckpoint` and
  `loadReentryRemediationSourceTree` both read the report under
  `ReusedInvocationID` when set. v1 and v2 rows still load and may not name
  a reused invocation; a remediation still starts from a v2 row.

## Revisit When

- A verification input outside the report (image, control patterns, the
  verifier itself) can change between cycles of one run: reuse then needs
  that input recorded and compared.
