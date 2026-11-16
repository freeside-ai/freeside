# The Publication Lane Runs Each Run's Tasks in Its Own Goroutine

Agent decisions for #3214, under the contract the owner approved on
2026-11-13 (the proposal on tracker #3201). Tier A by path: the lane is
on the publication-decisions list.

## Decisions

- **Key the lane by run, not by task.** Each pass starts each run's
  pending tasks in that run's goroutine on `keyedrun.Group`, one after
  another in the order the pass read them. A per-task key would let a
  run's continuation or successor task overlap its parent, and every
  per-run record the lane writes (holds, reviews, the verification
  reservation, the run's quarantine) assumes one writer per run. Two runs
  that name one declared branch now publish at once, where the serial lane
  ordered them. The forge closes that race, not the lane: the head push
  only creates its ref (`publish.pushArgs`, an empty
  `--force-with-lease`), and the publisher refuses a branch at another
  commit or a pull request with another identity. One run publishes and
  the other is refused, as it would be serially.
- **The loop doesn't wait; the single pass does.**
  `RunProductionPublications` starts goroutines and returns to its
  cadence, so a task queued behind a long verification starts on the
  next pass. A run whose goroutine was working when the pass read its
  pending tasks is skipped, not queued, so a pass never blocks. The busy
  set is read before the pending tasks, not checked at start: a
  goroutine that ends between the read and the start would otherwise
  leave the pass holding a task that goroutine already finished. `ReconcileProductionPublications`, the
  one-pass call tests and tools use, waits for what it started, so its
  counts and errors keep their meaning.
- **No new cap.** The verification slots, the memory budget, the review
  pools, the judgment sites (#3225), and the one-build image gate already
  bound the real work. A lane-wide cap would be a second, coarser limit
  on the same resources.
- **Correctness errors reach the next pass.** Each goroutine records its
  counts and errors in a collector the next pass drains, so a
  non-retryable error still stops the loop, one pass late instead of
  vanishing into a log. A pass drains the collector before it starts any
  task, so an earlier error stops the lane before more tasks begin (the
  closed-workflow sweep that precedes it still runs once), and a pass a
  drain refuses still drains it.

## Invariants and Their Guards

The serial loop and `reconcileMu` carried each of these implicitly. Each
now has its own guard and a test that fails without it.

| Invariant | Guard |
| --- | --- |
| A run's tasks never overlap and keep read order | The per-run group, and the busy set read before the pending tasks |
| Retry, lag, and review-deadline maps | `pacingMu` and its accessors |
| Startup review recovery runs once, before any task | Runs under the pass lock on the first pass |
| End-of-task proof and operator release never beside the run's task | They hold the run in the group; a busy run refuses the release |
| Launch-wait keepalive while any task runs | Holder counting in `launchWaitKeeper` |
| Restart readiness counts running tasks | Each goroutine counts as publication work |
| Closed-workflow observation | Its own mutex, plus a per-run hold |
| Correctness errors stop the lane | The outcomes collector |
| Shutdown joins every task and leaves it pending | Cancel, then `laneRuns.Wait`, which refuses any later start |

## Keepalive From the Earliest Holder

Each goroutine keeps the waits refused since the start of the pass before
the one that started it, which is what one serial pass kept. The keeper
keeps from the earliest such start among running goroutines, so a long
task keeps alive the waits its own run will pursue. Chose that over a
per-pass window, which let a newer launch overtake an older run's wait
once the long task outlived the lease (#2953's failure, now across runs).
The cost is that a wait nothing pursues lapses only after the longest
running task ends; the hold retry (30 s) is far under the lease (5 min),
so a live wait is refreshed long before that matters.

## Refute-First Findings

A fresh-context reviewer tried to break the change before push.

| Finding | Outcome |
| --- | --- |
| A pass could re-run a task its run's goroutine finished after the pass read it | Confirmed, fixed: busy set read first, busy runs skipped, proofs included |
| A pass started work before returning an earlier goroutine's error | Confirmed, fixed: collect first |
| A pass a drain refused dropped goroutine errors | Confirmed, fixed: the refused pass collects |
| An operator release during shutdown could add to the group while it waits | Confirmed, fixed: `laneRuns` refuses starts once Wait begins |
| The release read its held reservations outside the run hold | Confirmed, fixed: it re-reads them inside the hold |
| Feedback transitions run beside task goroutines | Allowed: the engine loop always ran them beside the lane, and `requireUnchangedFeedbackItem` compares superseded items without timing |
| Shutdown can wait out an operator release's proof | Allowed: bounded by that one operator request |

A second fresh-context reviewer read the fixes and found nothing
blocking.

| Finding | Outcome |
| --- | --- |
| A proof could re-prove a reservation an operator released after the pass read it, filing a false alert | Confirmed, fixed: the proof re-reads that the reservation is still open |
| The busy-set comment claimed a snapshot never misses a held run | Confirmed, fixed: it can, but only once the run's work has returned |
| No test pins that the pass reads the busy set before the pending tasks | Allowed: the skip is tested at `startRuns`; pinning the order needs a hook inside the store read |
| A release during shutdown reports the lane busy | Allowed: documented in the README; retrying is the right response either way |

Guard-removal mutations, each run alone against the test named for its
invariant: all twelve fail their test. They remove the pacing lock, the
per-run sequencing, the recovery-once flag, collect-first, the closed
observation's run hold, the cancel check between tasks, the restart
readiness count, the release's run hold, the busy-set skip, the refusal
after Wait, the proof's run hold, and the proof's re-read of its
reservation. The ordering test first passed under one mutation by
scheduling luck; it now runs in a `testing/synctest` bubble, so the check
happens once every goroutine is blocked, and it fails on every run.

Revisit when the lane gains a cap of its own, or when one run's tasks
need to overlap.
