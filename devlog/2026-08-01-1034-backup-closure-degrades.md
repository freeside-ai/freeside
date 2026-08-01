# Report an Uncomputable Backup Closure, Never Refuse to Start

Work unit: #430. Scope: `daemon/internal/store`, `daemon/cmd/freesided`,
and `devlog/`.

## Decision

A durable outbox row this binary cannot reconstruct is a **closure gap**
carried by the scan, not an error returned by it. Every consumer then decides
for itself: maintenance skips checkpoint production and reports, backup health
reports the artifact closure unhealthy, and the manifest boundaries (seal,
verify, restore) refuse outright. This closes the residual gap the #424 note
recorded: that unit kept the reconcile loop alive, while startup still died in
`newDaemon` before `Engine.Run` existed.

The posture is the issue's: **fail-closed but alive**. Unattended admission
stops (`ErrArtifactClosureIncomplete`, already a mutable refusal), attended
work continues, and the daemon survives to be upgraded in place, which is the
repair the dominant cause needs. A daemon that refuses to start cannot be
upgraded, so intolerance at startup converts a reversible downgrade into an
outage no operator action inside the product can end.

**Tolerance is confined to the outbox scan.** Only there is the value a payload
written by some binary's version of an intent rather than state this store
owns; the artifact, conversation, admission, attention, and command sections
of the closure stay loud, following the codebase's rule for broken owned
state. Within the outbox, all four causes are one class: an unregistered kind,
a rejecting extractor, an extractor yielding an empty digest, and a row
`GetOutbox` cannot rebuild at all. The last one matters most and is not in the
issue's list: a status or timestamp a newer daemon wrote wedges the scan
exactly as an unreadable payload does, one column over, with the same remedy.
Fixing only the two named causes would have left the same wedge reachable.

**The live database, not the last production attempt, is what health
describes.** The producer re-derives the verdict from a live outbox scan every
pass and publishes it to the health evaluator through the shared file set.
Deriving it from the produce path instead was simpler and wrong: production
runs only when a checkpoint is due, so a gap arriving under a current
checkpoint would have left unattended admission open for up to the currency
window (24h) while no new checkpoint could be produced at all. The cost is one
bounded outbox scan per maintenance pass (hourly), which is the same work the
checkpoint scan already does when it produces.

**No manifest is written from, or verified against, an incomplete scan.**
Three guards, deliberately not one: the seal path (a manifest must describe
everything its snapshot holds), stored-checkpoint verification, and restore.
Verification cannot be left to digest equality, which is the subtle case: the
skipped rows may contribute no digest the rest of the scan lacks, so an equal
manifest proves nothing. The regression pins exactly that, by inserting an
unreadable row into a checkpoint whose manifest already matches without it.

**Restore refuses where the daemon tolerates.** An older binary restoring a
newer daemon's checkpoint would admit rows whose blobs it never proved
present, and restore is the one caller that overwrites a store rather than
observing one.

**Legacy plaintext cleanup stays behind deletion-before-proof.** A first draft
reasserted the plaintext-absence invariant on the gap path, on the reasoning
that it is unrelated to the closure. It is not: that pass proves no
checkpoint, so removing the last legacy fallback under it is the exact
deletion-before-proof failure an earlier unit's refutation established, and
every other failing pass already declines it. Found by this unit's own
refutation pass, fixed, and pinned by a test.

## Rejected Alternatives

- **Tolerate at the caller** (let `newDaemon` swallow the scan error) — the
  scan would still refuse to produce a manifest at all, and health would still
  fail rather than report, so the doctor and the scheduled producer keep the
  same wedge.
- **Quarantine the row in the store** (the durable `outbox` `quarantined`
  status) — rejected for the reason #424 rejected it: the status is permanent,
  and the dominant cause is a temporary downgrade, so it converts a reversible
  condition into permanent data loss.
- **Raise a `system_health` attention item** — the doctor already converges an
  unhealthy `artifact_closure` finding into an operator-visible item, and this
  condition needs no second channel. It also blocks all unattended admission,
  which the health dimension already does through the intended gate.
- **Report the condition through `Store.BackupHealth` directly** — truthful for
  the separate `freesided doctor` process too, but it needs the payload
  extractors threaded through `store.Options`, widening a `kind:fix` unit into
  a shared-surface change beyond the interfaces this issue declares.

## Accepted by Decision

- **The `freesided doctor` CLI does not see a live-only gap.** It builds a
  health source without a producer, so it reports the checkpoint's own verdict.
  A checkpoint holding the unreadable row is reported unhealthy there (the
  condition the issue names), and this is no worse than today, where the CLI
  never scans the live database either. Closing it is the `store.Options`
  change above.
- **A `GetOutbox` failure is classified as a gap even when its cause is
  infrastructure** (a SQL fault, a cancelled context), because the causes are
  not separable from a wrapped error and every one has the same fail-closed
  remedy here. A genuine SQL fault fails the surrounding read anyway.
- **Only the first unreadable row is named.** A downgrade past a version
  boundary makes every row of that kind fail, so joining them adds volume, not
  diagnosis.

## Verification

Refute-first pass (mandatory: returned-object trust boundary). Run by the
author, not a delegated fresh-context lens, which this session's platform
policy does not permit without an explicit request; the PR's automated
reviewer is the independent pass. Confirmed and fixed: the
deletion-before-proof violation above. Rejected by verification, so they are
not re-raised: the manifest content for a readable database is unchanged (the
outbox helper still feeds the same de-duplicated set, and existing
produce/verify fixtures pass across the change); no untrusted value reaches an
operator surface unquoted (`%q` on the row key and kind, extractor errors are
daemon-authored, and the doctor's finding detail is a status string, not this
error); the tolerance grants no new capability to a writer who can already
insert an outbox row, since before this change the same row crashed the daemon;
a race between the live probe and the serialized snapshot is caught by the
seal guard, which also records the verdict.

Codex review, round 1 (one finding, blocking, accepted). The startup ordering
defeated the posture in production mode: `run()` reconciles orphaned Claude
writers before it maintains backup evidence, and that reconciliation resumes a
writer through the unattended admission gate, which reads backup health. With
the live verdict still unset and a valid pre-downgrade checkpoint on disk,
health answered healthy and admitted work the gap was supposed to hold. Fixed
on both axes: the first maintenance pass now runs before the driver wiring, and
`NewProducer` marks the closure refused until that pass completes, so the
refusal no longer depends on where in a startup sequence the pass sits. The
second half is what closes the class; the reorder alone would leave the next
startup-sequence edit free to reopen it.

Mutation check, against committed state, eight branches disabled one at a time:
the unregistered-kind gap, the live verdict publication, the restore guard, the
stored-checkpoint verification guard, the startup tolerance, the seal guard,
the legacy-cleanup omission, and the encrypted health source's redundant
snapshot term. Seven of the eight failed a test with the branch disabled. The
eighth (the encrypted health source re-checking the snapshot's own gap) proved
unreachable behind the verification guard above it and was removed rather than
left as untested defense; the plaintext-path health source keeps its check,
where no manifest makes it load-bearing.

Not run: `LocalBackupProducer.Run`'s tolerance of the sentinel is unpinned. The
loop's poll interval is a package constant with no seam, so a test would idle
an hour; the branch is three lines over an error already covered at both other
callers.

## Revisit When

The outbox grows to where a per-pass live scan costs real time (it currently
reconstructs every retained row hourly), or a second component needs the
live-closure verdict. Both point the same way: move the verdict into
`Store.BackupHealth` with the extractors on `store.Options`, as its own
contract unit, which also closes the `freesided doctor` boundary above.
