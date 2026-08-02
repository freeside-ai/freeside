# Active-Resource Reconciliation Owns Ready-Item Lifecycle (#463)

Issue #463 corrects the lifecycle seam chosen in
`2026-08-01-2308-capture-hooks.md`. That note attached merge capture to the
base-advance schedule because it was then the only periodic PR reader. The
plan's stronger boundary is controlling: PR state is an active-resource
cadence outside the durable schedule union, and a ready item stays open until
that cadence observes its exact PR merge or close.

## Decisions

- **A plain-ticker reconciler owns PR and issue observation.** It performs an
  immediate startup pass, then conditionally polls each open
  `ready_for_final_review` resource. Per-resource observation failures are
  reported and retried without blocking healthy siblings; an inability to
  enumerate or commit durable state remains fatal. Rejected: a fifth schedule
  kind, a global cursor, and continuing to use the base-advance watch as the
  production capture lifecycle.
- **Every ready item gets a daemon-internal PR binding.** The optional
  work-unit binding cannot identify undeclared runs, so publication now records
  a separate write-once `ReadyItemPRBinding` for every ready item before arming
  its schedules. The binding carries the producing and publication invocations
  plus the publication identity, then re-anchors every coordinate on write and
  read to the immutable admission, export, dispatched publication intent, and
  publication outcome as well as the ready item. Migration 0028 backfills only
  where those same records agree on every coordinate; incomplete history stays
  absent and fails closed. Rejected:
  parsing presentation text and widening the synced AttentionItem/API shape.
- **One observation commits lifecycle and world-model state atomically.** An
  exact closed PR (canonical repository id, PR number, admitted base, and
  published head all match) appends the material pull/issue facts. An unmerged
  close resolves the item without inventing completion. A merge with a bound
  issue stays active until GitHub's later issue-closing side effect yields the
  declared completion; that pass records completion, resolves the still-open
  item, and resolves all three publication schedules in one client-visible
  transaction. A
  concurrent return or dismissal wins its own terminal status while the
  observation may still commit facts and schedule convergence.
- **Returned objects are rechecked, not restamped.** The forge response must
  return the requested PR or issue number. Repository identity comes from the
  response, and issue observation retains the second PR read that detects a
  repository-name rebind during the pass. A closed response whose repository,
  base, or head differs from the durable binding records no lifecycle
  conclusion.

## Refute-First Verification Findings

- **Rejected by verification: a returned PR number can retarget the pass.** A
  response whose number differs from the requested binding is refused before
  any fact or lifecycle write.
- **Rejected by verification: a reused repository name can conclude the old
  item.** The observed numeric repository identity is recorded as observed but
  cannot satisfy the ready binding or completion evaluator.
- **Rejected by verification: migration ambiguity picks an arbitrary PR.** The
  dispatched run-bound intent selects one exact outcome, so unrelated outcomes
  cannot retarget the backfill; a missing intent leaves the binding absent.
- **Confirmed by automated review and fixed: PR merge can precede bound-issue
  closure.** A merged PR no longer concludes a bound-issue resource until the
  matching `closed_by_commit_sha` is observed; a regression test advances the
  two forge observations on separate passes and proves restart convergence.
- **Confirmed by automated review and fixed: partial binding re-anchoring left
  retargetable coordinates.** The binding now names its producing invocation
  and publication identity. Reconstruction independently re-derives repository
  name/id and base from the admission, head from the export, and PR coordinates
  from the strict publication outcome. Corruption tests retarget each coordinate
  together with its extracted column and prove the immutable sources refuse it.
- **Confirmed by automated review and fixed: shared publication facts can
  coalesce across work units.** Completion now re-derives from the pull and
  issue rows visible after material-change coalescing, so its timestamp is
  exactly reproducible by the read gate even when a later item observes the
  same deterministic PR. A two-run shared-PR regression pins this case.
- **Confirmed by automated review and fixed: optional issue context does not
  make PR-only completion issue-dependent.** Persisted issue lookup now keys on
  `CompletionBoundIssueClosedByMergedPR`, not merely a non-nil `BoundIssue`; a
  PR-only declaration carrying optional issue context completes without an
  issue observation.
- **Confirmed by automated review and fixed: historical completion remains a
  lifecycle authority.** On upgrade, an already-recorded completion that still
  passes its historical-fact reconstruction gate concludes the exact closed PR
  resource even if the bound issue has since reopened; the reconciler does not
  require or rewrite that completion.
- **Confirmed by automated review and fixed: outcome coordinates did not bind
  identity to the producing run.** The ready binding now names the publication
  invocation and authenticates its strict dispatched intent. The intent must
  bind that invocation, run, producing invocation, identity, repository, base,
  and head before the deterministic outcome can supply the PR. A cross-run
  substitution of another valid intent and outcome therefore fails closed. A
  corruption matrix also refutes every individual intent field, its kind,
  dispatch state, and unknown payload fields.
- **Confirmed and fixed during self-review: resolving only the synchronized
  schedule aggregate leaves internal clocks live.** Item conclusion now also
  removes each schedule timer and consumes pending occurrences in the same
  transaction, matching the durable scheduler's own terminal path.
- **Concurrency disposition:** a return or dismissal that commits before the
  pass is not polled and promptly settles its schedules; one that commits
  during observation keeps its terminal status while the already-observed
  facts and schedule settlement converge transactionally.

## Revisit When

- The §7 review stage adds a richer active PR/check/review projection: fold its
  conditional reads into this reconciler so each resource still has one
  poller.
- Remediation legitimately changes the published PR head: revise the
  write-once ready and work-unit binding contract together; do not weaken exact
  head matching in the observer.
