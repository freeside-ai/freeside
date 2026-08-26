# Command Result Trust Boundary

**Work unit:** #929.

## Decision

Chose fail-closed, exact correlation of every returned command result to the
submitted client command before trusting either its durable record or revision.
Generated decoding proves field shape, but it does not prove that a syntactically
valid result belongs to the command in flight. The client therefore requires a
positive revision and exact command, device, item, item-version, PR-head, artifact,
action, normalized-message, and attachment agreement before the result may clear a
retry slot, advance a cursor, or produce a conclusion receipt.

The mock server and client gate share the daemon's command-message normalization.
This keeps adversarial response transforms realistic without duplicating a second
normalization whose drift could make a malformed response look trustworthy in
tests.

Rejected trusting the decoded result, selected identifiers, or returned revision
alone: each permits a response for another command or state frontier to settle the
wrong durable retry slot. Also rejected treating a post-snooze 404 as sufficient
proof after a daemon restore; verbatim replay must first prove the durable snooze
record still exists before absence can conclude the item left the active queue.

## Verification Finding That Changed the Work

Automated review found that a mismatched returned result could clear durable retry
state. The widened refute pass confirmed the same trust class on direct submission
and on snooze recovery across restore. A later review found that a successfully
decoded snooze result followed by a failed canonical refetch could still discard
the replay command before absence was reconfirmed. Snooze commands now remain
durable until canonical visibility and verbatim replay jointly settle them. Tests
transform returned results without altering server records and cover mismatched
correlation, positive-revision gating, direct submission, replay, failed refetch,
restore, and expired-snooze boundaries.

The recurrence refute pass found that ordering and attempt ownership are part of
the same invariant: a replay can be the operation that first commits a command,
so the canonical read that settles a retained snooze must follow the replay.
Cancellation, superseding validation, and sync-epoch churn return an unsettled
owned attempt to the durable Retry state instead of leaving it in flight.

Revisit when the shared API authenticates and binds a command receipt to its request
and frontier strongly enough that the client can consume one generated invariant
instead of reconstructing the correlation gate.
