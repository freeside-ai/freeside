# Completion Recording Survives Ready-Item Conclusion (#621)

Run 482 falsified the premise behind #463: the active-resource lifecycle does
not always observe a bound merge before another observer concludes the ready
item. In particular, the base-advance watch can supersede the item first, and
the former open-item-only reconciler then made the work-unit completion
permanently unobservable.

## Decisions

- **Base-advance capture returns to production as an atomic fast path.** The
  existing `mergeCapture` observes the bound pull and issue and composes its
  fact and completion writes with base-advance supersession. The fake lane
  remains unwired because its base is static and its pulls never merge.
- **Completion observation is independent of attention-item status.** The
  active-resource reconciler sweeps concluded ready items that still have a
  declared and PR-bound unit without a durable completion. It reuses the open
  path's identity checks, fact construction, persisted-fact reconstruction,
  and write-once gate, but performs no readiness, native-review, or item
  lifecycle mutation.
- **Concluded incomplete units remain observable.** Missing, open,
  closed-unmerged, merged-but-incomplete, and failed observations retry at the
  existing active-resource cadence. Rejected after refute-first review: treating
  `closed && !merged` as permanently terminal. GitHub permits reopening and
  later merging the same pull, so that shortcut would recreate the completion
  foreclosure this unit exists to remove. Unchanged observations coalesce and
  do not churn the sync revision.
- **Base invalidation never waits on GitHub completion capture.** A transient
  pull or issue observation still permits supersession to commit. The
  status-independent sweep supplies the recovery path on a later healthy pass.
  Rejected: holding base-freshness correctness hostage to the availability of
  a separate resource observation.

This general recovery path is also the run-482 repair: after deployment, its
superseded ready item remains eligible and can record the already-merged pull,
closed issue, and completion without state surgery or a run-specific case.

## Refute-First Verification Findings

- **Confirmed and fixed: the dormant scheduler capture trusted returned
  coordinates.** Before production wiring, it stamped the requested PR and
  issue numbers without validating the response and permitted foreign
  repository facts. The production path now requires positive, exact returned
  PR and issue coordinates, including the post-issue identity recheck.
- **Confirmed and fixed: the work-unit binding lacked an independent PR
  anchor.** Capture now reconstructs the ready-resource binding and requires
  every repository, PR, base, and head coordinate to agree before observing.
- **Confirmed and fixed: closed-unmerged was not a durable negative fact.** A
  reopen-and-merge regression proves a concluded item remains recoverable.
- **Confirmed and fixed: coalesced facts could strand a shared-PR
  completion.** Capture now re-evaluates completion from the persisted rows
  after appends, so a second unit records the earlier coalesced fact timestamp
  and remains reconstructable.

## Revisit When

- A durable forge event stream can replace polling of unchanged concluded
  resources without losing reopen-and-merge recovery.
- Ready-resource observation moves to a durable per-resource scheduler; retain
  status-independent eligibility and the same write-once reconstruction gate.
