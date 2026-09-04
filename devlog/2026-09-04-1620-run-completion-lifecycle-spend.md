# Run Completion, Lifecycle, and Spend in the Run Contract

Chose to widen the run contract with three daemon-held facts in one
`kind:contract` unit (#1134): a `work_unit_completed` milestone and a
derived `completed` outcome, a derived `lifecycle` split with
`superseded_by`, and `billable_cost_so_far` on the run summary and
timeline. Each decision below rejects a reasonable alternative; the owner
took them at planning and may veto any of them.

## Decisions

- **The milestone stays invocation-only.** `work_unit_completed` carries
  the run's publication invocation, like `publication_ready`, and no new
  columns. The PR number, merge commit, and bound issue reach the wire as a
  `completion` object read from the store's re-gated
  `work_unit_completions` row. Rejected: copying those facts onto the
  milestone row, which needs new columns and a second agreement check
  against the same authority. One authority for the fact, no duplicated
  trust bits.
- **`completed` is a distinct terminal outcome**, outranking `published`
  and `blocked`, not "published plus a milestone". Every consumer switches
  on `RunOutcome`; a documented two-field rule would put derivation back
  on each client.
- **Supersession derives from `runs.parent_run_id`**, the lineage a retry
  already records, not from attention-card supersession (#1127). The
  successor is the earliest attempt naming the run as parent. The engine
  only retries a final run, so a run with a successor is finished.
- **`lifecycle` is its own derivation and leaves `RunConclusion.Final`
  unchanged.** `Final` is engine input (retry admission, resume, intake
  counts) meaning "the daemon will not change this outcome on its own"; a
  blocked run is final yet active, because the operator still holds a
  decision.
- **`unobserved` is finished; `blocked` without a successor is active.**
  #733 chose `unobserved` so a legacy run is never shown in flight, and a
  legacy run still running gains a liveness observation and becomes
  `pending`. A blocked run that was retried is superseded and finished.
- **Existing completion rows get their milestone through a one-time
  start-up reconcile.** The wave-7 run that prompted the issue shows
  `completed` after the upgrade. The pass appends only where the timeline
  lacks the milestone and bumps no sync revision otherwise. Veto by
  dropping the pass; then only completions recorded after the upgrade
  carry the milestone.
- **The billable-cost aggregate moved from the engine into the store read
  transaction.** The store opens one connection, so an aggregate a `Read`
  callback needs cannot go through a nested `ReadUsage`; the run
  projection and the attention cards now compute the figure through the
  same method. `UsageReadTx` stays the only raw-row surface.
- **The milestone must restate the completion record's instant.** Both
  recorders and the reconcile pass write the milestone at
  `completion.RecordedAt`, so the sync boundary can bind the two with an
  equality check at no cost. Rejected: a looser binding that only requires
  the row to exist, which would let a replayed milestone at a later
  instant pass as the merge time.

## Refute-First Findings

The sync boundary trusts a new milestone kind against a store row, so the
`docs/agent-workflow.md` refute pass ran over the new binding:

- **Forged milestone with no completion row.** Confirmed as a risk and
  closed: the read fails with `ErrParentKeyMismatch`, the listing reads
  exclude the run, and the exclusion is logged (corpus forge
  `missing_completion_record`, listing test).
- **Milestone without prior ready authority, or after a later block.**
  Closed: the authentication pass requires an authenticated
  `publication_ready` whose index is after the last `publication_blocked`;
  a block appended after the completion also trips the standing
  ready/blocked exclusivity check (corpus forges
  `missing_publication_ready`, `blocked_after_ready`).
- **Milestone on a foreign invocation.** Closed by the existing
  publication-invocation binding (corpus forge `foreign_invocation`).
- **Milestone instant disagreeing with the record.** Closed by the
  equality check above (corpus forge `instant_disagrees_with_record`).
- **Completion over a resolved block.** The conclusion for `completed`
  re-runs the same accepted-rerun chain authentication a `published`
  outcome over that history needs, and a completion recorded while a
  reevaluation is live fails closed. Disproved as a gap by construction;
  no fixture exercises the live-reevaluation path with a completion, and
  that is recorded here so a later reviewer does not re-raise it as
  untested without adding the fixture.
- **Cost figure as a trust vector.** Disproved: the aggregate is
  render-only, reads rows the store already isolates, and the sync
  boundary neither authenticates nor acts on it.
- **Reconcile pass mirroring an unsupported completion row.** Closed:
  `ListWorkUnitCompletions` re-gates every row and reports unsupported
  ids separately; the pass logs and skips them, never appends.

A fresh-context reviewer then tried to refute the diff. Its findings and
their outcomes:

- **Successor lookup trusted the extracted `parent_run_id` column
  alone.** Confirmed and fixed: the lookup decodes the candidate's
  canonical body and requires it to restate the id and the parent (the
  0048 rule for extracted lineage columns); a divergent column fails
  closed. The narrower decision that full production-lineage
  authentication stays with the run's own read did not survive the
  automated review round below.
- **Infrastructure errors on the completion read were mapped to the
  integrity sentinel**, so a transient store failure would have excluded
  a healthy completed run and minted a system-health item. Confirmed and
  fixed: only `ErrNotFound` and the store's now-exported
  `ErrCompletionUnsupported` / `ErrDeclarationUnsupported` verdicts map to
  `ErrParentKeyMismatch`; anything else propagates unwrapped. The
  transient path itself has no fixture; the mapping is by sentinel.
- **`ORDER BY attempt_number` let a NULL attempt number sort first.**
  Confirmed and fixed with `attempt_number IS NULL` leading the order and
  a seeded NULL child in the test.
- **Per-run `runs` scan in the listing reads.** Confirmed; 0066 adds the
  `runs (parent_run_id)` index rather than a one-sweep projection, which
  keeps the read shape per run like the rest of the projection.
- **Start-up reconcile made one damaged run fatal.** Confirmed and
  fixed: an unreadable declaration or timeline, and a timeline whose
  ready authority does not stand, are logged and skipped; only failing
  to list the rows is fatal.
- **An append the boundary refuses is irreversible.** Confirmed as the
  reason for a writer-side precondition: every writer (both recorders and
  the reconcile) goes through one helper that appends only when
  `domain.PublicationReadyStands` holds, the boundary's own rule. A
  completion row without its milestone keeps the run at its publication
  outcome, which is recoverable; a refused milestone is not.
- **The real-work supervisor has no `implementation:completed` case.**
  Confirmed; outside this unit's scope, filed as #1143.
- **`GetRunTimeline` serves completion facts without the conclusion's
  live-reevaluation check.** Declined: the timeline carries no outcome
  and never ran the conclusion; its facts are milestone-authenticated,
  and the window is the same one in which the summary reads fail closed.
- **Backfilled milestones clear a standing hold.** Declined: a hold on a
  run whose work unit is done is stale by construction, and forward
  progress clearing the hold is the standing milestone rule.

## Automated Review Findings

The automated reviewer refuted two of the fixes above. Both held on their
merits and are closed:

- **The direct-store conclusion surface skipped the completion binding.**
  `freesided follow` and `follow -snapshot` reach
  `signet.AuthenticatedRunConclusion` through `observedb` without the sync
  boundary's observation pass, so a completion milestone the store's
  re-gated record no longer supports read there as a final `completed`
  outcome while every sync read over the same rows failed closed. The
  record is re-gated on each read, so the divergence needs no forgery.
  Confirmed and fixed: the conclusion itself re-binds every completion
  milestone, making the binding a property of the outcome rather than of
  one caller.
- **The successor lookup skipped the run reconstruction gate.** The body
  cross-check refuses a divergent lineage column but not a row whose
  production-attempt authority is forged or damaged, and nothing else on
  the parent's read authenticates the child, so a planted row could make
  an active run report `finished` with a false `superseded_by`. Confirmed
  and fixed, overturning the narrower decision above: `RunSuccessor`
  reconstructs the candidate through `scanRunSnapshot`, the gate every
  other run read already runs, and a candidate that fails it fails the
  read closed. The extra cost is one chain per superseded run on a path
  that already pays that gate for every run it returns.

Declined in the same round: accepting `implementation:completed` in
`scripts/run-real-work-supervision.sh` as part of this unit. The script is
outside the declared scope and the state is unreachable there today, so the
work stays #1143.

A second automated round found three more, all confirmed and fixed:

- **The completion binding still omitted the invocation authority.** The
  conclusion re-bound the completion record but not the milestone's
  publication invocation, which the sync pass checks separately, so a
  milestone riding another run's publication invocation still concluded
  `completed` on the direct-store path. Fixed structurally rather than by
  adding one more check: both authorities now live in one
  `authenticatedCompletionMilestone`, which the observation pass and the
  conclusion both call, so the two surfaces cannot drift again.
- **The observation-only fallback had no gate at all.** `run_milestones`
  carries no run foreign key, so a timeline whose `runs` row is absent
  takes `ObserveConclusion`'s legacy branch, where `domain.ConcludeRun`
  alone could now return final `completed`. The branch predates the
  completed outcome and its comment reasoned only about reevaluation
  authority. It now fails closed on a completion, because a milestone with
  no run has nothing to authenticate it. This widened the unit's declared
  paths by one file, recorded in the PR body.
- **One damaged completion could keep the daemon from starting.** The
  start-up reconcile runs `ListWorkUnitCompletions` before the daemon is
  built, and the list isolated only `ErrNotFound`: a declaration the
  re-gate refused (`ErrDeclarationUnsupported`) or a body the store could
  not reconstruct aborted the whole list and the start. The list now
  isolates every fail-closed verdict about a single row and still
  propagates infrastructure failures, and a damaged row is reported by the
  unit id read from its key column. This is the same
  verdict-versus-infrastructure distinction the earlier finding drew for
  the sync-side completion read; the class is now closed on both sides.

A third round found the last member of that same class: the start-up
reconcile's own per-row reads swallowed every error, so a cancelled context
or a database failure while reading one declaration or timeline read as
"this run has no completion" and left a healthy run at its publication
outcome until the next restart. Fixed by exporting the classification the
list already used, `store.IsRowVerdict`, and applying it at both per-row
reads: a verdict is logged and skipped, anything else is fatal. Exporting it
is deliberate, because the isolation has to be applied by whoever walks the
rows, and a second hand-written list of sentinels would drift from the
first. The per-row infrastructure path has no fixture; the split is pinned
by sentinel in `TestIsRowVerdict`.

A fourth round found the last hand-written sentinel list: the sync
boundary's completion binding enumerated two sentinels, so a completion row
the store could not reconstruct escaped the per-run integrity sentinel and
failed the whole `/sync/bootstrap` or `/runs` request instead of excluding
that one run. Fixed by routing that branch through `store.IsRowVerdict` too,
which leaves no second list to drift. This is the same class as the two
findings above, in its third and final location; the class is closed because
one predicate now answers it everywhere, not because a round budget ran out.

A fifth round found a different class, and a real one: the completion read
never bound the work-unit PR binding to the run's own ready resource. The
store re-derives a completion from the declaration, the binding, and the
merge facts, which ties those rows to each other but not to the pull request
the run published, so a set forged consistently around another PR would have
reported a run published as PR A completed by PR B's merge commit. The
capture pass already makes exactly this comparison before it observes
anything through the binding, but a write-time check is not read-time
evidence, which is the reason the store re-gates at all. Fixed by re-running
the same restatement (repo, repository id, PR number, base, head, and the
ready binding's run) inside `authenticatedWorkUnitCompletion`; corpus forge
`binding_disagrees_with_ready` authenticates on the pre-fix code.

Convergence call at that point: continue. Findings per round ran 3, 4, 1, 1,
1; every accepted one was a distinct defect with a failing pre-fix test, and
the verdict-classification class closed for good once a single predicate
answered it in all three places. This last finding is a new class and a
false-fact defect, not more of the same hardening, which is why it earned a
round rather than a note.

Deferred from that round: `usage_observations` are appended through
`WriteInternal`, which bumps no revision, while this unit makes the derived
spend client-visible, so a cached figure can go stale under an unchanged
revision and `entity_version`. Real, but the fix is a revision-semantics
decision with three genuinely different answers (bump per usage append,
bump a narrower per-run version, or state a staleness bound), so it is its
own contract unit: #1145.

## Rejected Options

- **Deriving `completed` at the client from `published` plus the
  completion object.** Rejected for the derivation reason above.
- **Finishing a bound specification run when its implementation run
  finishes.** Out of scope: under this contract a specification run stays
  active while pending, per the owner's framing that "awaiting approval"
  and "bound" are active.
- **A separate milestone for the retry instead of a successor lookup.**
  Rejected: the lineage already exists on the run row; a second record
  would need its own agreement check.

## Revisit When

- #1129 lands and the operator still sees bound specification rows under
  Active; then the specification-run lifecycle needs its own rule.
- A specification reattempt is minted without `parent_run_id`; the failed
  specification row would then stay active.
- Per-campaign spend across attempts is wanted; the summary is per run by
  design and a campaign total needs its own projection.
- `scripts/run-real-work-supervision.sh` observes an
  `implementation:completed` state; the supervisor stops at `published`
  today and treats `completed` as unsupported, which only matters if
  supervision outlives the merge. Follow-up: #1143.
