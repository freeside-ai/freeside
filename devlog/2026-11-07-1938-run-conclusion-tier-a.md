# The Run-Conclusion Gates Join the Tier A List

#2811 classifies three daemon files that decide whether a run's
conclusion is final, or act on that answer, and that the AGENTS.md Tier A
Paths list missed. The fresh-context review for #2761 found the first two
(`devlog/2026-11-07-1213-publication-file-split.md`); planning found the
third by tracing the same check. All three are Tier A, checked against
the code at `778755d3c`. No gate's behavior changes.

## Decision

- **Listed `internal/signet/publication_reevaluation.go` under
  "Approvals, decision binding, and trust records".** Its gates:
  - `applyRerunTrustEvaluation` binds an accepted `rerun_trust_evaluation`
    command to the repository's latest trust profile, refuses a profile
    already evaluated for that head, and queues the reevaluation intent.
    That is decision binding.
  - `AuthenticatedRunConclusion`, with `publicationBlockedMilestone`,
    `publicationReadyOrder`, and `publicationBlockResolutionAuthenticated`,
    decides whether a blocked run's block is resolved and its conclusion
    final. A wrong `Final` admits a retry attempt (next two entries): a
    second implementation run over the approved specification, under the
    operator's retry binding, while the parent can still publish. That is
    why it is authority and not the quota park's "only delays or retries
    a task".
  - `AuthenticatePublicationReevaluationCompletion` authenticates the
    completion that `engine/publication_continuation.go` rebuilds a
    publication task from.

  Chose this category over "Publication decisions in the engine": the
  file sits in `signet`, and its first gate binds an operator's decision.
- **Listed `internal/engine/production_attempt.go` under "Credential
  injection, admission, and runner capabilities".** While the parent's
  workflow is open, `reattemptProductionRunTx` refuses a new retry unless
  `AuthenticatedProductionRunConclusion` reports the parent run `Final`.
  It then writes the attempt record that carries the
  operator's command, capability manifest, or alternate-agent record. That
  record decides which agent and capabilities the retry runs under, and
  the listed `internal/store/production_attempt.go` authenticates it on
  read, so its writer sits beside it.
- **Listed `internal/engine/production_workflow.go` in the same category.**
  `authenticateProductionAttempt` repeats the `Final` check for every retry
  attempt inside the submission transaction. The file also authenticates
  the specification grant before an implementation run is submitted
  (`authorizeProductionSubmission`) and a run's terminal against its
  admission and driver outcome (`authenticateProductionTerminal`).
  - **Two checks, neither alone the gate.** A fresh allocation writes the
    attempt and submits its run in one transaction, so both checks see the
    same state there. A resumed allocation (an operator retry replayed, or
    an attempt whose run was never created) returns before the attempt-level
    check, so the submission re-check is the only `Final` check on that
    path. It matters because an accepted trust reevaluation turns a
    blocked, final parent back into a live one. Rejected: leaving this file
    off because `production_attempt.go` already checks.
  - **Accepted the cost.** The file is about 2,600 lines, and every edit to
    it now takes the full bar. Rejected: moving the gates into a smaller
    file first. That changes code, which this unit's contract excludes.

## Left Off the List

- **Callers that grant authority themselves go to #2836.**
  `engine/capability_retry.go` turns an accepted capability choice into a
  campaign attempt, the twin of the listed `engine/alternate_agent_retry.go`.
  `engine/publication_continuation.go` reconstructs a decoded continuation
  task and records the publication successor. Rejected: listing them here.
  The contract scoped this unit to the gates, not their callers.
  Follow-up: #2836.
- **Helpers the gates call also go to #2836.** The fresh-context review
  found them. `signet/sync.go` holds
  `AuthenticateReevaluatedBlockedItemIdentity`, which decides whether a
  rerun on a non-canonical blocked item binds as a reevaluation, and
  `authenticatedCompletionMilestone`, which feeds the completed branch of
  `AuthenticatedRunConclusion`. `domain.ConcludeRun` in
  `domain/observation.go` is the base classifier that marks a blocked run
  `Final`. Both files mix these with display projection, so classifying
  them needs its own read.
- **Readers that only display the conclusion stay off.**
  `cmd/freesided/task_record.go`, `observe/observedb`, and the snapshot
  projections in `signet/sync.go` show it to the operator and grant
  nothing.
  `engine/quota_park.go` stays Tier B per its worked example.

Revisit when a `Final` check or the grant check moves to another file, or
when a refactor splits `production_workflow.go`: the tier follows the file
that holds the gate.
