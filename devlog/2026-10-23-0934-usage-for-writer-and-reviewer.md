# Usage for the Writer and the Reviewer

Issue #2212. A published unattended run recorded usage only for its
specification invocation, so its `billable_cost` was null. The writer's usage
was dropped because an unattended export is never collected, and the
reviewer's because `usage_observations` accepted only execution admissions.

## Decisions

- **Chose dropping the `execution_admissions` foreign key (migration 0102)
  over a second review-usage table.** Review admissions live in
  `review_admissions`, and SQLite can't express "references one of two
  tables". One table keeps a single `UsageReadTx` surface and one
  `BillableCostSoFar` projection. Attribution still comes only from an
  immutable admission: `AppendUsageObservations` reads the execution
  admission, falls back to the review admission, and writes nothing when
  neither exists. That store gate, not SQLite, now refuses an unattributed
  row, so it must not be loosened.
- **Chose writing the writer's usage in the export's own transaction over a
  later collect.** The unattended export mints its publication task in that
  write, and the workflow never collects the stage afterwards. The usage
  travels on `stage.ExecutionReplay` and `engine.ProductionReplay`; every arm
  of `engine.RecordExecutionExport` appends it. Attended and specification
  exports are still collected, and that second append converges on the same
  rows.
- **Chose observing usage at the intent's pinned export time over the clock
  (the agent's call, departing from the plan).** A crash after the export
  write replays the import, and a fresh `observed_at` would make the
  write-once usage rows conflict, failing recovery on every retry. The pinned
  stamp is the same one the export row uses, so the replay is byte-identical.
- **Chose keeping usage out of the persisted publication task
  (`json:"-"`) over the plan's `omitempty` field.** Usage isn't replay data;
  it is durable in `usage_observations` once the write commits. Leaving the
  task bytes unchanged means a re-entry copy or a retried write can't drift
  on it.

## Known Limits

- A Codex-reviewed run's `billable_cost` stays incomplete: Codex reports
  tokens only, with no cost row.
- An unattended export the previous daemon already recorded never
  re-enters the export write, so a run in flight across the upgrade keeps no
  writer usage.
- Usage from any review admission counts, so a shadow reviewer's spend is now
  part of `billable_cost` too. That spend was real, so this is intended.

Revisit when the review source prices its own tokens, or when usage gains a
source that is neither an execution nor a review admission.
