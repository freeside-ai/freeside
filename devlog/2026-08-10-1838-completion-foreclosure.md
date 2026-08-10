# Merged Divergence Forecloses Completion Recovery (#650)

The completion-only sweep introduced by #624 had no terminal state when an
operator amended a pull request before merging it. The persisted pull fact then
proved both that the pull was merged and that its head or base no longer matched
the bound candidate, while the exact completion gate correctly refused to mint
a `WorkUnitCompletion`.

## Decisions

- **Derive foreclosure from the existing durable facts.** A concluded unit is
  terminal when it has no supported completion and any persisted pull fact is
  merged with a head or base that differs from the independently
  reconstructed ready and work-unit bindings. Chose this over a new refusal
  table because the store already proves the condition and a second fact would
  duplicate authority.
- **Keep the exact completion gate unchanged.** Foreclosure stops the recovery
  sweep and finalizes process-local cache eviction; it never creates or
  authorizes a completion. An operator acknowledgment resolves only the
  resulting attention item.
- **Surface one advisory system-health item.** Its ID is deterministic from the
  work-unit ID, and existence in any status suppresses recreation. Chose the
  existing advisory type over a new synced attention contract because this is a
  terminal operator notice, not a new decision or blocking state.
- **Terminality requires merge, not close.** A closed-unmerged pull remains
  retryable because GitHub permits reopening it. An exact merged pull whose
  bound issue has not closed also remains retryable because the completion
  evidence can still become durable.

## Refute-First Verification Findings

- **Rejected by verification: a head-diverged or base-retargeted merge can
  record completion.** Both variants stop before remote observation on later
  passes, surface one notice, and retain no `WorkUnitCompletion` row.
- **Rejected by verification: restart or acknowledgment resurrects polling or
  attention.** A fresh reconciler derives the same terminal state from the
  append-only history even after a contradictory later exact fact, and the
  deterministic any-status existence check preserves a resolved notice.
- **Rejected by verification: terminality absorbs the retryable neighboring
  cases.** Exact merged work awaiting bound-issue closure remains observed, and
  the existing close-then-reopen-and-merge regression continues to complete.

## Revisit When

An operator-attested completion provenance is deliberately introduced as a
separate contract change; do not weaken exact bound-head and bound-base
completion to add it.
