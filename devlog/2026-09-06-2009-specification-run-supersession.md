# Terminalizing a Specification Run at Implementation Binding

Chose to finish a bound specification run by projecting it as superseded by
its implementation run, reusing the existing `superseded_by` derivation,
rather than giving the specification lane a real terminal outcome (#1183).
A specification run's timeline holds only `run_submitted`, so it concludes
`pending` and stays on the clients' active list forever; the sync projection
now marks it superseded once its production attempt is approved.

## Decisions

- **Reuse `superseded_by`, do not add an outcome or milestone.** In
  `daemon/internal/signet/sync.go`, `runProjectionFactsFor` reads the run's
  production attempt when the run has no retry successor, and sets
  `superseded_by` to the attempt's implementation run when the run is that
  attempt's specification run and the attempt carries an approved
  specification digest. `domain.LifecycleOf` already reads `finished` from a
  set `superseded_by`; the outcome stays `pending` and the wire shape is
  unchanged. Rejected: a new `RunOutcome` (for example `completed` for the
  specification lane) or a new milestone kind. Both change `domain`,
  `api/openapi.yaml`, and the clients, which is a `kind:contract` unit, not
  this fix. The owner may veto in favour of a distinct specification outcome;
  that contract unit would then be planned first and this fix replanned after.
- **The meaning of `superseded_by` widens from "retried" to "the run that now
  owns this run's work".** A retry successor keeps precedence
  (`RunSuccessor` is checked first); the specification hand-off is the second
  producer, used only when there is no retry successor. This matches the
  supervisor, which already derives `implementation_bound` from the same
  attempt fields (`observe/follow.go` `deriveSupervisionState`). The
  `api/openapi.yaml` description still says "retried"; widening that wording is
  a follow-up, not part of this unit.
- **The binding fact is read, not recomputed.** The production attempt already
  names the specification run, the implementation run, and (once the
  implementation is submitted) the approved specification digest. Approval and
  the implementation run's `PutRun` commit in one transaction
  (`engine/production_workflow.go`), so an approved attempt always has a
  submitted implementation run. No engine change is needed; the scope narrowed
  to `daemon/internal/signet` alone.
- **Campaign-less legacy specification runs keep today's behaviour.** They have
  no attempt row, so the rule never fires and they stay active. This is an
  explicit non-goal, not a gap.

## Refute-First Findings

The projection reads the production attempt through
`store.ReadTx.GetProductionAttempt`, a returned-object trust boundary, so the
`docs/agent-workflow.md` refute pass ran over the new read:

- **A forged attempt row naming a foreign run as `specification_run_id` with an
  approved digest.** Not reachable. `GetProductionAttempt` re-authenticates the
  reconstructed row: `authenticateInitialApprovedSpec` re-derives approval from
  the dispatched specification request, the output specification artifact (exact
  id, digest, and agent provenance), the resolved spec-approval item, and its
  digest-bound approve command, all keyed to the attempt's own specification and
  implementation run ids. A row cannot cheaply claim approval for a run it did
  not legitimately produce. `boundImplementationRun` adds a second guard,
  requiring `attempt.SpecificationRunID == run.ID`, and the run read itself
  cross-checks the run's `CampaignID`/`AttemptNumber` against the same row.
- **An approved digest set without an implementation run.** Not reachable.
  `store.ApproveProductionAttempt` runs inside the implementation run's
  submission transaction (`engine/production_workflow.go`, the approve call and
  the implementation run's `PutRun` in one `Write`). No preterminal guard is
  warranted for a state the engine cannot produce.

Revisit when #1083 lands a revision model that reactivates a bound
specification run after a blocked implementation: the rule would then also need
a liveness check on the specification run's own invocations, not the attempt
row alone. Revisit too if the owner decides a specification run deserves a
distinct terminal outcome rather than borrowing `superseded_by`.
