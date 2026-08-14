# Classify an Empty Observation History as Unobserved

Work unit: #733 (kind:contract). Extends the run-observation projection
(#657, [2026-08-12-0710-run-observation-projection.md]) with one new
`RunOutcome` value. This note records the contract decisions the widening
turns on.

## Decision

Chose to add a distinct **`unobserved`** run outcome for a run with an empty
observation history, over leaving such a run classified as `pending`. Migration
0024 deliberately backfills no milestones onto pre-0024 runs, so after an
upgrade every legacy run, including completed, failed, and lost ones, has an
empty history and would otherwise render as "In progress" indefinitely.
`ConcludeRun` now classifies a run with no observation history as
`unobserved` before any timeline analysis. The history is milestones and
invocation observations together: a legacy run still executing across the
upgrade gains liveness observations, not milestones (`observeInvocation`
records invocation observations, never milestones), so a nonterminal
invocation observation flips it to `pending`, as does a first milestone.
Only a run with neither is `unobserved`. This preserves the 0024
no-backfill rule by construction.

Chose the name **`unobserved`** over `unknown` (agent/owner default, vetoable
at pickup, unvetoed). It states what the daemon has (no observation rows)
rather than an epistemic shrug, matching 0024's rationale that inventing a
timeline "would present reconstruction as observation".

Chose to make `unobserved` **non-final with pending-shaped detail** (no reason,
no terminal, `Final: false`), over a terminal shape. A pre-0024 run still in
flight across the upgrade must be able to move to `pending`; because it gains
liveness observations rather than milestones, a nonterminal invocation
observation is what moves it (a first milestone does too). A terminal shape
would wrongly freeze it.

Chose **no migration and no store write-path change**. `RunOutcome` is
derivation-only: milestone rows persist `ExecutionOutcomeStatus`, never
`RunOutcome`, and the projection computes the outcome at read time. The signet
projection needed no production change either: `runSnapshot` already flows
`ConcludeRun(observation).Outcome` onto the wire, and `LatestMilestone` is
already nil for an empty history.

## Rejected Alternatives

- **Reuse `pending`:** rejected because it is the bug: a legacy terminal run
  reads as live forever.
- **Backfill milestones onto pre-0024 runs:** rejected; it overturns the 0024
  no-backfill rule (a non-goal on #733) and would present reconstruction as
  observation.
- **A terminal `unobserved` shape:** rejected; it would misclassify a legacy
  run that is genuinely still in flight after upgrade.

## Rollout Implication

Widening the enum is a wire-compatibility break for an installed app: the
generated Swift enum fails decoding on an unknown value, so an old app binary
against the new daemon breaks once any run reports `unobserved`. The app must
be rebuilt and reinstalled with the daemon upgrade, the standing rule for
sync-contract changes.

## Revisit When

The degraded candidate-readiness vocabulary (#692) lands, since it may
introduce further derived-outcome states that interact with how the client
groups non-terminal outcomes.
