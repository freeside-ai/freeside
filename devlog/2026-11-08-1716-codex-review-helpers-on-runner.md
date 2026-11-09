# Codex Review Helpers on the Ward Runner

Issue #2868. Tier A by path (`daemon/internal/ward/`). Link 3 of the chain
that `devlog/2026-11-07-1956-ward-helper-role-runner.md` started (#2776).

## Decision

**Moved the Codex review's seven helper roles and the Claude enrollment
credential seeder onto `runHelperRole`, and retired `runCodexReviewObserver`.**
The roles are the shadow initializer, snapshot seeder, and input seeder; the
workspace, shadow, snapshot, and input observers; and
`credentialVolumeAuthor.runSeeder`. Each keeps its staging, sentinels,
deferred reap, and the intent marks its caller makes before it runs. The
observers share `runCodexReviewObserverRole`, which owns the claim and reap
and builds the observer's `helperRole`.

**Chose three optional runner steps over a Codex-specific runner.** The Codex
roles did three things the runner didn't. Each step is a field a role may
leave unset, and the handoff roles set none, so
`testdata/helper-role-sequence.golden` is unchanged.

- **`journal`**, after the fingerprint and before start. Every Codex role
  journals its fingerprint there, so the durable record binds the container
  before it can run.
- **`recheckStopped`**, an inspection after `helperStopped` and before the
  proof read. The observers re-check the stopped container's shape, so a
  container whose mounts changed while it ran is refused before its proof
  is trusted.
- **`wrapRuntimeFailure`**, applied to the create, inspect, start, stopped
  wait, stopped re-inspect, and delete failures. The Codex roles mark those
  failures `ErrCodexReviewOperational`, so the review retries them.

**The wrapper skips the fingerprint failure, a role's own steps,
`helperStopped`, and the absence check.** Each of those can report a
contradiction: an ownership label that doesn't match, a refused allowlist or
proof, a memory-limit kill, or a container that won't go away. Marking one
operational would turn a contradiction into a retry. Every Codex role already
kept its fingerprint failure a plain `failw`, so nothing moved out of the
operational class. `codexReviewOperational` builds the same text
`codexReviewOperationalCheckf` built. The one difference is that the
`ConformanceFailure` now carries its cause in `Err`, which `Unwrap` doesn't
expose and no production code reads.

**Accepted the observers' order change, the contract's one intended
behavior change.** `runCodexReviewObserver` took the fingerprint and journaled
it before the allowlist check. The runner checks the allowlist first, as the
seeders and every handoff role already did, so a report that fails the
allowlist is never bound as this run's object. An observer whose pre-start
allowlist check fails therefore leaves no fingerprint on its claim and no
fingerprint mark in the journal. That is safe for both readers of that state:

- Teardown reaps the container by its ownership label. The characterization
  rows show every observer allowlist failure reaped, before and after.
- Recovery finds the container by the name and ownership token the caller
  journals before the observer runs, the state it already handled after a
  crash between create and the fingerprint mark.

Error text follows the runner's shared wording (for example "inspect observer
before execution", "delete stopped credential seeder"). No test or production
code matches the old reasons.

## Evidence

`TestCodexReviewHelperRoleSequence` and `TestClaudeCredentialSeederSequence`
were committed against the old code first, with
`testdata/codex-review-helper-role-sequence.golden` and
`testdata/claude-credential-seeder-sequence.golden`. Each row records the
error class and check, the operational mark (Codex), content evidence, the
role's last journal mark (Codex), and what the runtime holds afterward.

- **Success traces: byte-identical** through every move: the review launch's
  runtime calls and journal marks, and the enrollment's runtime calls.
- **Failure rows: one change.** The four observer allowlist rows go from
  `journal=fingerprint` to `journal=token`, which is the intended order change.
  Their error class, check, operational mark, evidence, and leftovers are
  unchanged. Every other row is byte-identical.

## Refute-First Record

The old-versus-new comparison is the two characterization tests above.

- **A step reordered, dropped, or duplicated: disproved.** Both success traces
  are byte-identical, including every journal mark's place between the
  pre-start inspect and the start.
- **An operational failure lost its mark, or a contradiction gained one:
  disproved for the characterized steps.** The `operational` column is
  unchanged on every row: create, pre-start inspect, allowlist, start, the
  stopped wait, and the journal mark.
  `TestHelperRoleWrapsOnlyRuntimeStepFailures` pins exactly which six steps
  the wrapper covers.
- **The golden misses delete, the observers' stopped re-inspect, the copies,
  the proof read, the absence check, and the memory-limit kill: confirmed,
  allowed.** The issue fixes the golden's steps. That each Codex role sets
  the wrapper, so its delete and re-inspect failures stay operational, is
  confirmed by reading only; no Codex-level test would catch a role that
  dropped it.
- **A memory-limit error now wrapped: disproved.**
  `TestHelperRolePassesTheMemoryLimitErrorThroughTheWrapper`. Every moved
  role's `spec.Size` is the size it passed to `helperStopped` before.
- **Claim state changed on failure: confirmed only for the observer allowlist
  step, allowed above.** Every other step sets `attempted`, `owned`, and
  `fingerprint` at the same point as before. `helper_role_test.go` checks the
  claim after a journal failure and a refused re-check.

## For Link 4

#2869 inherits the three optional steps. A role there that journals, re-checks
a stopped container, or marks runtime failures retryable sets the matching
field instead of writing the step out by hand.

Revisit when a role needs a step the runner lacks: add it to the runner
rather than writing that role's sequence out by hand again.
