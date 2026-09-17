# Separate Stop Acceptance From Runtime Quiescence

#1367 introduces durable task cancellation without changing existing
attention-item Stop behavior. A stored request cannot attest that an agent
stopped. Chose an immutable request/fence, an append-only daemon acknowledgement
log, and separate immutable command receipts over a mutable command result.
Reconnect and restart therefore return the original receipt even after failure
or confirmation. Distinct command IDs share the target's fence without retrying
provider cancellation.

## Binding And Ownership

Chose the observed global sync revision over the private task-row version.
Signet projects the global revision as `TaskSnapshot.entity_version`, so only
that value identifies what the client actually saw. Rejecting unrelated
revision movement is conservative and permitted by the issue. Replaying a
recorded ID ignores live version movement but still requires an active device.

The daemon captures every owned run, its campaign, and the newest start ordinal.
This includes queued work with no start and older owned children. Chose this
membership plus a task fence over newest-run cancellation: a display selection
is not an ownership boundary. The target digest includes the sync epoch and
uses versioned UTF-8 byte-length framing so Go and Swift agree without JSON
object-order or escaping assumptions.

The runtime must block admission, successor launches and publication after the
fence, retain late results without publishing them, and reconcile owned
executions after restart. A PR published before the fence remains history.
Only runtime evidence of complete quiescence may confirm; an unresponsive
provider or partly stopped child remains pending or failed. Confirmation is
final, but failure may later become confirmed. Neither acceptance nor failure
releases WIP. This preserves the earlier lifecycle note's decision that
abandonment is not proof of an executed Stop.

A fresh Stop after an epoch change creates a fresh fence even if membership
is unchanged. A restored database cannot prove that executions created after
its checkpoint are quiescent. New acknowledgements for old-epoch fences fail;
exact historical command and acknowledgement replay remains idempotent. This
avoids reusing a pre-restore confirmation as current runtime evidence.

## Consumer Responsibilities

#1368 supplies runtime enforcement and acknowledgements. #1344 records stopped
lifecycle and releases WIP after confirmation; #1369 adds controls. This unit
adds no live acknowledgement producer and no automatic client retry. It changes
the material plan because these durable boundaries govern downstream behavior.
The full assigned contract stays atomic across domain, store, API, generated
client, mock and result trust; splitting only the public portions would expose
an inconsistent union or sync contract.

## Refutation Findings

Regression tests target revoked replay, cross-kind command-ID reuse, stale
epoch/version, multiple devices, changed request bodies, missing/foreign tasks,
forged acknowledgement bindings, confirmation regression, and late evidence
for a changed target. Acceptance does not infer quiescence from task lifecycle.
A reconstruction test exposed a task/run recursion: run reconstruction already
re-gates its task. Cancellation reconstruction now checks raw membership columns
and decoded run identity without recursively loading the task; full run policy
still gates new target capture and execution reads.

Independent review also found that a prior-epoch confirmation could be reused
after restore, and a client accepted an inflated receipt revision. Epoch-bound
fence digests now separate those targets, and receipt correlation requires the
exact pre-write revision plus one. These are contract checks, not claims that
runtime enforcement exists.

## Revisit When

Task projections gain per-task versions, task ownership can detach runs, or a
restart/resume feature intentionally replaces a confirmed cancellation target.
Those changes need explicit fence and acknowledgement migration rules.
