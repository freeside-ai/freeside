# Bind Task Progress To Recorded Campaign Approval

Issue: #1371

Specification and implementation are separate runs. Chose the existing task
history’s `specification_approved` event over stage order, supersession, or a
matching digest because only the event records the approval relationship.
Select the displayed run’s campaign within its task and project. Require the
specification member, initial implementation member, initial attempt number,
and approved digest to agree. An implementation snapshot must agree with the
campaign and digest when present. A retry inherits that campaign’s approval;
a revised campaign cannot borrow another campaign’s event.

The specification run’s digest describes source input, not the approved bytes.
Keep its source label even after approval. For implementation, show the
approved label only with bound history. Unknown history gets an explicit
qualification, including in the task rail’s accessibility summary. This is a
historical fact, not authority to execute or proof of PR readiness.

Reuse the task-timeline cache and transport. A task/revision/epoch/full-snapshot
key, including the existing cache generation, owns a shared request across
list and detail. Retain successful reads for that key; failed or cancelled
reads stay retryable. Partial reads still advance only the observed cursor.
Same-epoch cached history survives offline use under the freshness warning;
removal and epoch reset discard it.

## Refute-First Findings

- Wrong task/project/campaign, missing members, mismatched roles, and wrong
  implementation digests must not complete Specification. Focused tests
  disprove these paths while preserving every later rail entry.
- A cancelled list request could suppress a detail replacement. The shared
  waiter retries after the cancelled operation; the overlap regression
  verifies the replacement reaches a loaded state.
- Independent review found no reachable defect. Conflicting duplicate run
  members could pass the consumer’s membership lookup, but the daemon builds
  these sections from authenticated, unique run identities. Declined another
  duplicate guard as hypothetical malformed-response hardening.

Revisit when approval-event bindings or task-history completeness change, or
when a new producer no longer guarantees unique run identities.
