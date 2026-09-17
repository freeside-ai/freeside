# Present Task Status Separately From Phase Evidence

Issue: #1374

Chose task-local wrapping text over extending the shared stage rail. The row
needs an outcome and next-step cue as well as phase evidence; changing every
rail caller would broaden the work without clarifying the task list.

Use the daemon's task lifecycle for stopped and abandoned records. Keep
requested and failed cancellation distinct from confirmed stopping, and
retain confirmation separately when completion wins the lifecycle projection.
Finished is not synonymous with success. Select the current run by position;
a superseded predecessor does not make its successor historical.

Keep campaign approval and final-review binding unchanged. An open, bound
specification attention item supplies guidance, not approval. Pending or failed
cancellation suppresses approval and final-review guidance even if older
loaded items remain open. Otherwise, direct the reader to details rather than invent an
action or a queue-admission fact. Missing position alone is not queue evidence.
For a confirmed cancellation retained from an earlier episode, trust the
daemon's active lifecycle and preserve current guidance. A finished task can
also retain an older confirmation; label that only as a recorded fact rather
than claiming its current execution stopped. Do not reconstruct episode
ordinals in the client.

Visible text and accessibility use the same strings. Unknown specification
approval replaces the pending wording; it is not an additional contradictory
caption. Historical rows describe a current rail marker as the last recorded
phase, without changing the rail's underlying factual state.

The owner chose title case for task-row status and phase labels. Full guidance
sentences stay in sentence case; other shared-rail and timeline callers retain
their existing presentation.

## Refute-First Findings

Focused regressions reject guidance from closed, foreign-project, foreign-task,
and predecessor items; preserve approval after failed implementation; distinguish
pending/failed stopping from quiescence; and select a successor independently
of snapshot order. Existing approval and readiness regression suites preserve
the campaign, digest, PR, and freshness bindings.

Historical phase wording precedes the approval-history qualification, so a
stopped specification with unavailable history never reads as current or
pending. A focused regression covers that combination.

Cancellation suppresses a stale handoff heading together with its status and
guidance, preserving the recorded round from either a loaded run or the
daemon's position. The daemon's lifecycle test establishes that a newer
episode can remain active after an older confirmation; focused checks preserve
its current specification/final-review guidance and qualify the confirmation
when that newer episode finishes.

Revisit when the daemon supplies task-level next-action summaries or changes
the lifecycle, cancellation, approval, or readiness contracts.
