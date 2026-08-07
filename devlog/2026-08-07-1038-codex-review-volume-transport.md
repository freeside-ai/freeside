# Codex review snapshots move from host binds to a read-only volume

Work unit: #591 (`fix/codex-review-volume-transport`). Revises the frozen
topology note [2026-08-02-2259-codex-review-topology.md]. Mandatory note:
credential trust-boundary change revising a recorded topology decision.

## Decision

Deliver the two admitted Codex review snapshots (`auth.json`, `AGENTS.md`) to
the review container over one per-invocation, ward-owned, read-only named
volume, and retire host binds (`MountBind`) from ward entirely. The snapshot
*model* is unchanged: immutable, digest-bound, refresh-token-free, mutation of
the source identity only outside the container under `auth_store_mutation_lease`.
Only the *transport* moved.

## Disproven assumption

The frozen topology note assumed Apple `container` supports read-only
single-file bind mounts. It does not: `container` 1.1.0 rejects any bind source
that is not a directory (`path '…auth…json' is not a directory`), by design, in
its mount parser. The two single-file binds failed at `CreateContainer` on
first live contact (#482 production proof). File binds were a second delivery
mechanism validated only by the fake runtime; volumes are the one transport ward
already proves live end to end (workspace seeding, shadow volumes, observer
proofs, and `LaunchStateClaudeClean` read-only credential state).

## Why volume, not a staged-directory bind

Rejected: staging the two files into a directory and binding that directory.
It reintroduces a host-bind transport (an explicit non-goal), keeps two delivery
mechanisms alive, and still fails the systemic gap that caused this bug — a spec
shape the fake runtime accepts but the live runtime rejects. Converging on the
one live-proven transport and teaching the fake runtime Apple's directory-only
rule closes that gap and makes the seam role-generic for a future
Claude-as-reviewer.

## What preserves the trust boundary

- Least exposure: a nonce-bound, networkless, read-only observer proves the
  volume holds *exactly* `{auth.json, AGENTS.md}` as regular, unsymlinked files
  and reports their sha256; the gate fails closed on any extra, missing,
  changed, redirected, or non-regular entry (exact `find` match under `LC_ALL=C`,
  symlink rejection, digest parse).
- Content binding: the observed per-file digests are tied to the bytes admission
  re-reads from `InputRoot` at build *and* at the final pre-start reconstruction,
  so a volume seeded with divergent content cannot start.
- Read-only delivery: the snapshot volume is mounted read-only; the review
  command symlinks the two files from it into the writable `CODEX_HOME`, so the
  credential bytes stay immutable while `CODEX_HOME` stays writable.
- Seeder hygiene: credential bytes transit only a ward-owned, networkless,
  pinned-image seeder proven absent afterward; the host stage is a 0700 dir with
  0400 files, wiped on return. Same trust shape as workspace seeding.
- Lease + lifecycle: the snapshot volume joins the exclusive volume lease and its
  atomic Start transfer; cleanup, terminal collection, and recovery all destroy
  it, so no credential material outlives the invocation.

## Two refinements beyond the issue plan (accepted by decision)

1. **Lease reconstruct generalization.** The real review container multi-mounts
   the `.agents` shadow at many targets (plus the snapshot and workspace once
   each), so `reconstructTransferLocked`'s old "one mount per leased volume"
   count was already wrong for the real shape and would have rejected a genuine
   post-Start recovery. Relaxed to the coarser, still-closed invariant: every
   leased volume attached at least once, no non-leased volume attached, single
   ownership-proven transfer container. Strictly more correct; containment
   preserved.

2. **Restart backward-compatibility.** The persisted #482 round-2 intent has the
   pre-snapshot six-resource shape. `validatedResourceNames` accepts it as a
   cleanup-only legacy generation (`preSnapshotCodexReviewNames`), keyed off an
   empty `snapshotVolume`; new intents carry the nine-resource shape. This lets
   #482 restart, recover the old intent, and open a fresh v2 intent without
   rewriting the round-1 contradiction, the #582 recovery transition, or the
   round-2 request.

Also: the topology version is bumped `v1 → v2`, so a prepared/final binding
written under the bind shape cannot validate against the new mount shape. No
such binding persisted for #482 (it never crossed the handoff boundary), so
nothing is stranded; a pre-#591 *started* binding is unreachable because the
feature never ran to completion in production.

## Refute-first verification

Three refute passes (the author's own read of the trust-boundary code, a
fresh-context adversarial reviewer prompted to disprove, and Codex's automated
PR review on #595) tried to break eight invariants: least exposure, content
binding through final reconstruction, writability/refresh-token, seeder
credential handling, lease generalization, restart legacy acceptance,
bind-retirement completeness, and teardown of legacy bindings. The first two
found none broken; six Codex passes surfaced findings on the least-exposure
invariant across successive pushes (below). One was a fail-open in the launch
prologue (fixed). Three were successive layers of the SAME #591 host-credential-
stage cleanup surface, which drove a design reframe rather than a fourth patch
(fixed, ending in the `ExportRoot` boundary move). The fifth exposed that the
recovery *block* under those residues is **pre-existing** and belongs to a
different surface (the lease/recovery ordering): the #591-new host-file residue
was hardened here, and the block itself was deferred to #599 - the deeper
boundary move was correctly declined as out-of-scope. The sixth reviewed that
hardening and found two convention-alignment defects in it (a fail-open on the
wipe error, and durable classification of transient staging I/O), both fixed to
match this file's established patterns. Independent verification:
`go build ./...`, `go vet`, `golangci-lint` on `internal/ward` (0 issues), and
the fresh (uncached) ward test suite all pass; the only failing test is the
off-scope `cmd/freesided` worktree `-buildvcs` quirk, which passes in a normal
checkout.

Findings, confirmed and fixed:

- **Confirmed P1 fail-open, fixed:** the review container's launch prologue ran
  under `set +e`, so the `ln -s` that installs the two snapshot symlinks into
  `CODEX_HOME` swallowed its failure. A derived or updated review image that
  already shipped `auth.json` or `AGENTS.md` at those fixed paths would make the
  link silently fail and leave `codex exec` reading the *image-provided*
  credential/instruction instead of the digest-bound snapshot volume, defeating
  the least-exposure and content-binding invariants; the image conformance probe
  checks launch metadata, not the absence of those rootfs paths. Fixed by running
  the setup prologue under `set -e` (relaxing to `set +e` only immediately before
  `codex exec`, so a nonzero codex exit is still captured), so a pre-existing
  target aborts the container before codex runs. Guarded by a new CI-blind live
  regression (`TestLiveCodexReviewSnapshotPreambleFailsClosedWhenImageShadowsCredential`)
  that runs the exact production command and asserts no output artifacts appear
  and the image file is left untouched.
- **Confirmed credential-leak on a mid-seed crash, fixed by a design reframe
  (three same-class Codex rounds):** because Apple `container` forces seed-via-
  copy, `seedCodexReviewSnapshot` must first write the plaintext `auth.json` to a
  host directory before copying it into the networkless seeder. A daemon killed
  between that write and the deferred wipe orphans the plaintext credential on
  disk, at a path outside the runtime resource set recovery reaps. Real but narrow
  (dir `0700`/file `0400`, so same-uid only; a precise SIGKILL window; marginal
  over the admitted snapshot already at rest). Fixing *where cleanup finds the
  stage* took three rounds, and the recurrence, not any single miss, is the
  lesson: cleanup must derive the path from **trusted, stable, re-derivable
  state**.
  - **Rejected anchor 1 - random `os.MkdirTemp`:** the original path was random,
    so recovery could not find it at all. Rejected: a crashed run's stage is
    unreapable.
  - **Rejected anchor 2 - deterministic under the mutable `InputRoot`:**
    a runID-keyed subdir under `cfg.InputRoot` is reapable in the common case, but
    the launch path requires an absolute `InputRoot` while recovery permits a
    changed or empty one (empty is valid in `attended_dev`), so a restart under a
    different `-review-input-root` reaps the wrong path and closes the intent while
    the credential remains. Rejected: cleanup bound to mutable config.
  - **Rejected anchor 3 - persist the launch root on the intent
    (`SnapshotStageRoot`):** persisting the original root made recovery
    root-independent, but the root was then a value *decoded from the journal* and
    only `cleanAbs`-checked. Under this codebase's rewritten-journal threat model
    that violates "never trust a decoded bit; re-derive from trusted state; fail
    closed" (AGENTS.md daemon conventions): a rewritten record could redirect
    `RemoveAll`, and an invalid value silently skipped cleanup while still closing
    the intent. Rejected: reintroduces a decoded trust bit.
  - **Final design - stage under the trusted daemon-owned `ExportRoot`:** both
    `seedCodexReviewSnapshot` and `recoverCodexReviewIntent` derive
    `codexReviewSnapshotStagePath(b.cfg.ExportRoot, runID)` from the Backend's own
    trusted config (validated `cleanAbs` at init, stable across restart,
    independent of `-review-input-root`), never from the journal. Recovery
    re-derives the exact same path, so it fails closed - it always knows the one
    root - with no decoded field to authenticate. Reap is unconditional and
    idempotent (`RemoveAll` no-ops when absent, so a legacy pre-#591 intent is a
    harmless no-op), best-effort with the error joined as operational; the seeder's
    `defer` wipe error is surfaced. `createPrivateStageDir` defeats a residue or
    pre-created symlink under a possibly-shared `ExportRoot` (remove-and-recreate,
    then `Lstat`-verify a real, unsymlinked, 0700, euid-owned directory before the
    0400 write). The `InputRoot`-based staging and the `SnapshotStageRoot` intent
    field were both fully reverted, so the commit reads as one clean design; the
    persisted intent shape is unchanged from pre-reframe (run 482's record and the
    legacy generation are unaffected). Guarded by unit tests: a crash-residue stage
    is reaped; the reap holds across a changed and an empty `-review-input-root`;
    and the symlink/residue pre-attack is defeated.
- **Confirmed PRE-EXISTING recovery-block (round 5), host residue hardened here,
  the block itself deferred to #599 (accepted by decision):** the fifth Codex pass
  flagged that a crash in the seeder window leaves a leftover snapshot-only prep
  container, so `recoverCodexReviewIntent`'s three-volume
  `RecoverCodexReviewVolumeLease` hits `reconstructTransferLocked`'s
  some-but-not-all rejection (`len(attached) != len(volumes)` →
  `ErrCodexReviewVolumeLeaseForeignOwner`) and returns before reaping. Confirmed
  **pre-existing**, not a #591 regression: the old reconstruct at `f0b70f10`
  rejects a leftover single-volume prep container (workspace observer, shadow
  initializer, or shadow observer, each attaching one of the two pre-#591 leased
  volumes) the same way; #591 only adds the seeder/observer prep windows. The one
  #591-new consequence is that the seeder window stages a plaintext credential, so
  a blocked recovery there strands it. Split accordingly:
  - **Hardened in this PR (host file):** `recoverCodexReviewIntent` now
    `RemoveAll`s the ExportRoot-derived host stage **before** the lease gate,
    needing no lease/claim/owner proof because it is the daemon's own host file
    derived from trusted `b.cfg` + a validated runID. A crash in the seeder window
    can no longer strand the plaintext `auth.json` **file**. It **fails closed**:
    on a wipe error it returns an operational (retryable) error before acquiring
    the lease or closing the intent, so the reconciler retries and the intent
    stays open until the wipe succeeds (a transient FS error can never close the
    intent over a surviving credential, since the closed-intent path never
    revisits the wipe). Guarded by two unit tests: a lease-blocked recovery still
    wipes the stage; a wipe *failure* keeps the intent open. Round 6 found and
    fixed two convention-alignment defects in this hardening's first review: (A,
    P1) the initial "defer-join and continue" closed the intent over a failed
    wipe - corrected to the fail-closed return above; and (B, P2) the seeder's
    host-filesystem staging failures (stage create/write/sentinel/copy) were
    `failf` (durable `ConformanceFailure`), so a transient full disk would durably
    terminate the review - reclassified to `codexReviewOperationalCheckf`
    (retryable, check context retained), matching the seeder's other operational
    steps. Both align the new code with this file's established conventions
    (fail-closed on credential cleanup; operational classification for transient
    host I/O), so they were convergent fixes, not another turn on the surface.
  - **Deferred to #599 (the block itself):** the partial-attachment recovery block
    is pre-existing, spans all prep windows, and belongs to the lease/recovery
    ordering, which is out of #591's declared scope (volume transport). Until #599
    lands, a blocked recovery leaves the seeded snapshot **volume** holding
    `auth.json` (a runtime object, reaped once recovery is unblocked).
  - **Option 1 (eliminate the host stage via exec-stdin) rejected:** the `Runtime`
    interface exposes no exec primitive (`CopyIntoContainer`, a host-*directory*
    copy, is documented as the only host-to-guest data path besides argv/env), and
    `CLIRuntime` wires no stdin. Streaming the credential in would need a **new
    runtime-contract host-to-guest primitive** (`Runtime.Exec` + `container exec
    -i` + fake impl + re-establishing the "widens nothing" trust argument for a new
    data path) - disproportionate, and it would not fix the recovery block anyway
    (the seeder container still exists during the write). Rejected on cost and
    because it addresses the wrong layer.

**Sustained-rounds outcome.** Four Codex rounds (2-5) all converged on the same
consequence - a crash leaving a credential residue - through four mechanisms
(random path, mutable root, decoded root, lease block). Rounds 2-4 were genuine
in-scope layers of #591's own new host stage and were fixed, ending in the
`ExportRoot` boundary move. Round 5 exposed that the *recovery block* under those
residues is pre-existing and belongs to a different surface (the lease/recovery
ordering); the deeper boundary move (option 1) was **correctly declined** as
out-of-scope and disproportionate, the #591-new host-file residue was hardened,
and the pre-existing block was tracked as #599. The recurrence was the signal to
stop patching the same surface and split scope, not to keep folding.

Two further findings, both **non-blocking and accepted by decision**:

- **Confirmed, fail-closed, unreachable:** `validateForTeardown` now requires the
  snapshot binding fields, so a pre-#591 *started* binding would be refused at
  teardown. A binding persists only after the review container's `CreateContainer`
  succeeds, which is exactly the step that failed on Apple container before this
  fix, so no such binding exists in production (run 482 sits in `preparing` with
  no binding and is handled by the recovery path, which *does* accept the
  six-resource legacy shape). If one somehow existed it fails closed (refuses
  teardown, leaves the object visible), never a credential exposure.
- **Accepted, redundant with a tested boundary:** the snapshot seeder *script*'s
  own exactly-two-files guards (`exit 92/93/94`) have no unit coverage; the fake
  runtime models a permissive seeder and relies on the observer gate. This is
  writer-side defense-in-depth that is fully redundant with the read-only
  observer proof, which *is* the real least-exposure boundary and *is* covered by
  both unit and the live regression. A seeder bug that wrote a third file is
  caught downstream by the observer (`valid=invalid` → seed fails), so the gap
  cannot cause a security hole. Not worth expanding this unit; left untested by
  decision.

## Revisit when

- The live `FREESIDE_WARD_LIVE_TEST` regression runs on Apple `container`: if the
  pinned Codex CLI refuses to read `auth.json` through a symlink into a read-only
  mount, fall back to an in-container copy into `CODEX_HOME` with the volume
  still the immutable source of record (an implementer decision to surface, not
  silently take).
