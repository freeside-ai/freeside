# Lineup Probe on the Ward Runner

Issue #2869. Tier A by path (`daemon/internal/ward/`). Link 4 of the chain
that `devlog/2026-11-07-1956-ward-helper-role-runner.md` started (#2776);
it inherits the runner steps that
`devlog/2026-11-08-1716-codex-review-helpers-on-runner.md` recorded.

## Decision

**Moved the lineup probe container onto `runHelperRole`.** Only the
container's create-to-absence sequence moved. `verify` is
`verifyAgentAllowlist`, `afterStart` delivers the credential, and
`readProof` runs the export. The network, proxy, credential delivery,
export reader, and teardown stay in `lineup_probe.go`. The probe sets none
of link 3's optional steps, and the runner is unchanged.

**Chose `readProof` to carry the export over a new runner step.** The
export has to run after the stop and the memory-limit check and before the
delete, and `readProof` is the runner's only step there. The output leaves
through a closure, and the runner gets no proof bytes, so `checkProof` stays
unset. A runner step for one role would widen the runner for every role.

**Chose `CheckTeardown` as the probe container's check.** The probe's
absence verification already reported it, so the role's one check matches
what its last step said before. Runtime-step failures (create, pre-start
inspect, start, stopped wait, delete) become `ConformanceFailure`s for it.
Their causes stay in `cf.Err` and in the reason text, but leave the error
chain. No caller notices: `ProbeLineupImage`'s callers in `cmd/freesided`
return or wrap its error, and none calls `errors.Is` or `errors.As` on it.
The operator-facing text gains the conformance prefix.

**Accepted the order change.** The probe took its ownership fingerprint
before its allowlist check. The runner checks the allowlist first, so a
container that fails the allowlist is never bound as this run's object. Its
claim then has no fingerprint. Teardown's `reapUnlistedContainer` proves
ownership by the label alone, and the characterization row shows that
container reaped. The probe journals nothing, so no recovery path reads
the fingerprint.

**Accepted the new memory-limit failure.** The probe now runs
`helperStopped`, so a container killed at its memory limit fails with an
unwrapped `*MemoryLimitError` for `LaunchReview` at its size instead of
being exported and judged. Its output could be partial, as any helper's
could. A probe that runs near its limit now fails rather than reporting a
verdict.

**Gave ward one host-only network check, `attestHostOnlyNetwork`.** The
provider egress, the probe, and the Codex review observation each wrote out
the same three checks: the inspection names the network, its mode is
host-only, and it carries this invocation's ownership label.

- **It takes a label for the failure text, not fixed text.** Each caller
  keeps naming its own network, and `prepareProviderEgress` keeps its exact
  text.
- **It returns an empty fingerprint without refusing it.** A runtime may
  omit the creation date, and the handoff and the probe then rest on the
  ownership label. Only the Codex observation needs a non-empty
  fingerprint, so it keeps that refusal itself.
- **The probe's network refusals become `ConformanceFailure`s for
  `CheckAgentEgress`.** Their text is unchanged except the ownership
  refusal, which now reads "ownership is unproven".
- **The Codex observation's one "identity or mode diverged" refusal splits**
  into the shared name and mode refusals. Its ownership-label validity
  check, empty-fingerprint refusal, and gateway and proxy checks stay where
  they were. Every input it refused before, it still refuses.

**Left the other network reads alone.** The Codex lifecycle's network
creation fingerprints and journals its network before
`ObserveCodexReviewNetwork` checks the name and mode; that order is the
journal's, not this check's. The writer's pre-start re-check in
`handoff.go`, teardown, and recovery compare against an earlier
observation or a claim rather than attesting a fresh network.

## Evidence

`TestLineupProbeContainerSequence` was committed against the old code first,
with `testdata/lineup-probe-sequence.golden`. Each failure row records the
error class and check, content evidence, the container claim, whether
teardown succeeded, and what the runtime holds afterward.

- **Success trace: byte-identical.**
- **Failure rows: only the expected changes.** Create, pre-start inspect,
  start, stopped wait, and delete go from `plain` to `conformance:teardown`.
  The allowlist row goes from `fingerprint=set` to `fingerprint=empty`. The
  copy row stays `plain`, since delivery is the role's own step. Every row
  still ends `reaped`.

## Refute-First Record

The old-versus-new comparison is the characterization test above. A
fresh-context reviewer then tried to refute the diff and found no defect;
its coverage findings are recorded here.

- **A step reordered, dropped, or duplicated: disproved.** The success trace
  is byte-identical.
- **The golden can't show `helperStopped`'s boot-log read: confirmed,
  covered elsewhere.** The fake records boot-log reads apart from the call
  trace. `TestProbeLineupImageReportsAMemoryLimitKill` fails if the read is
  missing or runs after the export.
- **`ProviderReached` now read later: disproved as a change.** It is read
  after the delete and absence check instead of before the delete. The
  container has stopped by then, so no provider byte can arrive between the
  two reads, and the proxy stays open until teardown.
- **A caller depends on the probe's error class or chain: disproved by
  reading** the four `cmd/freesided` callers. A caller added later that
  checks `context.Canceled` on a probe error would miss a cancellation
  during the stopped wait; allowed, since none exists.
- **The Codex observation accepts something it refused: disproved.** The
  shared check refuses a wrong name, a non-host-only mode, unobserved
  labels, and a missing ownership label, and the empty-fingerprint refusal
  follows it. `TestAttestHostOnlyNetwork` pins the shared refusals, and
  `TestObserveCodexReviewNetworkFailsClosed` passes unchanged.
- **The empty-fingerprint refusal is unpinned: confirmed, fixed.** It now
  follows a check that accepts an empty fingerprint, so deleting it would
  admit a network with no creation date and no test would fail.
  `TestObserveCodexReviewNetworkRefusesAnUnfingerprintedNetwork` pins it.
- **No end-to-end test refuses a probe network without its ownership label:
  confirmed, allowed.** The shared check's test pins that refusal, and the
  probe's success tests fail if it passes any owner but its own.
- **The memory-limit kill leaks or is wrapped: disproved.**
  `TestProbeLineupImageReportsAMemoryLimitKill` checks the unwrapped error,
  its class and size, no export, and nothing left behind.
- **The create-failure row reports a teardown error: confirmed, allowed,
  unchanged.** Teardown inspects a container that was never created and
  fails before and after the move; nothing is left behind. It is outside
  this unit's scope.

Revisit when a probe-like role needs to judge or return what it exports
through the runner: give the runner a step for that rather than a second
closure.
