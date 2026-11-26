# Retry Freed App Authority Lock in Prod

Unit #3246 lets a `prod` that started without execution because another process
held the App authority writer lock (`app_authority_in_use`) restart on its own
once that holder lets go, without an operator running `freesided execution
resume`. The issue body carries the contract; this note records the design
choices, the earlier record it revises, and the refute-first check of the
interleavings.

## Record This Revises

**"Allowed by decision"** in
`devlog/2026-11-16-0950-app-authority-writer-lock.md`, which accepted that a
`prod` started beside a short lock holder stays driverless until an operator
runs `freesided execution resume` and noted #3246 as the follow-up. Short
holders such as a standalone `preflight`, `setup`, or `onboard` run overlapping
a launchd start no longer leave `prod` driverless until manual intervention.
`rig_lease_present` and `execution_paused` stay manual operator decisions.

## Decisions

**Restart `prod` rather than composing the driver in place.** Chose asking
`daemonshell` for the existing restart (`RequestRestart`, exit status 0 under
launchd's `KeepAlive`) over composing the driver and loops inside the running
driverless process. Restarting runs `prod`'s full gated start order again,
reuses `convergeExecutionConfiguration` to conclude the `app_authority_in_use`
card once a driver runs, and avoids a second live composition lifecycle. The
retry check holds no lock past its return and opens no keystore or authority
store.

**Live in `executionPauseWaiter`, not a new supervised component.** Chose
adding the retry check to `executionPauseWaiter` over registering a new
supervised worker in `daemonshell`. The pause waiter already runs every five
seconds in every `prod` started with a driver (including every
`app_authority_in_use` start), already reads the execution pause section first
on every step, already holds `leaseRoot`, and already owns `RequestRestart`.
Keeping both in one loop makes the pause-over-retry priority structural: a
requested pause read in the same step as a freed lock is always handled by the
pause release path, never bypassed by the retry.

**Read the rig lease manifest before taking the lock.** Chose checking
`daemonlock.RigManifestPresent(leaseRoot)` first, then running
`gateProdAppAuthority(stateDir, leaseRoot)` (which takes the lock and checks
the manifest again) and closing the lock before returning, over calling
`gateProdAppAuthority` directly. A deputy takes its rig lease before its writer
lock. Reading the manifest first ensures a polling `prod` never contends with a
deputy that already published its lease and is about to take the lock. The
second manifest check inside `gateProdAppAuthority` closes the window where a
deputy publishes its lease after the first read and before the lock.

**Take a momentary real lock to test availability.** Chose acquiring and
immediately releasing the real `app-authority-writer.lock` over probing without
locking (such as `F_GETLK`, which does not inspect `flock` locks reliably
across platforms) or keeping the lock across restart. The check lasts a few
system calls every five seconds and runs only when no pause is in force and no
rig lease manifest exists. A standalone `preflight`, `setup`, or `onboard`
started in that exact instant can be refused once, matching what a `prod` start
already does to them.

**Log check errors at `Warn` and keep serving.** Chose warning and continuing
over returning the retry check's error from `step`. Returning an error from
`executionPauseWaiter.run` triggers a durable stop of
`ComponentExecutionPause`, which would take down a driverless `prod` that must
keep serving pairing and state. This matches `watchUnused`'s handling of an
unreadable lease root.

## Refute-First

Interleavings and failure paths checked against the single-writer invariant:

- **Disproved by a check: deputy takes its lease between the retry's manifest
  read and its lock.** After taking the lock, `gateProdAppAuthority` performs
  its own, unchanged manifest read, finds the deputy's manifest, closes the
  lock, and returns `ExecutionRigLeasePresent`; `prodAuthorityFree` returns
  `false`, so `prod` does not restart and leaves the lock free for the deputy.
  Disproved by `TestGateProdAppAuthorityDeclinesBesideADeputy` ("deputy holds
  the lease, not yet the lock" in `app_authority_gate_test.go`), which runs
  `gateProdAppAuthority` with a manifest present and the lock free.
- **Allowed by decision: deputy takes the lock while the retry holds it.** The
  retry acquires the lock only after seeing no manifest, so a deputy following
  lease-then-lock can race here only if it publishes its manifest after the
  retry's first read and tries the lock before the retry's second read and
  `Close`. When refused, the deputy fails in `acquireAppAuthority`
  (`daemon/cmd/freesided/preflight.go`) before opening the keystore or
  authority store, with the error naming `freesided execution pause`, and the
  operator reruns it; meanwhile the retry's second read finds the manifest,
  releases the lock, and does not restart. This window used to exist only once
  per `prod` start and now recurs at each retry check, every five seconds,
  while a `prod` sits in `app_authority_in_use` with no manifest; it stays a
  few system calls wide. No two writers can hold the authority at once, and
  the cost is at worst one refused, rerunnable command.
- **Allowed by decision: standalone lock holder (`preflight`, `setup`,
  `onboard`) races the retry.** The retry holds the lock only for one
  `RigManifestPresent` check and closes it before returning. If the holder
  acquires first, `gateProdAppAuthority` returns `ExecutionAuthorityInUse`
  without disturbing the holder's lock
  (`TestProdAuthorityFreeChecksManifestBeforeLock` and
  `TestGateProdAppAuthorityDeclinesBesideADeputy` in
  `app_authority_gate_test.go`). If the retry acquires first, the holder fails
  in `acquireAppAuthority` before opening the authority, with the error naming
  `freesided execution pause`, and the operator reruns it; like the deputy
  race above, this window recurs every five seconds while `prod` sits in
  `app_authority_in_use` with no manifest and stays a few system calls wide.
  Two writers never hold the lock at once.
- **Disproved by a check: pause requested in the same step as a freed lock.**
  `executionPauseWaiter.step` reads `execution_pause` first and skips the
  retry branch whenever `pause != nil` (`requested` or `released`). Disproved
  by `TestExecutionPauseWaiterRetriesFreedAuthority` ("pause wins over free
  lock", "blocked requested pause holds back free lock", and "released pause
  never retries" in `execution_pause_test.go`): a `requested` pause waits on
  any readiness blockers without retrying, drains to `released` once readiness
  is clear (zero in a driverless `prod`) and requests the restart that brings
  `prod` back `execution_paused`, and a `released` pause never retries. If a
  pause is requested after `step` read `pause == nil` and the retry asks for a
  restart, the restarted `prod` reads the `requested` pause from the database
  and serves it through the waiter.
- **Disproved by a check: lock leaked on an error path or after restart.**
  Inside `gateProdAppAuthority`, an error reading the manifest closes the lock
  before returning (`TestGateProdAppAuthorityFailsOnAnUnusableDirectory` in
  `app_authority_gate_test.go`). Inside `prodAuthorityFree`, any non-nil lock
  is closed before returning (`TestProdAuthorityFreeChecksManifestBeforeLock`
  in `app_authority_gate_test.go`), and a `Close` error returns `false, err`
  so `prod` does not restart with an unreleased lock.
  `TestExecutionPauseWaiterRetriesFreedAuthority` ("lock held then freed" in
  `execution_pause_test.go`) and
  `TestExecutionPauseHandsTheAuthorityToADeputy` (`daemon/cmd/freesided`)
  confirm the lock is free after the retry requests a restart.
  `AuthorityWriterLock.Close` calls `LOCK_UN` explicitly before closing the
  descriptor.
- **Disproved by a check: other driverless states running the retry.** `Start`
  (`daemon/internal/daemoncompose/start.go`) populates `retryStateDir` only
  when `gated == ExecutionAuthorityInUse`. In `execution_paused`,
  `rig_lease_present`, `not_configured`, and every stored-settings failure
  state, `retryStateDir` is empty. `TestExecutionPauseWaiterRetriesFreedAuthority`
  ("empty retry state dir never retries" and "released pause never retries" in
  `execution_pause_test.go`) proves the waiter never runs the retry check when
  `retryStateDir` is empty or a pause is released, and
  `TestExecutionPauseResumesOffline` (`daemon/cmd/freesided/execution_pause_test.go`)
  proves `Start` leaves the retry off for a `rig_lease_present` start even
  after the lease is released with the lock free.
- **Disproved by a check: repeated short lock holders.** Each retry restart
  happens only after the lock is observed free on a five-second tick, and the
  waiter requests at most one restart before waiting for cancellation
  (`TestExecutionPauseWaiterRetriesFreedAuthority` "lock held then freed" in
  `execution_pause_test.go`). The restarted `prod` takes the lock on start or
  re-enters `app_authority_in_use` with another five-second interval, so
  repeated holders cannot cause a tight crash loop.

The fresh-context review before push raised three more, each allowed by
decision:

- **Allowed by decision: the card says `prod` restarts with execution, but
  the restarted start can still fail.** A host check or composition failure
  after the retry leaves `prod` driverless under that failure's own card, so
  the operator is told why. The issue's acceptance fixes this wording.
- **Allowed by decision: a command refused during the check's hold reads
  that an executing `prod` or a deputy holds the authority.** The message
  in `acquireAppAuthority` predates this unit and lies outside its paths;
  the window is a few system calls wide and the command reruns.
- **Allowed by decision: a check that keeps failing warns every five
  seconds.** That matches `watchUnused`'s unreadable lease root, and the
  issue asks for a warning that leaves `prod` serving; a state directory
  that stays unusable also fails every App authority command, so the
  operator has other signs of it.

Revisit when `prod` gains an in-process driver lifecycle or when another
non-deputy process holds the App authority lock for long periods while `prod`
should not restart.
