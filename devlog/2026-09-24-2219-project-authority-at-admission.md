# Register Project Authority at Execution Admission

For #1535, chose to register a run's `projects` row inside
`RecordExecutionAdmission`, from the admission's base repository, over
registering at submission or at daemon startup. Client and CLI submissions
never wrote the row, so the closure gate's `GetProject` returned
`ErrNotFound`, which `isClosureGateRejection` reads as "not closable": every
client or CLI task quietly published `Refs` instead of `Closes`.

The admission base is the daemon's configured repository, the same identity
the closure target and every later production step already bind, and the
store re-checks it against the trusted profile for unattended work. Every
task's first admission is its specification stage, so the row exists before
the specification can be approved and before `verify-source` reads it. One
store rule covers the CLI, the paired client, and label intake.

Rejected options:

- **Register at submission.** Neither `freesided submit` nor the client path
  knows a numeric repository ID. It would need a new operator field that could
  disagree with the repository the daemon actually runs against.
- **Register at startup from configuration.** The CLI names projects no
  configuration lists, and a startup write binds projects that may never run.

Consequences the owner accepted in the issue contract:

- **Rebinding fails loud.** `RegisterProject` is write-once, so admitting a
  project already bound to another repository fails with
  `ErrImmutableConflict` and starts no work. A label-intake initiator that
  names a different repository than the daemon's `-repo` now surfaces as that
  failure.
- **A missing project on the closure path is corruption.**
  `closableSource` returns `ErrProjectAuthorityMissing`, which does not wrap
  `ErrNotFound`, and the engine treats it as a durable-state contradiction.
  Every other `ErrNotFound` keeps its gate-rejection meaning.
- **Existing stores backfill once.** Migration 0081 registers, per project
  with admissions and no row, the one repository its admissions name. Two
  repositories for one project fail the migration by name rather than guess.
  An existing row is left alone; a disagreement surfaces at that project's
  next admission. Checkpoints that already recorded "no proposal" stay as
  recorded.

Changed during implementation: the backfill skips an admission row that does
not reconstruct instead of failing the store open, following `backfillTasks`.
Authority never comes from unauthenticated bytes either way, and every
ordinary read of that row still fails closed. An older migration fixture that
seeds a stub `'{}'` admission showed the strict version would refuse to open
such a store over a row the backfill has no use for. Review also moved the
backfill's project source from the copied `runs.project_id` column to the
reconstructed run, so a column that diverges from the run body cannot mint an
immutable binding; such a run contributes nothing, like an unreadable
admission.

The `verify-source` fixture keeps its direct registration because it builds
store rows without an engine; the client and CLI admission tests prove the
production write. With the admission write disabled, the client close test and
the pre-existing close test both fail, naming the task and project.

A refute-first review found no blocker. Disproved by checks: the registered
repository is operator configuration, never task text; a refused admission
rolls its registration back with the transaction; no production path
configures a label-intake initiator today, so the two writers cannot
disagree; the foreign key makes an admission without a run unreachable; and
`closableSource`'s only callers act on proposals that could not exist
without a project row. Confirmed and fixed: after the backfill, a
same-version constructor replay of an item stored before its project row
carried the new repository label and was refused as stale, so the project
label is now normalized on same-version replay like the task name. Allowed by
decision: one project admitted against two repositories still fails the
store open (the owner chose loud failure over guessing), and an attended
admission whose repository fails the stricter project name pattern (a leading
`_` or `..`) now fails; neither shape exists in a known deployment.

Revisit when the base repository becomes per run rather than per daemon, when
submission starts carrying a repository, or when a project may legitimately
move between repositories.
