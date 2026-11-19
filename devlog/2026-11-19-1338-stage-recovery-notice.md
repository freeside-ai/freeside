# Notice A Stalled Stage Recovery, Conclude Missing Content

The #2382 exit run sat silent for the whole 30-minute recovery grace on an
error that could not change (#3427). This note records how #3428 fixes both
halves.

## Decision

- **Time-based notice bound.** Chose to raise the advisory
  `stage_recovery_stalled` notice once recovery of a released export has
  failed retryably for 5 minutes, over a count of failed passes. Reconcile
  passes run when the engine inspects, at no fixed rate, so a count means
  nothing in wall time. Five minutes lets a transient git, disk, or network
  fault clear without a notice and still leaves 25 minutes before the grace
  abandons recovery. The bound is a driver config field, zero for the
  default, so tests can set it.
- **One notice per deferral.** A changed error raises no second notice. The
  operator needs to know the run is stuck, not each new symptom; the daemon
  log already records each change, and the abandonment card names the last
  error.
- **Notice in `daemoncompose`, not `engine`.** The issue first put it in the
  engine. `InvocationStallNotice` already files the same kind of advisory
  system notice from the stage driver's hook in `daemoncompose`, with its
  startup sweep and inspect ownership, so this one follows it and the engine
  is untouched.
- **Missing content concludes at once and keeps the export.** Chose to
  conclude an import or persist failure that proves the released export
  lacks needed content as failed on the first pass, over keeping it
  retryable (the grace spent waiting on it) or treating it as a definitive
  rejection (which removes the directory). The directory may hold the
  agent's work, and a person may want it to diagnose the cause, as on the
  exit run. The class is `importer.ErrMissingBlob`, a stored manifest entry
  with no digest, and a blob or manifest file that is not in the directory.
- **Only "not found" counts as missing.** The importer also wraps a failed
  blob open in `ErrMissingBlob`, and its `openat` returns a bare errno, not
  an `*fs.PathError`. So the class excludes any chain that carries an errno
  or path error other than "not found"; descriptor exhaustion or an I/O
  error stays retryable. Only the driver removes a released export
  directory, and it does so only after the outcome is durable, so a file it
  no longer finds was never released, or belongs to an import already
  decided. Recovery therefore concludes missing content only once the
  export-record lookup has answered: a crash while a committed import's
  directory was being removed leaves it partial, and that record, not the
  directory, decides the invocation.
- **The live pipeline concludes too.** The classification is shared, so a
  live import that finds content missing commits a failed outcome at once
  instead of leaving the window to recovery, which would only wait out the
  grace on the same error.
- **Resolve on every end.** The notice resolves when the deferral ends for
  any reason. Each recovery pass and each `Inspect` resolves the notices
  whose deferral is gone, so an end outside a pass (a cancel, or a terminal
  reconstructed by `Inspect`) is covered; the startup sweep covers a
  restart, since the deferral clock lives in memory.

## Refute-First Findings

- **A wrong permanent class turns a transient fault into a failed run.**
  Allowed: the export is retained, so a retry loses no work, and the errno
  rule keeps operating-system open failures retryable
  (`TestMissingReleasedContentClass`).
- **A hook call under the driver's lock could deadlock against a store
  write.** Disproved by trace: the raise runs after `deferExportRecovery`
  unlocks, the resolve reads under the lock and calls after unlocking, and
  `Inspect` resolves in a deferred call after every unlock.
- **A store write from `Inspect` could block on an engine transaction.**
  Disproved by trace: no engine caller holds a store transaction around
  `Inspect`, and `Inspect` already writes outcomes when it retries a pending
  terminal.
- **Concluding in recovery drops the authenticated usage the abandonment
  carried.** Confirmed and fixed: `recoverExported` commits the
  missing-content failure itself with the usage extracted from the
  authenticated export, rather than through `reconcileIntent`'s catch-all.
- **A missing `manifest.json` still retried for the grace.** Confirmed by
  the fresh-context review and fixed: every read of a released file, not
  only the persistence reads, marks "not found" as missing content.
- **A transient export-record lookup error could fail an import that had
  already committed.** Confirmed by the fresh-context review (a crash
  during the directory's removal) and fixed by the lookup rule above.

## Revisit When

The deferral clock or the grace becomes durable, or #3440 lands a typed
failure cause the missing-content class should carry.
