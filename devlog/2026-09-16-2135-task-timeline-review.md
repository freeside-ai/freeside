# Review History Inside Task Runs

Task history composes the existing run-timeline reads for #1372. A second
review cache or new task-timeline fields would duplicate facts and their
epoch rules. Each implementation or legacy run keeps its own rounds and evidence
binding. At the owner's request, Review appears above Implementation, below
the attempt title and run ID.

## Read Ownership And Freshness

Only implementation or absent-role run IDs in fetched task history enter the
read batch. Migration 0048 preserves legacy runs without campaign lineage;
their absent role does not prove that recorded reviews are absent. Include
those members without inventing a role label, and exclude specification runs.
The identity includes task and history revisions, membership, epoch, full
snapshot revision, and cache generation. Observed partial-read revisions do
not trigger another batch. The existing timeline loader owns response
binding, freshness, persistence, and generation rejection.

An in-flight operation must be joinable, not merely marked as started.
Independent refutation confirmed that a started-marker alone let a cancelled
view suppress the next view's read until manual retry. Callers now join the
operation, then retry an unsuccessful read. A unique operation token prevents
an old caller from clearing its replacement. Cancellation is checked before
adopting a timeline, including when the transport ignores cancellation.

Cached facts remain visible with explicit freshness context. Only a successful,
fresh read with no rounds shows "No review requested yet." A clean historical
round describes that round's bound head and base; the contract supplies no
current-candidate validity predicate. External or unknown sources retain
explicit labels. Reviewer output still passes the existing authenticated
run/round/invocation/head/source gate and remains private agent claims.

## Refute-First Findings

- Confirmed and fixed: cancelled navigation could suppress a replacement
  caller. The overlapping cancellation regression exercises reopening before
  the old response finishes.
- Confirmed and fixed: requiring an explicit implementation role hid recorded
  reviews on legacy runs. Mock task-to-review/evidence coverage admits absent
  roles, while a campaign specification fixture verifies zero review reads.
- Refuted by targeted checks: duplicate task membership can trigger duplicate
  run reads; concurrent callers can issue the same batch repeatedly; a new
  bootstrap or epoch can be suppressed by an older request.
- Refuted by targeted checks: two runs can share the wrong review/evidence;
  failed refresh or offline relaunch can erase same-epoch facts; a missing run
  timeline can be presented as an empty review history.

Revisit when task history carries review facts itself, review sources gain a
closed vocabulary, or the daemon adds a current-candidate validity contract.
