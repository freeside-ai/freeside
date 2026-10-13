# Authenticate In-Place Re-entry Conclusions With Ready Bindings

An in-place publication cycle never creates a producing invocation or its
terminal record. Reading a terminal by that empty invocation ID made run
conclusions unreadable after the cycle finished. Queued cycles still read as
pending. The regression tests for #1986 reproduce the failure after external
review ends on a person and after readiness re-entry succeeds.

Use the cycle's authenticated ready-item PR binding as its completion proof.
A finished cycle with no ready item remains incomplete, allowing the
existing milestone classifier to report its conclusion. A bound cycle returns
the inherited producing invocation and its own publication invocation. Always
returning incomplete would lose authenticated readiness, including the proof
required to classify a merged work unit as completed.

Reuse the store's existing proof instead of copying its predecessor checks
into the engine. `GetReadyItemPRBinding` revalidates the sealed re-entry
authority, head, run, predecessor publication identity, producing invocation,
repository, PR, and base ref. The engine authenticates the task's full successor
against the sealed record, then compares the returned binding with the selected
task's run, publication invocation, and head. The existing
superseded-item check still applies before this path, and the existing
milestone authentication still follows it. No classification or record shape
changes.

## Refute-First Evidence

- Confirmed: both finished in-place paths reached `GetInbox` with an empty
  key before the fix. The task-record test also reproduced the null
  conclusion and section error after dispatch, while its queued read passed.
- Confirmed by independent review: treating every missing binding as ordinary
  incompletion could hide a lost proof behind an existing ready milestone.
  A dispatched cycle with a ready item now propagates all binding errors;
  only a missing ready item takes the incomplete path. The integration test
  removes the proof of a published re-entry and requires the read to fail.
- Disproved by existing store checks: a binding can substitute another
  repository, PR, base ref, producer, publication identity, or head.
  `TestReenteredReadyBinding` and `TestReenteredReadyBindingNeedsItsAuthority`
  exercise those forgeries, including a changed persisted head and missing
  authority.
- The integration regression covers the completed-work-unit path after a
  re-entry re-earns readiness, so a missing terminal cannot silently remove
  the proof required for a final completed conclusion.
- Confirmed by automated review and a failing regression: a task's embedded
  successor could claim a different review round while retaining its IDs and
  head. The completion path now uses `authenticateTaskSuccessor`, the same
  sealed-authority gate used by task execution, and rejects that divergence.

Revisit when an in-place cycle gains its own producing terminal or the
store's ready-binding authentication contract changes.
