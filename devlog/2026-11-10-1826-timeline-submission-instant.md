# A Submission and the Task Start It Records Share One Instant

A new task's Timeline read backwards (#3039): Task started sorted above the
specification run's submission, and the router's three lines sorted by line
ID. Each submission now takes one clock reading for its `run_submitted`
milestone and the task start it records, and the history breaks a tie
between line events by workflow role.

## Decision

- **Chose one instant per submission over an explicit causal rank in the
  sort.** The submission and the start are written in one transaction, so
  they happened together; giving them one instant states that. The existing
  same-instant tie-break (kind ascending, so `run_milestone` before
  `task_started`) then puts the start below the submission in the
  newest-first list. A causal rank would have to name every kind pair, and
  the issue rules it out of scope.
- **Fixed every submission site, not only `submitProductionRunTx`.** The
  issue named `production_workflow.go`, but the operator's case, a
  specification run, records its submission and start in
  `engine/specification.go`, in both the spec-artifact arm and the
  issue-subject arm (including the start after a released admission). Fixing
  only the production path would have left the reported timeline unchanged.
  Implementation found this; the PR records it as a departure from the
  declared paths.
- **Chose `domain.AllRoleNames` order for tied line events over line ID.**
  The router writes all its lines with one `setAt`, so time and kind tie and
  the old last key was the line ID, a digest with no meaning to a reader.
  `AllRoleNames` already lists roles in workflow order (specifier,
  implementer, remediator, ...). Version breaks a remaining tie; the source
  key stays last.

## Tier A by Path Only

`engine/production_workflow.go` is on the Tier A list, so this unit is Tier
A. The change passes the same instant to the existing writes and touches no
gate: which facts are written, the WIP admission and cap check, the
work-unit declaration, and every refusal are unchanged. The refute-first
pass checked that nothing compares a task start's instant with its
submission's (the only readers take a maximum or display it) and that the
history order feeds only the Timeline read.

## Revisit When

- A `TaskEventKind` is renamed or the kind tie-break changes: the start
  would sort above its submission again. `TestTaskHistoryOrdersTaskStartBelowItsSubmission`
  pins the order.
- `AllRoleNames` is reordered for another reason: tied router lines follow
  it.
- Tasks submitted before this change keep their microsecond gaps and their
  old order; nothing rewrites them.
