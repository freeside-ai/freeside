# Name the Refused Agent and Check on an Admission Hold (#2211)

Work unit: #2211. Scope: `daemon/` (domain, engine, store, signet,
migrations, the task record's version), `api/openapi.yaml`,
`app/Sources/FreesideAPI`, and `devlog/`. It changes the hold contract
(domain, schema, migration, API), so the note is mandatory.

## Decisions

**Name the cause with a closed `AdmissionCheck` enum, over carrying the
refusal's error text.** Run observations ban free text, and an error
string can echo writer or forge content. The detail holds only a check
code, an agent name from the admitted agent tree (operator-authored), a
stage name, and a launch digest. One member per distinct cause: the
thirteen sentinels in `dispatchHoldReason`'s admission arm, the eleven
agent-admission refusal causes, and `agent_admission` for an
`ErrAgentNotAdmissible` that carries no typed refusal (the review
admission path, which this unit leaves alone).

**Keep the hold span when only the detail changes, over resetting it.**
The span still keys on reason and invocation, so the held-work notice's
ID stays stable across a refusal that moves from one check to another.
The pace state folds in the check, agent, and launch digest, so the new
detail is written at once rather than at the next pace tick. The notice's
text is fixed when it is raised, so its sentence says what refused "when
this notice was raised" rather than claiming the current cause; the hold
row carries that. The fresh-context review found the undated wording
could name a check that no longer refused.

**Fail the store read closed on detail columns the contract can't
express, over returning a partial detail.** An unknown check, an agent or
launch without a check, or a stage without a digest all refuse the read.
A row with no detail columns (a hold recorded before the migration) reads
back with a nil detail, which the schema allows.

**Match the completion-fraction guard against whole key words, over
renaming the contract's `detail` field.** `TestObservationModelHasNoCompletionFraction`
substring-matched `eta` across the JSON, so `detail` tripped it. The
guard now splits each key on `_` and checks word prefixes, and a
self-check keeps the banned shapes failing. This is its own commit.

**Bump the task record to `freeside-task-record-v5`.** The record embeds
the run hold twice (timeline and run observation), and its rule says any
shape change moves the version.

**Number the migration `0103`.** #2243 merged `0102` first, and the store
refuses gaps, so the plan's renumbering condition applied.

## Departures from the Plan

- `agent_resolution` names no agent. The plan allowed naming one when a
  task line named it; the read failure happens before resolution returns
  an agent, so the detail would need a second source.
- The credential-expiry and snapshot refusal sites have no direct test:
  the test fixtures can't reach them (the fixture's auth method has no
  observable expiry). Each passes its check through the same `refuse`
  helper the tested sites use.

Revisit when the notice should track a changed check at once; that means
resetting the span on a detail change, which also changes the notice ID.
