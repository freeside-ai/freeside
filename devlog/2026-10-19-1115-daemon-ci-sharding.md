# Account For Sharded Tests Before Accepting The Gate

Use compiled test discovery and raw execution accounting instead of treating
a successful matrix as coverage proof. A shard can exit successfully after a
selection mistake, and a saved report can outlive the execution it describes.
The gate therefore re-accounts raw events, verifies the exact shard set and
all package/family assignments, and binds the artifacts to one tested commit,
run attempt, platform, and tool/image identity. A separate unsharded compiled
inventory and case union remain necessary for the performance comparison.

Start with three integration shards and one rest shard per platform. Keep
Linux static checks independent and the short macOS build in its rest job.
Avoid a serial build/artifact prerequisite because the accepted metric
includes allocation, restore, compilation, and aggregation overhead. More
runners incur setup and cache costs as well as reducing test execution time.
Measure that whole duration when evaluating the layout. Share integration
cache restores and allow only one successful
main-push writer per group; per-shard archives would multiply cache pressure.

Preserve the local slot limiter, real-Git wrapper, full default suite,
timeouts, concurrency, and opt-in skips. Weights are scheduling hints, never
an allowlist. The rest shard discovers only its own
packages so it does not compile integration solely to discover tests it will
never execute. The complete `go list` package inventory still guards against
omitting a whole package.

On October 19, 2026, the owner accepted the measured incremental speed and
reliability improvement instead of continuing to hold sharding for #1877's
six-minute and 50% whole-daemon targets. This revises the earlier choice to
defer sharding recorded in `2026-10-08-0952-daemon-ci-coverage.md`; that frozen
note remains the record of the earlier decision.

The isolated comparison reduced Linux gate time from 369 to 223 seconds.
All macOS integration shards passed, with a maximum execution of 187.231
seconds, while the unsharded baseline timed out at 600.050 seconds. Complete
case coverage matched the successful unsharded reference on both platforms.
These results justify accepting the incremental improvement without claiming
that the original whole-daemon target was met. The candidate took 590 seconds
to finish both daemon gates; its macOS rest job waited 212 seconds and then
ran for 355 seconds. Added runner work and allocation pressure remain costs.

The original optimization target and hosted cold-cache validation remain in
[#1877](https://github.com/freeside-ai/freeside/issues/1877). The owner accepted
the current evidence for this delivery, including those verification gaps.
Failed and excluded attempts remain in the
[measurement record](https://github.com/freeside-ai/freeside/issues/1877#issuecomment-6277577623).
Overlapping baseline/candidate workflows saturated the observed macOS
capacity, so their timings cannot establish an isolated speedup or be
attributed solely to the Actions incident. Failed baseline attempts remain
useful timeout evidence, but cannot enter a successful-completion median.

The independent refutation pass found that cache-size measurement could fail
before compilation on a cold miss because Go had not created its cache
directories. Create those directories before restore and measurement. Keep
other filesystem failures visible.

The same pass found that finished daemon gates could admit a still-running
PR attempt to the timing median, with only part of its total runner cost.
Retain its daemon timing for diagnostics, but count only a terminal,
successful attempt with complete runner-cost data in successful medians.

Hosted collection also showed that GitHub's attempt endpoint gives a rerun
its own creation time, after the accepted request. Validate a recorded rerun
trigger against the canonical run's original creation time, not the new
attempt record. Preserve both responses so the timing can be reproduced.

The first retry supplied complete candidate execution for all 398 integration
families on both platforms. Replace the historical weights with the larger
observed family duration from Linux and macOS (a 0.01-second minimum keeps
skipped or sub-resolution families valid scheduling hints). The source is
run `39284960193`, attempt 2, on production base `e43008f`. Keep three shards:
all measured integration executions were below 201 seconds, while two bins
of the new family weights each exceed 330 seconds and four would add runner
allocation pressure. These estimates guide layout; only measured package
execution establishes the 300-second limit. Allocation and the macOS rest
job, including its 65-second cache restore, remain limiting factors for the
whole-daemon target.

Revisit the layout and weights after matched hosted runs establish the
rest-shard, integration, cache, and allocation costs, or whenever production,
tests, toolchain, or runner images change enough to invalidate that evidence.
