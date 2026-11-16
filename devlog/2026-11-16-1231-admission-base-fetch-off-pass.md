# Admission Fetches a Run's Base Outside the Engine Pass

Unit #3229 (tracker #3201, part 7 of #3215's split) stops admission from
fetching a run's base from GitHub inside the engine's pass. Image selection
now reads the base inputs memo without waiting: a miss starts the fetch on
the memo's worker and holds the attempt with `engine.ErrBaseInputsPending`,
and a later pass selects from the finished fetch. It applies
`devlog/2026-11-15-1440-no-locks-across-network.md` to the one network call
that note left inside the pass. This note keeps the three choices the unit
made against a reasonable alternative.

## Decisions

- **Chose a reasonless invocation hold for a fetch in flight over a new
  `domain` hold reason or `input_unavailable`.** It follows the triage
  precedent in #3215's note: the fetch is the daemon's own short work, and
  nothing waits on an operator. A new reason would reach `api/openapi.yaml`
  and the app's hold presentation and make this a contract unit for a wait
  that lasts one fetch. `input_unavailable` would record a hold on every run
  at a new base, which since #3223 is routine, and that row isn't cleared
  when the invocation starts. A failed fetch still records
  `input_unavailable`, as a failed read did before.
- **Chose to serve a failed fetch for a retry backoff
  (`baseFetchRetryBackoff`, one minute) over refetching on the next pass.**
  The engine passes every 100 ms, so a base that keeps failing would be
  fetched about ten times a second. The failure keeps its own error, so a
  repository identity mismatch still reaches admission. The blocking read
  the start uses keeps no failure, since the start fails on it.
- **Chose a fetch deadline (`baseFetchTimeout`, five minutes) over relying
  on the transport's own timeouts.** A reasonless hold shows the operator
  nothing, so a fetch that hangs would hide the run's wait for as long as it
  hangs. The deadline turns a hung fetch into a recorded
  `input_unavailable` hold. Five minutes leaves room for a large clone.

The memo's `Close` cancels and joins its fetches before the driver and the
janitor close, and a lookup after `Close` starts no fetch, because the shell
closes the composition while an engine pass may still be running. After a
restart the memo is empty and the next pass fetches the run's base again.

Tests: `TestClaudeImageSelectorFetchesTheBaseOutsideTheSelection`,
`TestClaudeImageSelectorBacksOffAFailedFetch`,
`TestBaseInputsMemoFetchDeadline`, `TestBaseInputsMemoCloseJoinsItsFetch`,
and `TestPendingBaseFetchHoldsWithoutAReason` are new.
`TestDispatchHoldReasonCoversEveryHoldClass` counts the pending row among
the reasonless ones, the hold-classification golden gains the row, and the
existing selector tests select through `selectAfterFetch`, which waits out a
pending fetch, with their assertions unchanged.

## Refute-First Findings

A fresh-context reviewer tried to break the change before it was committed.
It found no blockers.

- **Allowed by decision: a first admission can stay pending while the tip
  outruns its fetches.** A run never admitted selects at the current tip,
  so if every fetch outlasts the tip watch's interval (one minute in
  operator mode, fifteen by default), each pass starts a fetch at a new tip
  and the run holds with no reason. Each fetch still ends by its deadline,
  so the cost is wasted fetches, not a hung one. Before this unit the pass
  blocked instead and admitted at the tip it had read. Accepted because a
  base fetch normally takes seconds, and bounding the pending time across
  bases would need state kept per run.
- **Confirmed and fixed: failed fetches were never forgotten.** A base whose
  fetch failed and that no run looks up again kept its entry for the
  process's life. Recording a failure now drops the expired ones.
- **Confirmed and fixed: the Close test counted a stray fetch on its own
  goroutine.** It now waits for the memo's fetches before counting.
- **Allowed by decision: no test checks that `read` no longer holds the lock
  across a fetch.** `read` runs at start, before the engine loop, so nothing
  contends for the lock there.
- **Disproved by checks:** lock order between the memo and its
  `keyedrun.Group`, a fetch started after `Close` began waiting, pending
  classified as `input_unavailable`, and a canceled fetch storing a failure.

Revisit when a composition serves more than one repository, since the memo
is keyed by base commit and layout on the assumption that one composition
names one repository's bases, or when runs are seen holding pending across
several tips.
