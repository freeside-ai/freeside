# Usage Collection Rechecks Its Collector's Answer At The Write

#1716 runs usage collection over the pool usage record of #1715. The
collector's answer is a returned object that crosses a trust boundary: it
comes back from a call made outside any transaction, under a collector and
generation read before the call. These choices outlast the unit, because the
real collector (#1717) and the doctor (#1719) build on them.

## The Answer Is Judged Against Current State

Chose to reread the record and the collector's current generation inside the
write transaction, and to discard the answer whole when either changed, over
trusting what the coordinator read before the call. The store's generation
gate sees only new readings, so an unsupported answer, or a failure that
carries none, under a replaced generation would otherwise clear the windows
of a credential that is gone. A reassignment during the call discards the
answer the same way, and the new collector collects next.

Chose to verify each returned reading's enrollment and generation against the
request, and to record a mismatch as a failed attempt, over letting the store
gate decide. A reading stamped with a newer generation by a confused collector
could pass the gate; a failed attempt keeps the old readings and says why.

A later reading wins. When any reading landed during the call, such as a
partial update, the full answer is discarded rather than allowed to replace
it or drop its window, and a discard is not a failure, so it starts no
backoff.

A collection that settles after a reassignment changes nothing: a per-pool
reassignment count, taken when the collection starts, keeps the old
collector's failure from backing off the new one.

## No Reset Is Lost

The collection after a reset is the one the acceptance names, so a reset
stays owed until a collection lands after it, judged from the record: a
window read before its reset, or a refusal mark whose reset came after the
collector's last answer or before a failed one. A reset that fires while a
collection is in flight reruns after it, since that collection may have read
the pool first. A reset during backoff collects when the backoff ends. A
collection that fails or is discarded with a reset still owed retries once
the backoff ends, and no sooner than one backoff base, so a collector that
keeps answering without landing is asked at that pace at most. A reset that
passed while the daemon was stopped collects at start.

## The Provider's Throttle Delay Is Honored Uncapped

A refute-first finding asked to cap a collector's retry-after at the backoff
cap. Declined: honoring the provider's delay is the collector contract, a
shorter retry only earns another throttle, and a restart or reassignment
clears the backoff when an operator needs it sooner.

## Findings Declined As Unreachable

- **`ApplyPartial` arming from an older record.** It arms its reset timer just
  after its write commits, so a full collection could in principle commit and
  arm in between. That needs a whole collector call, a network request, to fit
  between two adjacent statements.
- **`Wait` before `Start`.** The daemon always calls `Start` first.

## Departures From The Plan Comment

- **Throttle as an error type.** `ward.UsageThrottleError` carries the delay,
  over a field on the answer, because a throttle is a failed call and a field
  would let a supported answer carry one.
- **`Start` and `Wait` over one `Run`.** The daemon starts the coordinator
  before it serves, so the first control request finds it running, and waits
  for it on shutdown.
- **Collection in every daemon mode.** The coordinator is wired whenever the
  daemon runs; a pool whose harness has no collector ends its collection
  before any call and writes nothing, until #1717 registers one.

Revisit when a collector needs to report partial readings as part of a
failure, which the failed-keeps-readings rule now forbids.
