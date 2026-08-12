# Pace Transient Codex Cleanup Without Terminalizing

Work unit: #493. Mandatory note: authenticated destructive cleanup path.
Scope: `daemon/`, `devlog/`.

## Decision

Chose propagating transient Codex cleanup failures from `ReviewSource.Inspect`
over converting them to a nil-error pending status. The existing engine path
for a transient `ReviewSourceFailure` keeps the same invocation and review
round, sets its process-local retry deadline, and persists the exact retry in a
`ReviewRetry` row. The former pending result bypassed that path and retried
authenticated cleanup on every reconciliation tick.

Kept the review stage's outcome-before-cleanup protocol unchanged. Collection
or rejection still writes the durable outcome before teardown, cleanup remains
identity-checked and idempotent across partial deletion, and
`MarkCodexReviewOutcomeReady` still runs only after runtime and instruction
cleanup succeed. `Poll` therefore continues to hide both success and failure
outcomes while cleanup is incomplete.

Chose the shared review retry scheduler over a cleanup-specific attempt counter
or backoff subsystem. A cleanup retry overwrites the exact run, invocation,
round, base, and head binding; it does not write a terminal `ReviewFailure`,
advance the review round, or create a replacement invocation. The delay is the
existing round-derived value, capped at 256 seconds, while retry count remains
unbounded so a credential-bearing topology is never abandoned merely because
teardown has failed repeatedly.

## Rejected Alternatives

- **Keep returning `StatusPending` without an error.** This is the masking bug:
  the engine cannot distinguish cleanup failure from ordinary progress and
  never arms durable pacing.
- **Terminalize after a cleanup retry limit.** A terminal invocation is no
  substitute for authenticated teardown and could strand credentials or owned
  runtime resources.
- **Add a separate cleanup retry counter.** The existing scheduler already
  provides restart-safe same-invocation pacing, and a second state machine
  would duplicate identity and recovery rules without changing safety.

## Refute-First Findings

- **Confirmed and fixed:** four code sites masked a transient failure as
  `StatusPending, nil`: the shared post-collection cleanup helper, rejected
  outcomes, rejected decoded requests, and requests that decode but fail
  validation. A mechanical `daemon/internal/ward` sweep after the change found
  no remaining instance of that pattern.
- **Rejected by verification:** propagation does not terminalize or advance the
  invocation. The engine gate test observes the same round-one invocation,
  writes both the process-local deadline and durable `ReviewRetry`, starts no
  replacement request, and records no `ReviewFailure`.
- **Rejected by verification:** cleanup failure cannot expose an outcome.
  Ward tests hold the outcome unready and make `Poll` return
  `ErrResultNotReady`; after a successful retry, readiness flips exactly once
  and the owned container, volumes, and network are gone.
- **Rejected by verification:** a stale retry row cannot authorize or advance
  another attempt. Reconstruction deletes a row unless invocation, round,
  base, and head all match the current review; an exact matching row can only
  postpone retry.
- **Rejected by verification:** the rejected-request and rejected-outcome
  branches do not lose their terminal contradiction. Their first teardown
  failure is transient; once teardown converges, `Inspect`/`Poll` reports the
  already persisted contradiction.
- **Accepted by decision:** repeated cleanup failures in one review round use
  that round's fixed scheduler delay rather than increasing a cleanup-specific
  attempt counter. They remain durably paced and count-unbounded, which is the
  work unit's stated contract.

Revisit when the engine changes its error-before-status handling, when
`ReviewRetry` stops binding the exact invocation and candidate, or when Codex
cleanup gains an independently justified bounded-abandonment policy.
