# Main-Branch Race Incident Policy

The owner chose one exact `go test ./... -race` run after pushes to
`main`, with automatic incident reporting on failure, over a PR-gated
race matrix. The single runner preserves the detector's complete package
coverage and in-package parallelism, while moving its several-minute cost
off the pull-request critical path. This deliberately detects a regression
only after merge; ordinary Linux, macOS, and convergence checks remain the
pre-merge gates.

The rejected matrix split explicit heavy packages from their computed
complement and retained exact coverage, but its slowest shard still made the
PR gate materially slower than the existing checks. Serializing packages,
reducing detector coverage, or weakening the race run to advisory subsets
was also rejected.

The failure reporter is a separate `workflow_run` observer with job-local
`actions: read`, `contents: read`, and `issues: write` permission. It never
checks out, downloads, or executes repository content or upstream artifacts;
the tested daemon workflow retains only `contents: read`. This boundary was
chosen over workflow-wide write permission so tested repository code never
executes with issue-write authority. The observer queries the completed
upstream run's latest-attempt jobs after verifying that the run is a push from
this repository. A matching race job conclusion of `failure`, `timed_out`,
`startup_failure`, `stale`, or `action_required` creates or updates an
incident; `success`, `cancelled`, `skipped`, and `neutral` are the only silent
conclusions. Missing or duplicate job identities, unknown or null conclusions,
and malformed, failed, or incomplete API results are themselves reported as
operational incidents because they could hide a detector failure. Event and API
values cross into the shell only as quoted environment data, and the reporter
writes Markdown through runner-temporary files.

An open issue labeled `ci-race` is the deduplication unit. The first failure
creates one incident, later failures append evidence to the lowest-numbered
open incident, and multiple open incidents are handled deterministically by
warning and selecting that canonical issue. Concurrent reporters can both
observe no incident, so each creator re-lists after creation; a creator that
lost the race appends its evidence to the lower-numbered canonical issue,
marks only its newly created issue as a duplicate, and closes that duplicate.
Success never auto-closes an incident; triage owns classification and closure.
A later failure after closure creates a new incident. Reports intentionally
cover every observed race-job failure, including an actual race, setup or
infrastructure failure, and timeout, plus observer ambiguity, because a
reporter cannot safely classify incomplete jobs.

The SIGKILL fixture keeps its ordinary 5-second readiness deadline but uses
30 seconds in race builds. One cold hosted run showed that race-instrumented
child builds can exceed 5 seconds without a detector finding; the wider
test-only deadline stabilizes that infrastructure without changing production
code or package parallelism.

`workflow_run` observation preserves a completed failed race result even when
the daemon workflow is later cancelled by a newer main push. A notifier
workflow syntax or platform failure can still prevent the report; Actions
visibility is the residual detection channel for that failure class.

Revisit when the post-merge detection delay is no longer acceptable, the
slowest complete-coverage PR design fits the project's latency budget, GitHub
changes job-token or `workflow_run` isolation, notifier failures need an
independent alerting channel, or incident volume shows that label-based
deduplication needs a stronger lifecycle key.
