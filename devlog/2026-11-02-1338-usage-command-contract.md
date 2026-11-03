# Usage and Routing Command Contract (#1719)

The plan for #1719 said no note was needed unless the implementation departed
from it. AGENTS.md makes one mandatory for a contract change and for
returned-object trust-boundary work, and this unit is both. The issue and its
plan comment keep the contract's main decisions. This note records where the
implementation chose differently, and why.

## Decisions

- **Chose one receipt table keyed by `command_id` over a table per command.**
  Every operator command records a `domain.CommandReceipt` in
  `client_command_receipts` (migration 0118): the kind, the accepting
  revision, and the arm's JSON. One key across the four kinds, plus checks in
  the decision, submission, and Stop writers, keeps `command_id` unique
  across every kind with one lookup. A table per kind would need four more
  cross-checks in every writer.
- **The receipt is re-validated on read, never trusted.** `GetCommandReceipt`
  decodes the arm the row's kind names and runs its `Validate`. It refuses a
  body whose `command_id` isn't the row's key, and an accepting revision
  ahead of the server revision. A replay returns the stored receipt only when
  the request matches it field by field: `Same` for line changes,
  `SameRequest` (canonical orders plus `expected_revision`) for the policy.
  The refute-first review confirmed each refusal and found no forged replay.
- **The id check runs before the effect.** `commitReceipt` calls
  `RequireCommandIDFree` after the replay read and before the hook, so a
  reused decision, submission, or Stop id answers 400 even when the hook is
  unwired, the task is missing, or the role runs. An effect a later hook lets
  out of the transaction (#1720) can't fire for an id that conflicts.
- **Chose injected hooks over signet calling the engine.** The engine imports
  signet, so signet reaches `engine.ChangeTaskLineTx` and
  `engine.SetRoutingPolicyTx` through `WithTaskLineChanger` and
  `WithRoutingPolicySetter`, which `freesided` binds. A nil hook fails the
  command closed (500) after replay, so a recorded receipt still replays.
  `WithUsageOperator` stays unbound until #1720.
- **An omitted `agent` or `expected_revision` reads as null.** Both are
  required and nullable in the spec, but the generated Swift client leaves a
  nil member out of the body. The daemon already reads an omitted
  `pr_head_sha` as null for that reason. Requiring presence would have
  refused every clear and every first policy from the real app.
- **`TaskLineChangeRecord.line` is null after a clear.** The wire `TaskLine`
  names an agent, so a cleared head projects as no line, as it does in the
  task snapshot's `task_lines`.
- **`RoutingPolicyChangeRecord` carries `expected_revision`.** A replay has to
  match the request, and the policy alone can't show which revision the
  client expected when the orders were already held.
- **A line's `set_by` stays the change's digest for both sources.** The
  device appears in the receipt. The policy's `set_by` is
  `device:<device_id>`, as the CLI's is `cli:<user>`.
- **A pool with no usage record projects version 0 at the serving
  revision.** The enrollment generation isn't projected: no client decision
  reads it.
- **Owner decision: the bootstrap's usage projection may read the pool
  usage record.** #2486's containment test admits only allowlisted
  readers, and the landing batch handed this PR back rather than widen
  that list. Plan §5.4 says the record informs the account usage view in
  the clients, and the projection decides nothing, so the owner chose to
  allowlist `internal/signet/usage.go` over renaming the field and routing
  the read through an allowlisted package, which would only dodge the
  identifier check. `ListPoolUsageObservationSnapshots`, which this unit
  adds, joins the reader set, so a caller anywhere else still fails it.
- **Reused `HarnessClientKind` over a second enum schema.** #2492 landed
  `HarnessClientKind` with the same values as this unit's `HarnessClient`,
  so the enrollment's `harness_client` now refers to it. Two schemas for
  one domain enum would give the generated client two distinct types.

## Deferred

- **Some usage-state writes still skip the revision.** Pool limits, identity
  pool membership, client enrollments, and the pool-record delete on
  settlement still commit on `InternalTx`. The projection reads all of them,
  so each can change the bootstrap at an unchanged revision. Moving about ten
  writers across ward enrollment and auth adoption is a credential-surface
  refactor outside this contract. Follow-up: #2487.
- **Exhausted marks depend on the read's clock.** The projection keeps one
  mark per window as of `now`, so a refusal hidden behind a reading's mark
  can appear at an unchanged revision when the reading's reset passes.
  Choosing a time-steady rule is a contract decision for the owner.
  Follow-up: #2488.

Revisit when #1720 binds the usage operator: check that its effect stays
inside the accepting transaction, or that it runs only after the commit.
