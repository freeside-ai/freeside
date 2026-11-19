# An Unavailable Operator Hook Answers 400

Work unit: #2968 (kind:contract). Mandatory note: it changes a contract
(`POST /commands` statuses), touches a Tier A path
(`daemon/internal/signet/http.go`), and records an owner choice that
otherwise lives only in review feedback (F275DA07) and in the owner's PR
review of #3431.

## Decision: 400, No Command Effect

The owner chose that a command whose operator hook the daemon cannot apply
answers 400, applies no command effect, and saves no command result. Like
any authenticated request, it may still update the device's
`last_seen_at`, which `authenticated` in `http.go` records, at most once
per activity window, before the handler runs. The
hook sentinels are
`signet.ErrOperatorSettingUnavailable` and
`signet.ErrUsageOperationUnavailable`. In production the first one is the
case of a daemon with no committed agent tree. It answers
`set_routing_policy`, `move_parked_task`, and a
`retry_with_alternate_agent` decision. Before this change both sentinels
fell through `writeCommandError` to an ambiguous 500.

The 400 applies only when a command reaches the hook. Checks that run
before it keep their own answers: a malformed command is still 400 with
its own message, a stale routing-policy `expected_revision` still 409, a
move naming a missing task still 404, and a stale decision still 409.

Chose 400 over the two alternatives:

- **Keep 500.** A 500 tells the client the command may have committed, so
  every app model keeps its ledger slot claimed and offers Retry. Here the
  outcome is not ambiguous: each sentinel returns inside the command's
  `store.Write` transaction before anything is written, so the transaction
  rolls back and the daemon knows nothing was recorded. A 500 makes the
  app hold a command that can never have landed.
- **503.** A 503 says "try again later", which fits a refusal that may
  clear when the daemon gains a tree. But it is a new status every client
  model would have to classify, and the app treats 5xx as ambiguous. That
  is the same wrong signal as 500, and changing it would be a behavior
  change in every model. 400 already means "definite refusal, nothing
  recorded" to every model, and the schema's 400 text now says this
  refusal can clear once the daemon's state changes.

The 400 carries `message` alone, with no new `CommandRequestRejectionReason`.
No client acts differently on this refusal than on any other 400, so a
typed reason would add contract surface nothing reads.

## Tier A Rationale

`http.go` is on the Tier A list because it decides what a client learns
about whether a command committed. The refute-first pass checked that
every return site of both sentinels leaves nothing durable:

- **The five sites in `operator_commands.go`** sit in the `apply` function
  that `commitReceipt` runs inside `store.Write`, after the device gate and
  replay reads and before `RecordCommandReceipt`. `workflow_policy.go`
  returns its sentinel the same way.
- **The usage operator.** Neither usage command calls the operator before
  its sentinel, so `after` stays nil. `runAfterCommit` runs `after` only on
  a nil error.
- **The alternate-agent retry** (`alternate_agent_retry.go`) returns its
  sentinel after `PutCommand` in the decision transaction, so the error
  rolls the decision record back with everything else. The wakes in
  `submitDecisionTransaction` run only after a commit, and only for filing
  and adoption approvals.
- **The `daemoncompose/start.go` hooks** return the sentinel from inside
  the hook, before any engine write.
- **Wrapping.** Every wrap uses `%w`, and matching is by `errors.Is` on
  the two sentinels only, so no other error leaves the 500 path.
  `ErrTaskSubmissionUnavailable` stays a 500, as the owner scoped it.

One pre-existing write runs outside the decision transaction:
`convergeProposalSnoozes` releases expired proposal snoozes before any
decision is judged. It is due-time housekeeping that runs for every
decision whatever the outcome, and it records nothing about this command.

Replays are unchanged: an already-committed `command_id` returns its
recorded 200 before any hook is consulted.

## Contract Shape

The change is additive. It declares 401, 403, 404, 413, and 500 on
`POST /commands` and moves only these refusals from 500 to 400. That leaves
`x-compatibility-level` and `x-compatibility-floor` as they were. The app's
generated client now gives the new statuses typed cases, and each command
model routes them through the arm the matching undocumented status took
before, so no model's classification changed.

`POST /commands` is the only route that declares 401. The approved spec
said not to add it, because the security scheme covers it and no other
route declares it. The owner's review of #3431 overrode that for this
route: a decision retry that meets 401 leaves the original attempt's
outcome unknown, and the app handles it apart from other 4xx answers, so
a client written from the schema needs to see it. Other routes keep
relying on the security scheme.

Revisit when a client needs to tell this refusal apart from other 400s,
for example to show "waiting for the agent tree" instead of a generic
refusal. That is the point at which a typed reason pays for itself.
