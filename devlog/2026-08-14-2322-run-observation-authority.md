# Let Durable Execution Authority Override Paced Observation Status

Work unit: #785. This note records the returned-object trust-boundary change
that keeps a live approved run visible while its invocation observation lags.

## Decision

Chose to derive each served invocation status from authenticated durable
execution authority, when present, over requiring equality with the last paced
driver observation. `ExecutionExport` remains canonical `completed` authority;
`ExecutionOutcome` and the authenticated terminal milestone remain canonical
failure, cancellation, and loss authority. The observation row still supplies
its daemon-clock `observed_at`; the projection clears `live` when authority is
terminal so the served object stays structurally honest.

This overturns #767's corpus classification of an export milestone paired with
a `running` observation as forge-only. The changed assumption is evidence from
the production store on 2026-08-15: ordinary writers durably record export,
outcome, or terminal facts while the paced observation row can legitimately
remain `running` or later read `gone`. Observation lag is cache staleness, not
an integrity contradiction. The existing first-order authentication checks
still bind every authority record to the returned run, stage, attempt,
admission, and dispatched intent before it can affect the served status.

Milestone instant semantics are unchanged. In particular, #394's export
milestone retains the fact's replay-pinned instant even when that instant
precedes a later-written start milestone; insertion order remains the served
timeline order.

## Rejected Alternative

Rejected updating `invocation_observations` in the export/outcome writer
transaction. That would claim the runtime reported an observation it did not,
would not heal existing frozen rows or the `gone`-versus-`failed` shape, and
would leave the equality exclusion armed for the next legitimate pacing lag.

## Refute-First Findings

- **Confirmed, authority cannot be forged by observation status.** The corpus
  now accepts `running` behind authenticated export and `gone` behind
  authenticated failed outcome/terminal, and asserts the served status comes
  from authority. Every remaining forge case still fails closed, including
  attempt membership, admission/run binding, export/outcome existence, and
  dispatch-intent authentication.
- **Confirmed, genuine damage still isolates.** The #767 listing tests and the
  #770 health-item tests now use an observation bound to an invocation the run
  does not own. `GetRun`/timeline retain the typed integrity failure;
  `Bootstrap`/`ListRuns` exclude only that run and mint the advisory item.
- **Confirmed, repair converges.** A health item previously minted solely for
  terminal-status lag resolves on the next listing pass while the unchanged
  lagging run is served.
- **Confirmed, the projection does not mutate store-returned data or return an
  impossible live terminal.** The invocation slice is cloned, `observed_at` is
  preserved, and the terminal overlay clears the stale `live` bit.
- **Confirmed, the operator path stays served.** The integration fixture runs
  elaboration through digest-bound spec approval, creates the implementation,
  records admission/start and a running observation, then records export. Both
  `ListRuns` and `Bootstrap` serve the implementation at each step, the
  timeline reports `completed`, and no run-projection health item is minted.

## Revisit When

Two independently authenticated durable terminal records can disagree for one
invocation. That would require an explicit precedence or fail-closed rule
rather than the current writer-ordered terminal map.
