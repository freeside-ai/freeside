# One Recipe Runs Across the Image Room and the macOS Executor

Work unit: #2882. Mandatory note: Tier A (the engine decides which room runs
a verification command and whether a job may reach the host executor, and
the daemon decides which durable proof the executor declares from). The
issue's plan set the design; this note records what the implementation
settled and the residual risks it accepts.

## Decision

- **Admission only when the recipe needs it.** Every verification job reads
  its recipe through the project-image room, then parses it. Only a recipe
  with a `macos_host` command asks the host gate, so a project_image-only
  recipe behaves exactly as before and never touches the executor's
  configuration or conformance log. Chose that over admitting up front for
  every job because a failed or absent executor must not block projects that
  never use it.
- **No durable admission record.** An admission is logged and dropped. The
  gate re-checks the declaration and, under unattended, the passed record
  for the current digest on every job, so nothing downstream reads a stored
  admission and a stored one could only go stale. The verification report's
  per-step `room` records where each command ran.
- **One reservation per job.** The job's existing verification reservation,
  taken before the recipe read, covers whichever room runs each command,
  because commands run one at a time. The executor reserves nothing of its
  own.
- **Release needs both rooms' proofs, the slot's per pass (owner decision,
  2026-11-10).** #2954 made ward's cross-pass proof the only release proof
  for bound rooms, and it covers containers only: the executor writes no
  ownership records. So a pass whose slot returns
  `macverify.ErrAbsenceUnproven` keeps the reservation even when ward proves
  every container gone; ward's proof still runs first, so its cleanup retry
  is not skipped. A later pass whose own proofs pass releases. Rejected for
  now: holding across passes until the quarantined slot is re-proved empty,
  which needs a durable per-invocation slot record and a new proof (#3000).
  The owner chose the per-pass hold because host processes are uncapped
  either way and the pool never leases a quarantined slot again.
- **A refusal maps to each path's existing outcome.** A first cycle or
  remediation round records a failed verification whose evidence names the
  class and the refusal and blocks with the ordinary rerun actions, the shape
  a refused rebuild already uses. A readiness re-entry stops its cycle, and a
  handoff merge check stops the handoff, each with the refusal in its
  reason. Chose these over a new hold or attention type because each path
  already has a durable, operator-visible stop whose actions fit: once the
  executor is fixed, the rerun actions run the work again. A canceled
  context during the gate's store read stays retryable, so shutdown never
  becomes a durable failed verification.
- **The gate's accepted ids widen to re-entry and handoff jobs.** The gate
  previously admitted only the first cycle's producer-derived id. It now also
  admits a re-entry's id derived from the task's own sealed successor (same
  run, matching publication) and a handoff merge check's id derived from the
  task's own publication. It still refuses a forged id, another run's
  successor, another publication's handoff, and every ward-role id.
- **The daemon restores, and proves only on request.** `freesided` builds
  the backend from `-macverify-*` flags. Under `-run-conformance` it runs
  `Suite.Full` with the store's recorder, and a failed pass fails startup, as
  the ward's does. Otherwise it restores the newest record only when it
  passed for the current digest; a missing, failed, or stale record logs a
  warning and starts the daemon with the executor undeclared. Chose a
  non-fatal restore because the executor serves only recipes that name it,
  and under unattended the gate refuses those jobs by name. Under
  attended_dev the class has no capability floor (#2876's gate), so an
  undeclared executor still runs jobs, confined only by the pre-job probe;
  this unit leaves that policy as it found it.

## Residual Risks

- **A transient refusal becomes a durable failed verification.** An
  executor that is briefly undeclared (a pre-job probe quarantined a slot
  and withdrew the declaration until restart) fails a first cycle's
  verification rather than retrying. The operator reruns once the executor
  is proved again. Revisit if exit runs show this happening on healthy
  hosts.
- **Host memory is uncapped.** The reservation is a budget figure for the
  executor's processes, not a limit; a recipe command can exceed it.
- **An unproven slot holds memory only until the next pass.** A job whose
  slot was quarantined keeps its reservation until a later pass's proofs
  pass. The executor never re-inspects a quarantined slot, so that pass
  releases while the first slot's processes may still run. Those processes
  are uncapped either way; revisit if quarantines strand real memory (#3000).
- **Executor errors an operator must fix retry quietly.** A toolchain
  mismatch, a non-macOS host admitted under attended_dev, or a missing Swift
  cache entry (which a candidate's `Package.resolved` can cause) retries
  every pass under the generic environment hold, like any other room
  failure, without naming the cause. So does an executor whose every slot
  is quarantined (`macverify.ErrNoFreeSlot`), until a restart (#3004).
- **The deputy configures no executor.** Its settings file requires every
  key, so adding executor settings is a settings-format change left to a
  follow-up unit.

Follow-up: #2978, #3000, #3004.
