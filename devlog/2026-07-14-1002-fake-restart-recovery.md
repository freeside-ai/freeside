# Durable permanent fakes: daemon-restart recovery (#34)

Wave 0 adversarial review of #9 found the permanent fakes prove only an
in-process provider-session loss (an `Outcome` flag), not the plan's
process-restart boundary: their "crash-surviving" committed registry was an
in-memory map that dies with the daemon. This unit makes the provider half of
the §5.3/§5.9 boundary real. Closes #34 for acceptance 1–3; acceptance 4 (kill
the real daemon) is escalated, not met (see Deferred).

## Decisions

- **Three durable facets, not two.** Chose to persist scripts, committed
  results, **and committed intents** (the `StartSpec`/`ReviewRequest`) over the
  old two-store (session vs committed-result) model, because without a durable
  intent, "kill before dispatch" (re-Start must succeed) and "kill after intent
  before result" (re-Start must be a duplicate) are indistinguishable after
  reconstruction, yet acceptance 2 requires separating them. Session progress
  stays in-memory: reconstruction *is* the provider-session loss, so every
  intent without a committed result reads gone/ErrNoResult while a committed
  result stays collectable by id.
- **Opt-in file backing over always-on or an injected store.** `NewXAt(dir)`
  persists one atomic-rename JSON file per fake; plain `NewX()` stays in-memory
  (unchanged signature, zero churn to the 13 call sites). Rejected an injected
  persistence interface: the exec-interfaces entry already rejected an exported
  in-memory ledger as speculative surface (`2026-07-13-1957-exec-interfaces.md`),
  and the same logic applies. File backing (not an in-memory shared store) is
  what literally outlives a real process kill, so it is ready for the #41 harness
  unchanged.
- **Reconstruction = restart, symmetric across both fakes.** A review that
  finished execution but never committed a result is *lost* on restart, exactly
  like an in-flight stage, rather than re-delivered from the script. Chose
  symmetry (only committed results survive) over modelling the forge's
  "findings still fetchable" nuance, because the plan asked for stage/review
  symmetry and at-most-once holds either way; a test that wants survival scripts
  the commit before the restart.
- **ReviewSource gains the stage `Outcome` vocabulary** (reused, not a parallel
  enum): fail/crash-before → ErrNoResult, crash-after commits then loses the
  session. Empty `Outcome` normalizes to complete, so the six existing review
  scripts are unchanged. Poll/Verify report the no-result answer ("never" ≠
  "not yet") only once Inspect has *driven* the review to its failure/loss,
  never ahead of it: a review still consuming its inspect lag reads not-ready,
  matching Collect's discipline (Codex P2, folded into the review commit; the
  first draft leaked a scripted Fail/CrashBefore outcome through Poll before
  execution reached it).
- **Persist failure panics in every mutator, atomically.** Each mutator commits
  its in-memory change and the durable write as one step; on failure it panics
  rather than returns, so no half-committed state (an in-memory intent the disk
  never got) is left for a caller to diverge on. Chose panic over the reviewer's
  suggested in-memory rollback because a persist failure is a broken test
  environment, not a scripted scenario: the project fails fast and loud on
  data-integrity conditions (a fixture is not a runtime boundary to degrade at),
  it unifies the model with `Script` (already panic), and it is less error-prone
  than six bespoke rollbacks. (Codex P2 round 2; the first draft returned the
  error after mutating, leaving the divergence.)

## Deferred

- **Acceptance 4 (real SIGKILL harness) -> Refs #41.** No daemon binary, engine,
  or store-backed acceptor exists (Wave 2 / later Phase 1A; exec is "no
  persistence by design"). The fake half is done and its `dir` is what the
  harness reuses; the test-local `acceptor` becomes the store inbox/outbox
  ledger. Filed now with its marker (won't drain in a session or two).

## Verification

- Determinism is a *test*, not just a claim: two dirs driven identically write
  byte-identical `stage_state.json`, guarding against a stray clock/nondeterministic
  key. Skipped a golden testdata file (plan's optional item): the format has no
  external reader yet, and the determinism test already pins non-determinism;
  add a golden when #41's harness freezes it as a contract.
- Queue swept: no open promote/deferred item touches exec/fake; the standing
  Phase 4 license ADR-candidate (`-> Refs #18`) is out of scope, untouched.

Scope: `daemon/internal/exec/fake`.
