# Completed Runs in Real-Work Supervision

For #1143, accept `completed` as success only in the implementation lane.
A resumed supervisor, or one switching from a bound specification, can first
observe the implementation after its PR has merged. Requiring an earlier
`published` snapshot would reject a valid run merely because observation
missed that state.

Treat specification completion as an unexpected terminal outcome and report
it through the existing failure path. Rejected: sharing the success arm with
both lanes, or treating completion as an implicit implementation handoff.
Neither establishes which implementation to observe. `implementation_bound`
remains the only lane-switch signal, consistent with the specification
lifecycle decision in
[the source note](2026-09-04-1620-run-completion-lifecycle-spend.md).

## Refute-First Findings

- **Specification completion could establish false success.** Disproved by
  fixtures for supplied and unresolved implementation identities: both fail
  with the specification identity and completed outcome, retain the snapshot,
  and make no implementation observation. The unresolved case also proves
  that the resolver is never called. Scratch mutations that accepted
  specification completion as success or as a handoff both failed these
  fixtures. Independent review found no actionable issue in the lane split.
- **A terminal snapshot might be lost during resume or handoff.** Disproved
  by comparing the retained report byte-for-byte with the completed snapshot
  in both paths. Diagnostics name the selected implementation run.
- **Accepting completion could bypass publication authentication.** The
  daemon's `ObserveSnapshot` calls `authenticateRunConclusion` before exposing
  its conclusion. Completion authentication binds the milestone to the
  publication invocation and the re-gated work-unit completion record. This
  consumer change adds no authority and changes none of those checks.
- **Supervision success could replace final verification.** Disproved by
  tracing `scripts/run-real-work.sh`: a zero supervision status still enters
  `TestRealWorkItemCompletesProductionPipeline` and requires its positive
  publication-verification message. That normal verifier deliberately rejects
  completed checkpoints: completed history is not a current ready publication.
  The overall exercise can therefore still fail after completed-state
  supervision succeeds. Keep that caller change outside #1143's explicit
  publication-verification non-goal. Follow-up: #1852 must use authenticated
  completed-checkpoint verification and its distinct success marker without
  granting continuation authority. No live production run was performed for
  this dispatch fix.
- **The new state could make unknown states succeed.** Disproved by fixtures
  that retain unsupported-state failure in both lanes. Existing publication,
  polling, attention, resolver, observation-failure, and timeout fixtures
  remain in place.

## Revisit When

Revisit if specification completion becomes a defined handoff, the daemon
changes completion authentication, or the caller stops independently verifying
publication after supervision succeeds.
