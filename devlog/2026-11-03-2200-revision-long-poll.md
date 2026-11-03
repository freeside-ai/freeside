# The Revision Heartbeat Long-Polls

Contract change for #2522. `GET /sync/revision` takes an optional `after`;
with it the daemon holds the request while its revision equals `after`, for
at most 25 seconds. The app's heartbeat loop now waits on that instead of
sleeping 15 seconds, so a daemon change reaches the clients in about a
second. Plan §5.14 already allows a latency channel; correctness still rests
on the heartbeat's gap, epoch, verdict, and lineup checks.

## Decisions

- **Chose a long poll on the existing heartbeat over a changes feed,
  server-sent events, or a WebSocket.** It adds one optional query parameter
  and no new response shape, and every answer is the same `ServerRevision`
  the client already judges. Rejected: a push channel, which would need its
  own reconnection, ordering, and replay rules for a gain the long poll
  already delivers.
- **Chose a process-local broadcast on `store.Store`** (a channel closed at
  every `Write` commit, `NewEpoch`, and both restore paths) over polling the
  database in the waiting request. A waiter takes the channel before it reads
  the revision, so no commit between the read and the wait is lost. Restore
  signals too because `freeside-signet-dev` restores while serving.
- **Chose to answer an epoch change even when the revision equals `after`.**
  A restore can leave the revision at the client's cursor while every cursor
  is dead; the waiter compares the epoch it first read.
- **Chose to re-authorize the device after the wait.** The authenticated
  wrapper checks the credential before a wait of up to 25 seconds, so a
  device revoked meanwhile would otherwise get state. It gets 401.
- **Chose `http.Server.RegisterOnShutdown` to release waiters.** Without it
  every daemon stop with a client attached waits out the 5-second drain
  budget and `Close` returns `context.DeadlineExceeded`.
- **Chose to let the app's long-poll answer only trigger the shared refresh
  round, never be adopted directly.** This departs from the plan comment on
  #2522, which split `performHeartbeat` so the long poll would run the gap,
  epoch, verdict, and lineup branches itself. Doing that outside a round
  would call `bootstrap()`, which bumps `syncGeneration` under a concurrent
  manual round and returns it without a stamp, the case the
  `refreshAfterCommit` comment forbids. Routing through `refresh()` keeps
  every judgment in one place at the cost of one immediate heartbeat per
  answer, which is less traffic than the old 15-second poll at idle.
- **Chose to hold an unchanged or failed long-poll answer to the 15-second
  interval.** A conforming daemon answers an unchanged state only at its
  25-second bound, so this costs nothing there; it stops the loop spinning
  against a transport failure or a daemon too old to honor `after`, which
  ignores the parameter and answers at once.
- **Chose a 1-second floor after a change and a 35-second client-side cap on
  each long poll.** Without the floor every commit costs every device a
  heartbeat and a bootstrap, and a burst of engine writes would chain rounds
  back to back on the daemon's single SQLite connection; one second still
  meets the issue's "about a second". Without the cap a connection that died
  silently (a network handoff, an iPhone app suspended in the background)
  holds the loop until URLSession's 60-second timeout, which equals the
  staleness threshold.
- **Left an epoch rotation that lands before the waiter's first read to the
  bound.** The waiter compares epochs from its own first read, so a rotation
  between the client's round and its long poll, at an unchanged revision, is
  learned up to 25 seconds late. Catching it would need the client to send
  its epoch, a second parameter the contract does not have; a production
  restore usually moves the revision too, and the next round still judges the
  epoch correctly.

## Revisit When

- A client needs per-resource invalidation rather than "something changed",
  or the bootstrap grows large enough that a full resync per change costs too
  much; then a changes feed earns its complexity.
- A response-buffering middleware (#2523's compression) wraps the handler, if
  it would delay the early answer.
