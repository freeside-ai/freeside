# Re-Admit A Never-Started Attempt After A Daemon Upgrade

An unattended admission binds the backend configuration digest, and ward's
digest includes the daemon executable's hash. After any daemon upgrade, an
attempt recorded but not yet started replays its stored admission, the
store's gate passes it (it judges A by A's own rows), and the start
authority refuses it because the process yields B. The refusal holds the
run under `backend_not_conformant` and ends the pass, every pass, for ever.
Issue #3433.

## The Decision

Chose to supersede the stored admission in place, for an unattended attempt
the stage driver never started, over two alternatives:

- **End the attempt and file a retry.** Rejected: production and
  remediation invocation ids are fixed per run and round, so a retry cannot
  take a new invocation id without reworking every path that derives one.
  A never-started attempt has nothing to end.
- **Drop the daemon hash from the digest.** Rejected by the contract's
  non-goals: the digest is what proves the running binary is the one that
  passed conformance.

This is the one exception to write-once admissions. It is narrow on
purpose: both admissions unattended; the same invocation, run, stage,
attempt, and base; a different configuration digest; the stored body
exactly the one the engine read; and no export, outcome, export rejection,
or import start for the invocation. The fresh admission passes every gate a
new one does (`requireAdmissionGates` and `requireNewAdmissionGates`, shared
with `RecordExecutionAdmissionReserving`). The replaced record moves to
`superseded_execution_admissions`, so the history keeps every admission an
invocation was granted.

"Never started" is proved by the stage driver, not inferred: the driver
commits its intent file before any runtime object exists, and `Inspect`
returns `exec.ErrUnknownInvocation` only when that file does not exist.
Only the engine pass calls Start, synchronously, so no Start can race the
check. Any other `Inspect` error fails closed.

An attempt the driver did start may have run its writer, so its admission
still binds it and it is never re-admitted. It keeps the hold and now
raises one `execution_failure` notice naming the invocation, because the
hold alone gave the operator no way to learn why it waits.

## Departures From The Plan

- **No engine option or `claude_driver.go` change.** The engine reads the
  running digest from the same ward backend the start authority does
  (`ConformanceFor`), so there is no second source to wire.
- **No second `invocation_admitted` milestone.** `run_milestones_identity`
  is unique on run, kind, and invocation, so a second append is a silent
  no-op. The history table records the re-admission instead.
- **`export_rejections` joins the started check.** A rejection exists only
  after the driver exported, so it proves the attempt ran.

## Findings

Refute-first pass, recorded so the claims don't return:

- **A driver that forgot a started invocation re-runs its writer.**
  Disproved as a new risk: the replay path already calls Start for any
  invocation `Inspect` reports unknown, under the same admission. The
  supersession changes which configuration that Start binds, not whether
  it happens, and an invocation that left an export, outcome, rejection, or
  import start is refused by the store.
- **The fresh admission starts from another base.** Disproved: the engine
  admits at the run's base, or its first recorded admission's, which is the
  stored admission's own; the store refuses a different base anyway.
- **A row citing the old admission id is orphaned.** Disproved: the tables
  that cite `execution_admissions (id)` are exactly the four the started
  check reads (exports, outcomes, export rejections, import starts).
- **The pool slot is counted twice.** Disproved: capacity and memory
  checks exclude the invocation's own row, and the dispatch reservation is
  idempotent on an entry already dispatching.
- **The engine tests passed without the fix.** Confirmed and fixed: the
  engine fixture had no start authority, so a stale admission started
  anyway. The tests now start through a driver that refuses a mismatched
  configuration the way `requireRunnableConfiguration` does, and both fail
  with the re-admission disabled.
- **A refused re-admission strands the dispatch reservation.** Confirmed
  by the fresh-context review and fixed: the refusal returned before the
  replay reached the start authority, whose refusal released it, so a
  credentialed attempt left `dispatching` by a stop kept a pool slot while
  held. A re-admission refusal now goes through `refuseStart`.
- **An operator stop that commits mid-pass fails the pass.** Confirmed
  (race only) and fixed: the supersede write's operating-state refusal now
  holds the way `recordAttempt`'s does.
- **A store refusal or an `Inspect` error stops the lane.** Allowed by
  decision: `ErrAdmissionNotSupersedable` past the engine's own checks
  means the stored row changed under the pass, and an `Inspect` failure
  means the driver can't prove the attempt never ran. Both fail closed.
- **Backups skip blobs only a superseded body cites.** Allowed by
  decision: the superseded record is history nothing starts from or reads
  blobs through, so the backup closure keeps reading current admissions.
- **The notice never expires.** Disproved: workflow closure expires open
  items on the run's task episode, and a run-subject item maps to it.

Revisit when the stage driver can start an invocation from anywhere other
than the engine's dispatch pass: the "never started" proof assumes it
cannot.
