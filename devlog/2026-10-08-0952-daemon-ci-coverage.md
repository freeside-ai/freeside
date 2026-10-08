# Preserve Complete Coverage While Sharding Daemon CI

Chose compiled test discovery and whole-family sharding over a maintained
allowlist because new tests, runnable examples, and default fuzz seeds must
join CI automatically. Package discovery includes packages without tests.
Weight entries influence balance only; they never decide membership.

Chose one rest-of-daemon runner and initially two integration runners per
platform, with independent build/static checks. The hosted whole-gate
comparison determines whether more integration runners are warranted. Go's
default concurrency and ten-minute package timeout remain binding. Reducing
coverage or raising the timeout would conceal the scheduling problem.

The first two-shard hosted comparison missed the whole-gate target despite
passing both integration shards below five minutes. Chose three shards for
the next comparison, while narrowing each integration runner's compiled
discovery to its own package. The rest runner still discovers every package,
and execution reconciliation proves the complete per-platform union.
The macOS aggregation predicate runs on Linux because it consumes job results
only; its required build and test work remains on macOS. A separate macOS
runner for that predicate added queueing without platform coverage.

## Coverage and Failure Boundaries

A valid plan assigns each discovered family exactly once and has no empty
partition. Each execution must produce a single completed run or default skip
for every selected case, complete every selected package, and introduce no
unselected family. Comparing the platform's complete execution union against
uncached baseline events checks all nested cases and fuzz seeds too.

Kept the visible Linux/macOS gates, with explicit dependencies and an always
condition. Their predicate requires every named child job to report success;
failed, cancelled, skipped, missing, or incomplete results cannot pass.
The matrix runs all members even when one fails.

Independent refutation found that shallow checkout hid the two parents used
to identify the tested merge. Fetching two commits deep preserves those
parents before the evidence step. It also found that rerunning only failed
final gates could produce a short, falsely complete timing sample. Whole-gate
measurement now requires the original attempt's complete work-job membership.
A regression reproduces the gate-only rerun and excludes it from the median.

Checks disproved coverage loss from anchored selectors: a real Go fixture
preserved prefix-overlapping test names, Unicode identifiers, nested names
with slashes/spaces, parallel subtests, examples, fuzz seeds, ordinary skips,
and a package without tests. Hermetic checks reject duplicate or missing
families, malformed or failed discovery, empty partitions, invalid or
conflicting shard inputs, missing execution, nonzero child exits, and false
success from child-job failures. Baseline events also passed full accounting
against the compiled inventory on both hosted platforms.

## Measurement and Cache Tradeoffs

Whole-gate latency includes setup, child work, cache restore, and evidence
upload. Initial queue delay is separate. Failed and cancelled attempts stay
in the report; only complete successful uncached attempts enter its median.
JSON instrumentation applies to both measurement variants and is identified
separately from the scheduling change.

Freeze the production base for controlled before/after samples so unrelated
merges cannot repeatedly invalidate collection. Run the batches sequentially
after the first concurrent comparison exposed shared macOS runner queueing.
Final integration verification still targets the current default branch;
the PR identifies the controlled benchmark's distinct production base.

Separate build-cache keys avoid concurrent saver collisions, and legacy
fallback keys let new jobs use the same warm main caches as the baseline.
PR build caches stay restore-only. Module caches remain shared because their
contents depend only on go.sum. Extra runners repeat checkout, discovery,
compilation, and cache restoration; the measured runner cost accompanies
the latency result.

Revisit when added families unbalance integration runners, repeated cache or
queue overhead erases the latency gain, or the default test recipe changes.
