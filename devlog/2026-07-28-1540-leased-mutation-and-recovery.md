---
run: manual
stage: wave2-1a2
date: 2026-07-28
branch: feat/leased-mutation-recovery
---

# Leased Auth-Store Mutation and Restart-Safe Handoff Recovery

Work unit #303 (mandatory note: credential-leak surface, destructive
path, and a reconstruction trust boundary). The two halves share one
design center: every trust decision is made from acquired-and-verified
or persisted-and-re-gated evidence, never from a caller's claim or a
decoded record.

## Decisions

- **The gate owns the lease lifecycle.** Ward acquires, re-verifies,
  and releases the §5.4 mutation lease itself rather than verifying a
  caller-held one: only the gate knows the handoff deadline and when
  the writer is provably absent, so it sizes the window past
  `HandoffTimeout + TeardownTimeout` plus a margin and ends the
  audited window at teardown. A caller-held lease verified once at
  preflight could lapse mid-run invisibly. Rejected: caller-supplied
  lease with ward-side verification only. Consequences: no mid-run
  renewal exists (the window covers the budget), and the pre-start
  re-verification (same holder, same fence, live) is the last gate
  before mutation ability exists.
- **Release only after writer absence is proven.** Teardown releases
  the lease only when its own fresh evidence shows the writer gone
  (`writerGone`); otherwise the window is kept held and recorded as a
  teardown problem, with expiry as the backstop. Releasing beside a
  possibly-live writer would invite a second holder to mutate in
  parallel — the exact §5.4 violation the lease exists to prevent. In
  recovery the bar is stricter still (review round 3): the release
  waits for the full-token orphan audit, not teardown's name-keyed
  writer check, so a token-carrying survivor under an unexpected name
  keeps the window held.
- **`ErrLeaseWindowEnded` tolerance.** A release refused because the
  window already ended (released, expired, taken over) converges as
  nothing-left-to-release in both teardown and recovery; any other
  release failure stays loud. Without this, every crash recovery that
  outlives the lease window would fail forever. The store's lease row
  remains the audit trail for how the window actually ended.
- **Digest-level mutation attestation.** Two observer VMs digest the
  leased volume through read-only mounts before the writer starts and
  after it is proven absent (the base observer's exact discipline:
  nonce-bound proof, exported rootfs, strict parsing, never echoed).
  This attests *that* the store changed, never *what* changed.
  Rejected: semantic diffing — it would require reading credential
  material into the daemon; the §5.4 export scan remains the content
  control.
- **The journal records identity and unreconstructible proofs, not
  progress.** After a restart the only trustworthy account of how far
  a handoff got is the runtime world classified by the persisted
  ownership token; persisted per-stage progress bits would be decoded
  trust bits recovery would be tempted to believe instead of
  re-observing. What must be persisted is what cannot be rebuilt: the
  token itself, the spec digest, the held lease reference, the two
  pre-writer facts (observed base, credential pre-digest), and
  writer-complete (check-3 absence plus the egress proxy's
  process-local health). Rejected: a per-stage progress log.
- **Nothing pre-writer-complete is adoptable.** The daemon-side
  CONNECT proxy dies with the process, so a still-running writer has
  no enforced egress and even a stopped one lacks the
  proxy-healthy-throughout proof; adoption would deliver work below
  the gate's proof floor — the same trap as the refuted same-VM
  fallback class (§5.7). Recovery for those states is teardown plus a
  durably committed loss. Rejected: adopting a stopped-but-unmarked
  writer's workspace.
- **Recovery re-earns every release check.** A leftover exporter's
  pre-execution inspection died with the process, so it is reaped
  (ours-only evidence) and the role rerun through the same
  `runExporter` body the live path uses; the caller re-supplies the
  spec from its durable admission, bound by the record's digest.
  Rejected: trusting a finished exporter's rootfs.
- **Loss is prove-then-persist.** `Close(loss)` is written only after
  teardown's absence proofs and the ownership-token orphan audit over
  full listings both pass; a nil-error loss return is the caller's
  rerun-safe signal, and a recovery error commits nothing and leaves
  the record open for retry. Same discipline on the release side: a
  completed-close failure voids the delivery (release follows the
  durable append).
- **The post-write attestation dies with its window.** (Round 11;
  supersedes rounds 5-8's fresh-window observation design.) Recovery
  observes the store only inside the run's own still-held window, and
  only when that window outlasts a full observation budget; otherwise
  the attestation is reported as lost (`PostAttested` false, the
  digest never taken) — a process-window-local proof, like the egress
  proxy's health, is never recreated, because a fresh observation
  would attribute intervening holders' mutations to this run's window.
  Observer failures stay retryable, never a committed loss. Rejected:
  acquiring a fresh serialization window for the observation (it
  serialized the read but could not restore the state the writer left,
  misattributing later holders' changes, and spawned the window-leak
  and convergence-deadlock hazards rounds 7-8 patched — all deleted
  with it); observing unserialized; skipping silently (the loss is
  recorded, never silent).
- **The journal binds the lease by its window.** The persisted lease
  reference carries the acquired window (`AcquiredAt`, `ExpiresAt`),
  and recovery adopts the recorded window only on its exact equality
  with the live store row: fence plus a decoded ordering claim
  (`OpenedAt`) can be carried by a damaged row pointing at a later
  same-holder window, so no decoded ordering claim is load-bearing. A
  row forging the later window's complete tuple is an adversarial
  journal, outside the damage class this defends (the journal is
  daemon-owned). Rejected: proving acquisition order from decoded
  `OpenedAt`.
- **The stack is maintained as fully folded logical commits.** Owner
  decision (2026-07-28): review fixes fold into the concern commit
  they harden, even across the interleaved recovery commits; the
  per-round no-fold rationale earlier review commits recorded is
  superseded. History reads as the unit's logical concerns, each in
  its final hardened form.
- **The journal is optional in Config.** Nil preserves the one-shot
  semantics byte for byte; requiring a journal is the caller's
  operating-mode policy (#237 wires the store adapter and decides).
  Same posture as `ConformanceRecorder`: ward defines the seams
  (`AuthStoreLeaser`, `HandoffJournal`), the daemon wires them, so
  this unit ships with no production caller — deliberately, per the
  unit's non-goals.

## Verification Findings

- The credential observer's shell script executes in two lanes: the
  unit lane runs it under a local shell (symlink attestation across
  create/retarget/remove, digest determinism), and the live test
  remains the only in-VM execution under the pinned exporter image; it
  passed on Apple container 1.1.0, attesting a real mutation and a
  real read-only refusal in one run.
- Review round 6 (Codex P1, accepted): the re-gate's acquisition-order
  proof leaned on decoded `OpenedAt`; the lease reference now persists
  its window and the re-gate binds by exact window equality (see
  Decisions).
- Review rounds 7-8 (Codex, all four accepted): the recovered
  observation window releases the moment the observer finishes (the
  amended decision above), lease releases run on a detached
  teardown-bounded context so a cancelled run cannot leak the window
  to expiry, adoption refuses retryably beside any token-carrying
  stray container before observing or re-leasing (reaping nothing it
  cannot name), the exporter's lifecycle failures are retryable with
  only verifyExport's refusals committing loss (the observer rule
  applied to its sibling stage), and the credential digest's
  enumeration is complete: symlinks, then FIFOs, sockets, and devices,
  then every entry's mode, owner, and group (rounds 9-10) — the
  round-4 class closed by enumerating content, paths, kinds, targets,
  modes, and ownership. Round 10 also inverted recovery's loss
  default: a committed loss requires the explicit content-evidence
  mark (`evidencef`) on the refusal, so operational I/O anywhere in
  verification — and any unmarked future failure site — defaults to
  retryable; the destructive direction is opt-in. Declined within it:
  distinguishing a scanner infrastructure error from a scan refusal
  (the OutputScanner contract is any-error-fails-closed; a scanner
  error taxonomy is gauntlet's contract to change).
- Review round 11 (Codex, both P1s accepted; P2 accepted): the
  attestation-loss model above replaced the fresh-window observation,
  an instantaneous liveness check became the observation-coverage bar,
  and the default `HandoffTimeout` now budgets the two credential
  observers (4 -> 6 SeedTimeout operations).
- Review round 12 (Codex, both P2s): the export byte cap is marked
  content evidence (a deterministic fact about the immutable
  workspace; leaving it retryable made an oversize output an
  ever-open record instead of a loss signal); the
  acquire-before-Begin crash window is deferred to #372 (expiry is
  the documented backstop; the fix is a journal-ordering design).
- Review round 13 (Codex, both P1s): blob verification splits verified
  content facts (missing, wrong size, wrong digest — marked) from host
  I/O (unreadable, read failure — unmarked, retryable in recovery),
  closing a mixed evidence site; `RecoveryOutcome` gains the
  conventional `valid()` predicate with a registration-pin test.
- Review round 14 (Codex, P1 and P2): the commit-plan stat error is
  split from the not-regular evidence (the last grouped operational
  branch), and the metadata pass records the hard-link count, so
  replacing an identical copy with a hard link moves the digest.
- Review round 15 (Codex): the metadata pass binds each path to its
  inode (`ls -ldi`), so a count-preserving relinking of equivalence
  classes moves the digest; the identity-to-volume binding re-raise
  keeps its round-1 disposition (deferred to #370, `kind:contract` —
  the binding needs trusted store state, outside this ward-only unit).
- Review round 16 (Codex): a missing required manifest is marked
  content evidence (deterministic absence in the just-written output);
  timestamp attestation declined — `ls` time display is unstable
  across its six-month format boundary (an unchanged store could
  digest differently between observations, a false `Mutated` in the
  audit record), a sound version needs a stat helper in the exporter
  image, and `touch -m` changes neither content nor access.
- Review round 17 (Codex, both P2s; the exchange closes here): a
  freshly opened window refused for short coverage is released rather
  than abandoned held to expiry (it is the one rejection past the
  freshness check, so provably this run's), and recovery's
  `HandoffTimeout` context now also bounds the journal and lease-store
  reads, so a stalling adapter cannot block restart recovery.
- Review round 18 (Codex): the post-teardown audit detaches like its
  sibling teardown (an expired context would spuriously void delivered
  work after the objects are gone); the decoded-WriterComplete
  re-raise keeps its round-2 disposition — the proxy-health half is a
  durable proof by design, the §5.7 conformance-row trust class,
  re-gated against the world as far as the world can answer.
- Review round 5 (Codex, both P1s accepted, class swept): the
  partially-gated re-read of leaser rows recurred across rounds 1, 2,
  3, and 5, so the boundary is now drawn at "any row a leaser read
  returns passes the acquire-strength gate", not per-site; the
  serialized recovered observation and the retryable observer
  failures are recorded under Decisions.
- The kill-boundary matrix takes its states from the real flow
  (snapshot at hook points, replay into a fresh backend), not from
  hand-modeled worlds; an in-process abort cannot model a kill because
  Handoff's deferred teardown always runs.
- `TestLiveConformanceSuite` fails identically on `main` on this
  machine (PreJob: missing required capability; environmental, the
  egress probe under the active VPN) — pre-existing, not from this
  unit.

## Refute-First Ledger

Two passes: self-review during implementation, then a fresh-context
adversarial reviewer over the full diff, prompted to refute (lenses:
non-leased-writable, unverified release, foreign reap, loss with
survivor, lease bypass, journal honesty).

Confirmed and fixed:

- Self-caught: teardown's early return skipped the lease release when
  no runtime object existed yet (acquire precedes preflight);
  restructured so an early refusal still releases.
- Self-caught: unconditional release in teardown would have freed the
  window beside a writer whose absence was unproven; the `writerGone`
  gate plus the kept-held problem record closed it.
- Reviewer: `Recover` took the whole record as a parameter — a stale
  copy's `WriterComplete`/`Outcome` would be a caller-supplied trust
  bit steering adoption. Fixed: `HandoffJournal.Get`, and Recover
  reads the durable row itself.
- Reviewer: any adoption failure (including a cancelled context)
  committed loss and the deferred teardown destroyed the
  writer-complete workspace. Fixed: loss only on conformance evidence;
  an erroring recovery decides nothing and destroys nothing, and a
  retry can still adopt.
- Reviewer: two concurrent handoffs reusing one holder ID converged on
  one §5.4 window (store-side same-holder idempotence), and the first
  teardown's release freed the identity beside the second live writer.
  Fixed: acquisition refuses a window not acquired at its own instant,
  plus an in-process per-identity slot for the same-instant collision.
- Reviewer: `Close(completed)` was durable before the export was
  locatable; a crash in the window left a completed record with an
  unfindable delivery. Fixed: `MarkExportMaterialized` records the
  export path durably before the close, in both paths.
- Reviewer nits: the credential proof now has its own scratch field
  (no `baseArchiveDir` sharing), and the observer-script comment
  states honestly that readability is not attested (unreadable
  digests like empty; `Mutated` is content identity, audit-only).

Rejected by verification (not re-raisable without new evidence):
writable-mount escapes via duplicate/colliding/renamed targets or a
runtime realizing different access (exact `sameMounts`); a planted
proof in the credential volume (nonce is the ownership token,
invisible in-VM); double release (Close-once + voided delivery on
close failure); loss over survivors (every teardown problem blocks
both closes; the token audit has fail-closed inspect fallbacks);
release beside a possibly-live writer (`writerGone` stays false on
re-list failure); foreign-object deletion (token-only evidence refuses
everywhere); journal mark ordering (each proof durable at the point it
is earned, intent-before-create at Begin).

## Revisit When

- #237 wires the store-backed `AuthStoreLeaser`/`HandoffJournal`
  adapters: revisit whether unattended admission must require a
  configured journal (the gate deliberately does not).
- A second runner backend implements recovery: the journal record
  shape (`SpecDigest` over ward's own spec JSON) is ward-internal
  today; a shared shape would be a `kind:contract` promotion.
- Renewal becomes necessary (a writer legitimately outliving
  `HandoffTimeout`): the no-renewal design assumes the window always
  covers the budget.
