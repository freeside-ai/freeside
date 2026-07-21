# Real-Process Daemon Kill Recovery (#41)

## Decisions

- **Use a kill-test-only daemon build over exported production hooks.** Chose
  the `freeside_kill_test` build tag and a file marker that blocks inside the
  daemon's fake `StageDriver` wrapper over adding engine observers, public
  flags, or timing-based polling. The wrapper is the exact external-effect
  boundary the test needs, the marker makes each SIGKILL deterministic, and a
  normal build compiles the checkpoint call to a no-op. The fixture still
  builds and runs the real `freesided` command rather than a helper process or
  an in-process reconstruction.
- **Map the matrix to durable effect boundaries, not source-line timing.** The
  three pauses are immediately before `Start`, immediately after successful
  `Start`, and after `Inspect` has committed a completed result but before the
  engine can accept it. This directly names the states the store and permanent
  fake must reconstruct: pending outbox intent, committed fake intent without
  a result, and committed fake result without an inbox acceptance.
- **A committed intent without a committed result fails closed.** Chose zero
  accepted results and zero completion advance over re-starting the provider
  invocation after restart. `NewStageDriverAt` deliberately reconstructs that
  state as a lost provider session, so the daemon reports
  `ErrInvocationLost`; starting again would contradict the durable fake's
  committed-intent dedup and the Section 5.3 at-most-once guarantee. The other
  two boundaries converge to exactly one accepted completion.

## Verification Finding

The real-process matrix confirmed that every boundary retains exactly one
invocation outbox row and one Run attempt; the permanent fake rejects a second
`Start`; the completion inbox contains zero or one row according to whether a
result was committed before death; and the feedback item and conversation
advance exactly the same number of times. Automated review found that the
nested daemon build did not initially inherit `go test -race`; a race-tagged
helper now adds `go build -race`, so repeated race-detector runs instrument
both the parent harness and the child daemon. The same review required atomic
marker publication, closing the test-only partial-read race with a
same-directory rename.
A separately built production `freesided` binary contained no
`FREESIDE_KILL_TEST` hook string, confirming the tagged implementation and its
environment controls were eliminated from the normal artifact.

Revisit when the fake `StageDriver` leaves the daemon composition or the real
workflow adds an explicit retry policy for lost provider sessions; either
change moves the correct checkpoint seam or the expected middle-boundary
outcome.

Scope: `daemon/cmd/freesided`, `devlog/`.
