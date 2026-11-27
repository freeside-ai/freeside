# Refuse Retry on a Failure Card Whose Task Has a Stop

Work unit: #3566. Mandatory note: touches Tier A paths
(`daemon/internal/engine/failure_retry.go`,
`daemon/internal/engine/alternate_agent_retry.go`,
`daemon/internal/engine/production_workflow.go`, and
`daemon/internal/signet/http.go`). The new gate in
`daemon/internal/signet/service.go` and the skip in
`daemon/internal/engine/capability_retry.go` only withhold or refuse and grant
nothing, so neither joins the Tier A list.

## Decision: Check the Stop at Offer and Acceptance, Not in Failure Facts

A task has a Stop whenever `task.Cancellation != nil`, in any state (`requested`,
`confirmed`, or `failed_to_stop`). Nothing clears a cancellation, and it fences every
new execution start on that task.

Chose checking the task's cancellation when raising failure cards and again at
signet acceptance over folding the Stop into `ExecutionFailureFacts`,
`DecideRetryOutlook`, or `OffersSameAgentRetry`, because:

- A Stop is task state, not a fact about the execution failure.
- `domain/card_facts.go` re-derives `RetryOutlook` on read from the stored failure
  facts, so altering the outlook or facts based on task state at raise time would
  either break read-time validation or entangle domain failure facts with mutable
  task state.
- Keeping failure facts unchanged preserves history for cards that stay open
  (unconfirmed Stop, `failed_to_stop`, or genuine failures raised before the Stop)
  while withholding retry actions and offers when the card is raised on an already
  stopped task.

Open cards are not rewritten when a Stop arrives later. Chose leaving existing open
cards untouched and refusing retry decisions in signet over mutating open cards on
`StopTask`, because card content is bound to the failure event and versioned for
clients; rewriting cards on Stop would add cross-entity mutation to `StopTask`
without removing the need for the signet gate (races and stale clients still submit
against the earlier card).

## Decision: Cover All Three Retry Actions and Feedback Retries

Chose covering `retry`, `retry_with_capabilities`, and `retry_with_alternate_agent`
(plus operator feedback card `retry` offers) in one change over limiting it to plain
`retry`, because all three had the same gap:

- Signet accepted all three on a stopped task.
- Worse, `reconcileCapabilityRetries` and `reconcileAlternateAgentRetries` skipped
  only `store.ErrTaskWorkflowClosed`, not `store.ErrTaskCancellationFenced`. An
  accepted capability or alternate-agent retry on a task stopped afterwards caused
  `Engine.Reconcile` to fail before reaching `reconcileInvocations` on every pass.
- `offerOperatorFeedbackRetry` added `retry` to open operator feedback failure cards
  without checking the task's cancellation, and `enqueueOperatorFeedbackRetry` then
  dropped the accepted command quietly.

Both capability and alternate-agent reconcilers now skip
`store.ErrTaskCancellationFenced` alongside `store.ErrTaskWorkflowClosed`, covering
a Stop that arrives after acceptance and any command accepted by an older daemon.

## Decision: Answer 400 With a Message and No Typed Reason

Chose 400 (`signet.ErrTaskStopped` in `isCommandAuthorityRejection`) with a plain
error message and no new `CommandRequestRejectionReason` over 409, 500, or a typed
rejection reason, following
`devlog/2026-11-19-1400-unavailable-hook-answers-400.md`:

- **Why not 500:** the gate runs inside `Submit`'s `store.Write` transaction before
  `PutCommand` and before any post-commit wake. The transaction rolls back with zero
  durable effect. A 500 tells clients the command may have committed and causes
  indefinite replay; 400 communicates a definite refusal with nothing recorded.
- **Why not 409:** 409 carries a replacement item for stale versions or closed
  items. Here the card is still open at the version the client observed; the closed
  and stale-version checks still run before the Stop gate and keep answering 409.
- **Why no typed reason:** no client handles a stopped-task retry refusal
  differently from any other 400 card refusal, so adding a
  `CommandRequestRejectionReason` member would expand the API contract without a
  consumer.

Revisit when a client needs to distinguish a stopped-task retry refusal from other
400 decision refusals to drive specialized UI behavior.

## Refute-First Findings

The pass was one fresh-context reviewer reading the diff at a fixed revision,
and a mutation run that disabled each Stop check in turn.

Confirmed and fixed:

- The README, this note, and a signet test named the capability retry
  `retry_with_capability`; its wire value is `retry_with_capabilities`.
- This note's first draft named `signet/service.go` and
  `engine/specification.go` as Tier A paths; `scripts/tier-a-lookup.sh` lists
  neither.
- A comment in `engine/failure_retry.go` said signet "now" refuses, which goes
  stale on merge.

Disproved by a check:

- **A Stop check that no test reaches.** Disabling the signet gate, or the
  Stop read at the specification, implementation, or operator feedback raise
  site, fails the matching test, and the replay test still passes. With the
  implementation check disabled, the stopped card offers
  `retry_with_capabilities` and `retry` or `retry_with_alternate_agent`, so
  its test is not vacuous.
- **A refusal that records something.** The gate returns inside `Submit`'s
  write before `stampDecisionEvidence` and `PutCommand`, and the wakes run only
  on a nil error. The signet and HTTP tests assert the revision, the command
  table, and the card's version are unchanged.
- **A closed or stale card answering 400 instead of 409.** The gate runs after
  both checks; a test covers stale-version precedence.
- **A retry accepted in a race with the Stop.** Acceptance and `StopTask` are
  both store write transactions, so one commits first: a retry accepted first
  is skipped by its reconciler, and one submitted after is refused.
- **A missing task failing terminal recording.** The new task read in the
  terminal transaction fails only for a task that doesn't exist; submission
  assigns every task, and nothing deletes one.
- **A card whose actions and offers disagree.** `AttentionItem.Validate`
  requires `retry_with_capabilities` and `OfferedManifests` to agree; a
  stopped card has neither.

Allowed by explicit decision:

- A card already open when the Stop arrives keeps showing its retry actions,
  and signet refuses them (the issue's non-goal).
- `runTaskHasStop` nearly repeats `requireTaskNotCancelled`; merging them
  would refactor code outside this unit.

Unconfirmed: the gate's "run with no task" branch may be unreachable if the
store gives every persisted run a task. It treats such a run as having no
Stop, which grants nothing either way.
