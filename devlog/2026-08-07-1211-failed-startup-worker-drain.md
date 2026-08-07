# Failed Startup Worker Drain

Issue #547 changes cleanup ordering after daemon background workers have
started, so it carries the destructive-path refute-first record. PR #598.

## Decision

Chose a failure-only cleanup registered immediately after the three base
workers launch, and ordered as signal-disposition restoration, context
cancellation, forced HTTP-server close, then worker-group drain, over relying
on the earlier context-cancel defer or calling `daemon.Close`. Restoring signal
disposition first keeps a second SIGTERM as the operator escape hatch if a
worker wedges while draining. Registering the cleanup beside the launch makes
Go's LIFO defer order place the drain before both startup-session cleanup and
the store close. The earlier cancellation alone did not wait for workers,
while `daemon.Close` also closes the store and performs normal-shutdown session
and HTTP semantics that are not valid for a partially constructed daemon.

Chose `http.Server.Close` over the normal path's bounded `Shutdown`. Startup
has already failed and returns no daemon or usable API address, so preserving
in-flight request service has no operator-visible contract. Closing listeners
and active connections promptly lets the HTTP worker join the same complete
drain as reconciliation and local backup, before any shared store dependency
is released.

Chose two test-only seams at the composition boundary over restructuring the
long `run` constructor: one injects the otherwise difficult-to-force failure
after the base workers start, and one observes the exact store-close boundary.
They keep production control flow unchanged while pinning both the cleanup
ordering and bounded return required by #547.

## Refute-First Findings

- **Confirmed and fixed:** on every error return after the three base workers
  launched, the pre-existing LIFO order closed SQLite before cancellation, and
  no failure cleanup waited for `d.wg`. The new defer cancels, closes the HTTP
  server, and waits before the earlier store-close defer can run.
- **Confirmed and fixed (fresh-context review):** the first regression
  assertion inferred ordering from logs after `run` returned, which was
  scheduling-dependent and did not observe the destructive boundary itself.
  The final test snapshots worker logs synchronously in the store-close defer
  and requires the reconcile loop's stop record there.
- **Confirmed and fixed (automated review, P1):** the first failure-only drain
  waited for workers before `closeStartupSessions` restored signal disposition.
  A slow or wedged worker could therefore absorb the operator's second SIGTERM
  and leave SIGKILL as the only exit. The drain now calls `stop` before
  cancellation and waiting; the destructive-order test records that signal
  restoration precedes the reconcile worker's stop record and the store close.
- **Rejected by verification:** cancellation plus server close does not leave
  the failed constructor hung behind one of the three launched workers. A
  bounded-return regression test covers the failure path, and both new tests
  passed 20 consecutive race-enabled runs.
- **Rejected by verification:** the drained workers do not retain a store use
  past the close boundary in the forced-failure case. Race-enabled coverage
  requires reconciliation to be stopped before close and rejects both
  `reconcile pass failed` and `sql: database is closed` evidence.
- **Accepted by decision:** normal graceful HTTP shutdown and the composition
  coverage of workers launched only by later scheduler setup remain outside
  this unit. The destructive-order regression pins the three workers already
  live at the defer's registration point; the same `d.wg` wait still drains
  any worker added later before a subsequent constructor failure.

Revisit when a new background worker can launch before this failure-only defer,
or failed startup must preserve in-flight HTTP requests rather than abort them.
