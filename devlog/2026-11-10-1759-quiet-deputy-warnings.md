# Quiet Deputy Warnings

Decision record for #3029: two deputy warnings, `task naming queue full`
and `record device activity`, repeated often enough to bury the log in the
Wave 9 exit run (#2386). The note is mandatory because the change touches
`internal/signet/device.go`, a Tier A path (client authority), though the
activity write grants nothing.

## Dedupe the Naming Queue Over Logging the Drop at Debug

Chose to skip an enqueue for a task already queued or being named, over
keeping the copies and lowering the drop's log level. The warning was a
symptom: every dispatch pass re-enqueues each run it touched, so copies of
one task filled all 32 slots while the single worker waited on one
inference call, and a genuinely full queue was then indistinguishable from
a flood of one task. With dedupe, the queue holds distinct tasks, and a
full queue (32 distinct tasks waiting) still warns, once per full spell.

- **Release after naming, not at dequeue.** A pass that runs during the
  inference call would otherwise queue a second copy. The worker loop is
  the queue's only reader, so every dequeued id is released.
- **The re-enqueue of started runs stays.** It is what names a task after
  a fallback or a restart; a released id can be queued again, so dedupe
  keeps that path working and makes it cheap.

## Debug for a Canceled Activity Write Over Detaching It

Chose to log an activity write at debug when the request's own end canceled
it (the request context is done and the error carries that context's
error), over running the write under `context.WithoutCancel`. A detached
write would keep a goroutine waiting on the write lock for a client that is
gone, to record an instant the device's next request records anyway. Any
other failure still warns.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **A real database failure logged at debug: disproved.** An error that
  wraps `context.Canceled` while the request is live sees a nil
  `ctx.Err()` and warns. `SQLITE_BUSY` and the foreign-key failure carry
  no context error and warn; a test pins the foreign-key case.
- **Authorization or serving changed: disproved.** The write returns
  nothing and the handler runs on every path.
- **A leaked id blocks a task's naming: disproved.** The worker is the
  only reader and releases on every dequeue, error or not.
- **Nil-map write from an engine built without `New`: disproved.** Those
  engines have no inference client, and `SupportsSite` returns false on a
  nil client before the map is touched.
- **A cancel after begin can still warn: allowed.** If the client hangs
  up after the begin succeeds, `database/sql` rolls the transaction back
  and the commit returns `sql.ErrTxDone`, which does not carry the context
  error. Catching it would teach signet a `database/sql` sentinel for a
  narrow window; the observed noise was the begin waiting for the lock.
- **Dedupe is by task while the queue holds a run: allowed.** If a queued
  run is fenced when the worker reaches it, a skipped sibling run of the
  same task waits for the next pass, which re-enqueues it while its intent
  is pending. Two runs of one task pending at once is rare, and it heals.

Revisit when the deputy log shows `record device activity` warnings that
name `sql: transaction has already been committed or rolled back`.
