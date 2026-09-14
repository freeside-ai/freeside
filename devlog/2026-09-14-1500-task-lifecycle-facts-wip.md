# Task lifecycle facts and WIP membership from recorded facts

Work unit: #1318 (record task lifecycle facts; count WIP admission from them,
not from the newest run). This note records the design decisions that go beyond
or diverge from the issue's Decisions section (D1–D7) and the finding that
descoped D5. Settled and implemented; freezes when the PR merges.
Follow-up: #1344 (D5's auto-abandonment).

## A lifecycle fact carries its producing run's campaign (extends D1)

Chose to store `campaign_id` on every fact, beyond D1's listed columns, so
`domain.TaskWIP(task)` stays a pure function of the loaded `Task`. D4 decides a
completion's currency by campaign ("a completion whose binding belongs to an
older campaign is history"), but the `Task` struct has no run→campaign map, so
currency is undecidable from the log alone unless the fact records it. The
completed fact's binding (`workunit-<impl run>`) is redundant with its run for a
completion, but the campaign is the field the derivation actually needs.
Currency is compared at the **campaign** level, not the attempt level: a
completion releases the slot iff its campaign equals the current work episode's
campaign. The acceptance scopes the "late merge against a prior binding does not
release" case to a specification revision (a new campaign); within one campaign,
a reattempt happens only after a terminal state, so a late completion of a
superseded prior attempt in the same campaign is not a stated or realistic case,
and campaign-level currency satisfies every acceptance bullet.

**Correction (issue #1318 G2): the current campaign is the newest recorded
`started` fact's campaign, not `last(CampaignIDs)`.** The first pass compared
against `last(CampaignIDs)`, which was wrong: intake `admit` persists a new
reserved specification run and appends its campaign to `CampaignIDs`
(`recordTaskRun`) *before* the proposal starts, so a reserved-but-unstarted
proposal shifted currency to a campaign that has no start. A completion of the
actually-running campaign was then treated as history and never released the
slot, contradicting §5.11 ("unstarted or snoozed proposals do not count, even
if intake reserved a run identity"). `domain.TaskWIP` now derives currency from
the newest `started` fact's campaign (the episode actually being worked), and
the D7 migration mirrors it (`currentCampaign` is the newest **launched** run's
campaign, not the last of `CampaignIDs`). The re-gate also corroborates every
started/completed fact's campaign against its run's persisted campaign,
including nullability (issue #1318 H3): clearing a campaign to NULL must not
bypass the check, since two nil campaigns would otherwise compare equal and
release the slot.

## `started` is recorded at run submission, not at a decision site (diverges from the plan's site list)

The plan's implementation-aid comment listed five per-decision start sites, but
those references were approximate: the proposal-start decision has no run yet
(the run is minted later), and `production_publication.go:266` is a completion
commit, not a start. The reachable, uniform "admitted start" event is a run's
**submission milestone** (`MilestoneRunSubmitted`), recorded at
`specification.go` (spec run) and `production_workflow.go` (implementation run).
A reserved-but-unstarted proposal run has no submission milestone, so it records
no start and holds no slot — exactly D2's rule. `store.RecordTaskStart` is
guarded by `!TaskWIP` and keyed on the run, so a spec run and its implementation
run in one campaign share one start, a retry on a still-WIP task records
nothing, and a reattempt or revision (task no longer WIP) records a fresh start.
Intake auto-start additionally records the start inside the WIP-cap write, so a
concurrent admission sees the slot taken (the cap counts WIP tasks excluding the
task being admitted); the later submission milestone is an idempotent no-op.
Recording the start inside the cap write keeps the count race-free, but the
start decision (`StartRunProposalUnattended`) runs after that write and can
report `started=false` when the card was declined or superseded in between. The
start is then held for a run that never launches, so intake auto-start releases
it with the idempotent `AbandonTask` sink unless the proposal was in fact
decided start (the reconciler's already-decided path relaunches that one, so its
slot is legitimately held). Recording the start only after the decision was
rejected because it reopens the concurrent-cap race the atomic write closes.

## The D7 backfill classifies by the current campaign's terminal state (issue #1318 G1)

A specification run never records a `terminal_recorded` milestone; only a
production (implementation) run does (`production_workflow.go`
`recordProductionTerminalWithCompletion`). So "any launched run that is not
concluded" is the wrong active test: a completed task's spec run is submitted
but never terminal, which would wrongly mark the finished task as active/WIP.
The backfill instead classifies by the current work episode's terminal state: a
current-campaign completion makes the task completed (not WIP); else, if the
**newest launched run** concluded, it is abandoned (the implementation finished
with no completion); else the episode is still in progress (started only).
Finality is the newest launched run's state, not any run sharing the campaign
(issue #1318 H2): a retry after an earlier same-campaign terminal is still live,
and taking any-run-in-campaign finality would wrongly abandon it. The first pass
used the non-finality test and passed only because its fixtures used single
terminal runs; the regression fixtures now use a realistic two-run
(never-terminal spec + terminal impl) shape plus a live-retry case.

## Facts live in a side table, stripped from the task body

The `task_lifecycle_facts` table is the single source of truth (D1); the task
JSON body never carries facts (`updateTask` strips them), and `GetTaskSnapshot`
loads them and re-validates the assembled task at the reconstruction boundary.
`GetTask` is called inside migration 0070's backfill, so the facts query falls
back to empty when the table does not yet exist (a pre-0072 migration window),
never masking a real query failure.

Structural re-validation alone is not enough at this boundary: a
current-campaign completion or an abandonment releases a WIP admission slot, so
a decoded fact is a trust bit and the trust-boundary rule (entities.go,
artifact.go) requires re-gating it against current authority, failing closed.
`regateTaskLifecycleFacts` therefore corroborates every loaded fact: its run
must be one of the task's runs (`task_runs`), and a completion's binding must
name a recorded `work_unit_completions` row. Abandonments have no corroborating
table, so only run membership is checked. Legitimate facts always corroborate
(`RecordTaskStart`/`RecordTaskCompletion` write only for the task's own runs,
and a completion row is recorded with or before its fact), so the re-gate
rejects only a tampered or corrupt log, never valid data.

## D5 descoped: its trigger is unbuilt (owner decision)

D5 (a stop on a no-artifact specification run records abandonment, editing
§5.11) was confirmed at fiat, then found **not implementable at `1ccb0b91`**:
no reconciler consumes a `Stop` on a specification run, so there is no reachable
trigger, and the only no-artifact spec-run branch also covers retryable failures
that must keep their slot. This is revision 58's sketch-round stop behavior,
which is not yet built. The owner chose to descope D5: §5.11 is unchanged, a
discarded sketch holds its slot until `freesided abandon` (D6) runs, and #1344
tracks wiring D5's auto-abandonment once the stop path exists. The
`store.AbandonTask` sink is implemented and ready for it.

## The WIP-cap policy key name is retained

`budgets.run_wip_cap` keeps its name though the cap now bounds WIP **tasks**,
not runs (renaming a resolved-policy key is a #1318 non-goal). Its documentation
now describes the task-level meaning.

Revisit when: the operator `Stop`-on-a-specification-run path is built (#1344),
at which point D5's auto-abandonment and the §5.11 edit land against the ready
`AbandonTask` sink.
