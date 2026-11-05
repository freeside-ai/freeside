# Claude Usage Collector: Memory Only, Fed By Writer Turns

Decision scope: #1717. Reads the capture #2543 added
([`2026-11-04-0929-claude-writer-usage-capture.md`](2026-11-04-0929-claude-writer-usage-capture.md))
on the path the spike chose
([`2026-11-03-1457-claude-usage-share-spike.md`](2026-11-03-1457-claude-usage-share-spike.md)).
Tier A by path: the collector lives in `daemon/internal/ward`.

## Decisions

- **The collector answers from memory and makes no request.** The spike
  found no read that carries the share under the setup token without an
  inference turn, and the issue forbids inference only to observe usage.
  So `ward.ClaudeUsageCollector` holds the latest capture per enrollment
  (highest generation, then newest `observed_at`) and answers
  `CollectUsage` from it. A test pins its imports and the `exec` names it
  uses, so it can't open a connection, start a process, or read a
  credential.
- **Memory only, over a persisted capture.** Chose losing captures at
  restart over a new table or a column on the usage record: either is a
  contract change, and the stored readings already survive a restart.
  After a restart every Claude pool's collection fails ("no writer turn has
  reported usage under enrollment ... since the daemon started") until a
  writer turn runs under its collector; the failed path keeps the readings,
  as #1716 built it. Rejected answering `unsupported` from empty memory:
  the record refuses windows under an `unsupported` attempt, so it would
  erase the readings. Rejected `supported` with no windows for the same
  reason: a full report replaces the window set.
- **A capture is a full report, not a partial update, so only a capture
  with a `get_usage` answer is held.** The capture arrives once, with the
  stage result, so its rate-limit events fold into the one answer and
  nothing goes through `Coordinator.ApplyPartial`, which refuses a reading
  before a succeeded attempt anyway. A capture with events and no answer (a
  specification turn, which sends no `get_usage`, or a writer turn whose
  answer was refused or null) is dropped: as a full report it would replace
  the pool's windows with the events' alone and erase the weekly reading.
  Rejected merging its events into the held answer, which would restamp the
  older shares with the new turn's time, and a partial update, which the
  issue rules out. The cost: a `rejected` event from such a turn doesn't
  mark the window until a turn with an answer reports it; a writer turn's
  quota refusal still marks it through the quota park.
- **Status mapping.** `rejected` reads exhausted; `allowed` and
  `allowed_warning` read allowed, as does a window with no event. Any other
  status is ignored and named in the answer's detail, so a new CLI token
  can't mark a pool exhausted by guess. An event's reset wins over the
  answer's: the event comes from the turn's own response headers. A window
  named only by an event gets status, reset, and length, and no share.
- **Fixed window table.** `five_hour` is 5h and `seven_day` is 7 × 24h; the
  router ranks by the seven-day length (owner comment on #1717,
  2026-11-04). Other names are left out and named in the detail, since
  their length is unknown. A capture with no known window fails rather than
  report an empty set.
- **The capture trigger skips backoff.** `Coordinator.CaptureArrived`
  clears the pool's backoff and starts a collection, and a collection in
  flight gets another after it even if it fails into backoff. Backoff spares
  the provider; asking a memory-only collector costs it nothing, and the
  backoff an empty memory builds after a restart (up to 6h) would otherwise
  hold the first capture. A collector that calls the provider must never be
  fed by this trigger; its doc comment says so.
- **Only the pool's collector at its current generation triggers.** The
  engine reads the enrollment and generation from the stored admission's
  agent binding and the pool from its identity (the quota park's lookup,
  now `admittedPool`). A capture from another enrollment on the pool is
  held but starts nothing; one under a replaced generation starts nothing
  and could not reach the record, since the coordinator asks for the
  current generation and the store discards a replaced one.
- **Newer wins, so replays are no-ops.** Every collection of a terminal
  reports its capture again. The collector keeps a capture only when it is
  newer than the one held, and the sink starts a collection only for a kept
  one.
- **Advisory on the terminal path.** A read error, a missing binding or
  pool, or a sink error is logged and the terminal is collected as before.
- **Plan §10 wording.** §10 said a separate network read collects usage.
  It now says Claude's readings come from the writer turn's own answer and
  are as current as the collector's last writer turn. This is a wording
  change under plan §9: revision 77 committed to settling Claude's path by
  spike, so there is no revision bump.

## Refute-First Review

A fresh-context reviewer tried to refute the change before commit. It could
not refute these: a replayed or forged `observed_at` locking out later
captures (the stamp is the daemon's export time, not the agent's); the store
refusing the re-sent identical readings of a second collection (it accepts
them, checked); a capture reaching another pool or enrollment; the ordering
of the backoff skip against a collection in flight; reset timers after a
cleared backoff; and a sink error failing the terminal.

Its findings and what became of them:

- **Blocking: an event-only capture erased the weekly reading,** confirmed
  by a probe. A specification turn's transcript carries a rate-limit event
  and no answer. Fixed as above, with tests in `ward` and in the daemon's
  sink.
- **The production wiring had no test.** Fixed: the collector choice moved
  into `usageCollectorsFor`, and a `cmd/freesided` test runs the production
  sink over the real collector, coordinator, and store.
- **Declined: a reassignment between `CaptureArrived`'s read and its
  backoff clear** can give the new collector one extra memory-only
  collection and a reset failure count. Only Claude providers produce a
  capture, so the new collector also answers from memory and the extra ask
  costs the provider nothing.

## Residual Risks, Accepted

- **The capture is agent-side output.** #2543's note records that the agent
  can forge the CLI's stdout. A forged capture can now set a pool's
  readings: a high share or a far reset changes how the router ranks the
  pool, and a forged `rejected` marks a window exhausted until its reset,
  which the router then passes over. That grants nothing: the record is
  observation, admission never reads it, and the quota park's provider
  refusals, also reported from the agent's side, already carry the same
  exposure (AGENTS.md, Assurance Tiers: untrusted input that only feeds
  scheduling). The next writer turn under the collector replaces it, and
  the operator sees the readings and their source turn's time.
- **The restart gap.** Until a writer turn runs under a pool's collector,
  `refresh-usage` and every doctor pass report a failed attempt.

Revisit when the CLI pin moves off 2.1.282 (status tokens and window names
are the CLI's), or when the restart gap proves costly enough to persist the
capture.
