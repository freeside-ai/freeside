# Verification Is Recorded As Run Milestones

Work unit: #3446. Scope: `daemon/`, `api/`, `app/`, `devlog/`. A contract
change (`domain.RunMilestoneKind`, the `run_milestones` table, the API
`RunMilestoneKind` enum) and Tier A work (the verification and re-entry
paths in the engine, and the publication reevaluation order). It revises
`devlog/2026-10-23-0856-verification-start-point.md` and extends
`devlog/2026-11-16-1845-run-clock-definition.md`.

## The Decision

Chose three new run milestones on the verification invocation,
`verification_started`, `verification_passed`, and `verification_failed`,
over reusing `invocation_started` and `terminal_recorded` for the verifier
and over a separate verification table.

- **Reusing the writer's kinds** would have read as a second writer pass:
  the app counts a `terminal_recorded` as an attempt's pass, and the clocks,
  the task rail, and `inspect task` all treat `invocation_started` as an
  agent invocation. Each reader would have needed a rule to tell the two
  apart, and the kinds would have changed meaning without changing name,
  which the contract rule forbids.
- **A separate table** would have needed its own projection into the task
  timeline, the run timeline, and the clocks, which already read
  `run_milestones` in record order. A milestone puts the verifier in that
  one ordered record for free.

Every verification path records them: a first cycle and a remediation round
(`runVerification`), a re-entry and a handoff check (`verifyReentry`), and a
reevaluation. The start is its own write just before the verifier runs; the
result is written in the same transaction as the verification checkpoint,
so a crash can't keep one without the other. A verification refused before
it runs (a refused rebuild, no host executor) records a failure with no
start. A verification that stops the run with no stored verdict (a verifier
killed at its memory limit, a tree it can't verify, a re-entry or handoff
stop) records `verification_failed` before the stop, so a stopped run
doesn't read as verifying while it waits on a person. A re-entry that reuses
its predecessor's verification (#3532) runs no verifier and records none;
the predecessor's verifier recorded its own.

## Verification Milestones Clear No Hold

Inserting a run milestone used to clear the run's standing hold, and the
2026-11-16 clock note relies on that ("inserting any milestone clears the
hold"). The verification kinds are the exception
(`RunMilestoneKind.ClearsHold`), found when a verification after an accepted
publication cleared the standing `publication_environment` hold
(`TestAcceptedPublicationKeepsOtherHoldCauses`). A verifier works on behalf
of the publication cycle that holds the run; its start or result says
nothing about whether that hold's cause went away. The writer's kinds still
clear holds as before.

## The Kind CHECK List Is Gone

Migration 0148 rebuilds `run_milestones` with `kind <> ''` in place of the
baseline's list of kinds, as 0144 did for `launch_reservations`
(`devlog/2026-11-07-1113-enum-columns-go-validation.md`). `RunMilestone`
validation refuses an unknown kind on write and on decode, so the next kind
adds no rebuild. The rebuild keeps every row and its id (record order is the
id) and the unique identity index that makes `AppendRunMilestone` first
observation wins.

## What It Changes Downstream

- **Clocks.** `verification_started` opens an active interval that the same
  invocation's result closes, else the next start of either kind or the
  run's end. Verification time is now working time, not Other, in
  `RunClocksOf`, work hours, and the app's strip, which shares one rule
  (`RunWorkSpan`) with the mock.
- **Task timeline.** The projection now carries the writer's
  `execution_export_recorded` (writer finished, import passed) and the three
  verification kinds. `inspect task` lists each run's verifications
  (`freeside-task-record-v20`).
- **Verification's start point.** The 2026-10-23 note drew the start under
  the first event that moved the task to Verification, because the daemon
  recorded no start. It now does: a run with `verification_started` starts
  Verification at that instant, and the derived start stays only for runs
  recorded before this change.
- **External review rounds.** Current Run shows the round an external
  review re-entry opened, with its step, and the run page reads `Review ·
  Round N` while a round requested after the newest verification result is
  open.

## Owner Decision

The plan proposed splitting the app half into its own issue. The owner
declined: one unit and one PR, as the issue was scoped.

## Refute-First Findings

One fresh-context reviewer read the change to prove it wrong.

- **Confirmed, fixed:** a verifier that stopped the run without a
  checkpoint (memory-limit kill on a first cycle or handoff, an unverifiable
  tree or other re-entry stop) left its start unpaired, so the clocks, the
  strip, Current Run, and `inspect task` showed it verifying until the
  blocked run moved again. Each stop path now records `verification_failed`
  (`recordVerificationStopped`); the memory-limit, handoff, and re-entry
  stop tests assert it.
- **Confirmed, fixed:** the run page's hold callout named the implementation
  pass's round while its header named the open review round. Both now read
  the same open round. The run list and task rows hold no timeline and keep
  the run's recorded phase.
- **Allowed:** a retryable verifier error leaves the start unpaired until
  the retry, and the retry converges on the first start, so the hold before
  it counts as working. See the revisit condition below.
- **Disproved by a check:** the verification kinds grant nothing (sync
  authentication treats them as projection only), clear no hold, and 0148
  keeps every row, its id, and the identity index; the migration and
  drop-declaration tests pass.

## Not Done Here

- Review invocations still count as Other: a reviewer records no start
  milestone. Follow-up: #3530.
- An import the daemon refuses has no milestone of its own; it reads as the
  writer's failed outcome. Follow-up: #3531.

Revisit when a retried verification's first start instant (first
observation wins) makes the clocks count a long gap before the retry as
working; the start would then need a per-attempt key.
