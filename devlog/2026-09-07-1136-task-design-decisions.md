# Task Scope, Identity, Naming, and Titles

The owner chose opaque task IDs, transactional intake keys, a stored name with
a limited rename lifecycle, and separate task-name and PR-title fields in
[#1206](https://github.com/freeside-ai/freeside/issues/1206). Plan revision 48
records those choices. They settle the naming decision deliberately deferred
in [the task-vocabulary note](2026-09-07-0921-task-vocabulary.md); that merged
note remains unchanged.

## Task Scope

The accepted audit in [#1211](https://github.com/freeside-ai/freeside/issues/1211)
applies revision 47's task definition to work that persists across runs. Chose
task scope for final review and its base-advance watch because returning work
to an agent starts a new run without starting a new undertaking. Keeping them
run-scoped would split the review history and its watch at each retry.
Returning work ends the current final-review item, so base-advance checks stay
inactive until a later item has its own bound PR head. That item arms a watch
with its own exact bindings; task scope does not require reusing the previous
schedule or carrying its run bindings forward.

Chose task lineage for follow-up filing and completion because scope,
dependencies, and the completion criterion describe the requested outcome.
The per-run work-unit declaration carries those bindings; a related or partial
merge cannot complete the task. Only the current, non-superseded completion
binding can complete it. Retaining an earlier binding's authority after a
revision would let a late merge finish work that no longer matches the task's
accepted outcome. Earlier bindings and their merges stay immutable history.
Task proposals and timelines name that same undertaking, and WIP counts it
once rather than accumulating its runs.

Rejected moving all run state onto the task. Evidence and approvals must
still identify the exact run, artifact digest, and PR head; an earlier
attempt's proof cannot authorize a later attempt. The three clocks stay
run-level, the execution cap keeps counting executions, and the `blocked` item
keeps naming the current run's wait. This boundary extends the task layer
without changing the durability model that required runs to remain separate.

## WIP Membership

The owner chose to count started tasks until completion or explicit
abandonment, including waits, retryable failures, and final review. Unstarted
or snoozed proposals do not count, even when intake reserved a run identity.
Retries and return-to-agent keep one slot. Dismissing a card or stopping an
attempt does not abandon the task; any later authorized restart of a completed
or abandoned task must pass admission again.

Membership uses the latest admitted start and only completion or abandonment
recorded after it. Earlier terminal facts stay history rather than releasing
a restarted task's slot. This ordering keeps replay from reviving an old
completion as the end of newly admitted work.

Chose this over preserving current admission counting, which includes
proposal reservations and derives counts from run conclusions, because WIP
should limit unfinished undertakings. A terminal attempt does not by itself
finish the task. Membership therefore needs recorded task start, completion,
and abandonment facts; the newest-run lifecycle described in #1203 does not
supply those facts. The downstream task contract must
carry these facts. This is a deliberate admission-policy change, separate
from the run-level compute clocks and concurrent-execution cap.

## Identity and Intake

Chose an opaque minted task ID over a content-addressed task ID because a task
persists as its specification and execution history change. Runs and campaigns
keep content-addressed identity because their IDs certify the bindings that
approvals rely on. A revised approved specification creates a new campaign
under the same task; an operational retry stays in the existing campaign.

Idempotency belongs to a separate project-scoped key: source digest for submit,
or repository ID and issue number for label intake. Chose one transactional
insert-or-fetch over check-then-insert because simultaneous intake must return
one task, and rollback before commit must leave neither a task nor its key.
Repeats fetch the committed ID; identical sources in different projects remain
different tasks. The key identifies intake, not an approval or execution.

## Stored Name

Chose an operator heading first, otherwise an advisory namer, over waiting for
an approved specification to name every task. Tasks need a recognizable name
at intake, including label intake and workflows without specification. An
unavailable namer leaves the identifier fallback and does not stop execution.
The name is stored rather than regenerated on read.

The specifier may refine an agent-provided name once at specification
submission. It never overwrites an operator name. If no name was produced,
the approved specification may supply the first name at approval. Approval
freezes the name; later specification revisions and retries leave it alone.
Only explicit operator rename changes it after approval. Chose this bounded
lifecycle over repeated automatic naming because a moving label makes the
same task harder to recognize. Operator precedence preserves the naming
contracts in #1203 and #1208.

Retained the approval-time first name for an identifier-only task because
#1203 explicitly requires that fallback at approval. #1208's submission hook
refines an existing agent name; it does not supply this fallback. Moving the
first name to submission would change those contracts. The approval-time
name is frozen immediately, so it receives no later automatic refinement.

Agent names remain producer-labeled claims under the advisory-only judgment
contract. A name grants no execution, approval, or publication authority and
cannot replace an artifact digest or exact run binding.

## Separate Titles

Chose separate task-name and PR-title fields with independent length bounds
over one shared field because the task outlives any one run or PR. A PR title
describes the proposed change; changing it must not rename or identify the
task. The downstream field contracts own their numeric bounds. The namer's
output bound and the existing commit-subject limit do not define a PR-title
limit.

## Revisit When

- One task needs independently completable outcomes or concurrent final-review
  watches with different completion criteria.
- Started tasks awaiting external decisions exhaust WIP often enough to
  justify a separate waiting-work policy; do not silently free their slots.
- Intake needs more than one task for the same project-scoped source key.
- Real use shows that the one-time specification refinement cannot produce a
  stable, useful name without overwriting the operator's choice.
- A task-level compute budget is needed. The three existing clocks remain
  run-level; changing WIP accounting to count tasks adds no new budget.
