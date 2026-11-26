# Hold a Restarted Intent When GitHub Can't Be Reached

After a restart, the stage driver's commit-author cache is empty, so startup
`Reconcile`'s current-policy re-check of a seeding, running, or exported
intent resolves the GitHub App bot identity over the network. Any failure
there read as a refusal: the intent was quarantined for the life of the
process, and the check ran under the driver's lock. #3381 makes an
unreachable GitHub a hold and runs the check off the lock. The work is Tier
A by path (`internal/exec/stage/pipeline.go`, `state.go`, and
`internal/admissionauth/`).

## Decisions

- **The authority classifies, the driver maps.** `admissionauth`'s
  commit-author check is the one place that knows a failure came from
  GitHub, so it wraps a new stage sentinel, `ErrAuthorityUnavailable`, and
  the re-gate in `state.go` maps that sentinel to `ErrRecoveryRetryable`.
  `heldClaudeRecovery` in `daemoncompose` was the other candidate, but only
  `Reconcile` consults it. `Inspect`'s restart check (#3268) sends any error
  that isn't `ErrRecoveryRetryable` to the engine, where it fails the pass.
  Mapping in the re-gate covers both readers with one rule.
- **The boundary is narrow.** Unavailable means a `net.Error`,
  `context.DeadlineExceeded`, or a `publish.APIError` with status 408, 429,
  or 5xx, and never an error carrying `publish.ErrAppBotIdentityMismatch`.
  Every other failure (token coordinates, keystore, decode, 404) stays a
  refusal. `daemoncompose`'s `transientExternalFailure` was not reused: it
  counts 403 as transient, and `daemoncompose` imports `admissionauth`.
- **403 stays a refusal, secondary rate limits included.** A seeding or
  running hold retries without a bound (#385), logs only at Debug, and
  raises no item. A permanent 403, such as a suspended App, would then hold
  silently forever. As a refusal it quarantines and raises the
  `stage_intent_quarantined` health item. A rate-limited 403 costs a
  quarantine until the next daemon start.
- **The listing checks admission only, and `Reconcile` runs the current
  check per intent with the lock released.** `listIntent` now calls
  `regateAdmission`, which reaches no network. For a terminal phase it checks
  the same things as the full re-gate. `Reconcile` runs `regate` after the
  live skip and before the cordoned hold, so current policy's refusal of a
  cordoned running intent still quarantines it. The quarantine causes are
  unchanged. `TestCurrentPolicyRequiredForWorkAndImportRetries`, a Tier A
  gate test, moved from the listing to `Reconcile` with the same assertion.
  The startup wait stays: `Reconcile` still runs before the engine loop.
- **Dispatch is unchanged.** The engine's start-authority check (#3224) calls
  the driver's `AuthenticateStart` wrapper, not the re-gate. It still
  refuses a start on the same failure.

## Costs Accepted

- **An exported intent's re-check retries without a bound.** The #1181 grace
  bounds a failing import inside recovery. A current-policy check that
  can't reach GitHub returns before recovery, so a long outage keeps an
  exported intent in its slot with no item. The issue left bounding the
  retry as a non-goal. The fresh-context review raised it, and follow-up
  #3654 tracks it.
- **Any `net.Error` counts as unreachable.** `*url.Error` is a `net.Error`,
  so a TLS verification failure or a bad base URL holds instead of
  quarantining, and such a cause can be permanent. The contract names
  `net.Error`, and splitting it by cause would need a list of TLS and DNS
  error types. The same follow-up covers it.
- **A live intent isn't re-checked by `Reconcile`.** The live skip now runs
  before the check. It matters only if a `Start` races startup `Reconcile`,
  which runs before the engine loop, and `Start` ran the full check itself.

## Refute-First Record

The agent's own checks and a fresh-context adversarial reviewer each tried
to prove the change wrong:

- An unknown verdict treated as a pass: disproved by reading and by the
  reviewer's trace. Every unavailable path returns an error carrying
  `ErrRecoveryRetryable`, and recovery starts only after a nil `regate`.
- A refusal that isn't unavailable getting held: disproved.
  `TestReconcileHeldFailuresNoLongerStopThePass` and
  `TestRestartCurrentCheckRefusalBlocksRecovery` pass without changing what
  they assert. The classifier table refuses 403, 404, an identity mismatch
  that wraps a transport failure, and an unclassified error.
- Other `loadIntent` callers (`Collect`, `Stream`, `Close`, the locked
  `Inspect` path) misreading the new retryable error: disproved by reading.
  Outside the stage package only `heldClaudeRecovery` reads
  `ErrRecoveryRetryable`, and the locked `Inspect` path maps it to running.
- The off-lock check reading state that a concurrent recovery changes:
  allowed. The only production caller (`daemoncompose/startup.go`) runs
  `Reconcile` before the engine loop, so no `Inspect`-driven recovery runs
  beside it, and `reconcileIntent` still re-reads the phase under the lock
  before recovering.
- The new tests passing without the fix: disproved by mutation. Removing
  the re-gate mapping, the identity-mismatch exclusion, the off-lock move,
  or `Reconcile`'s per-intent check each fails a new or moved test.
- Data races: none under `go test -race` of the stage and `admissionauth`
  packages.

## Revisit When

- `regateAdmission` gains a network call; the locked listing would then
  stall again.
- An unavailable hold is seen lasting long enough to matter, or a permanent
  `net.Error` cause is seen holding. Then bound the retry or narrow the
  classifier (#3654).
- Startup moves `Reconcile` after the engine loop starts; the off-lock
  check would then race `Inspect`-driven recovery.
