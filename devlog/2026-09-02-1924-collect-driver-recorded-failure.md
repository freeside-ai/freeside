# Collect a driver-recorded failed outcome (#1084)

`acceptProductionAttempt` skipped collection whenever a non-blocked
`ExecutionOutcome` record existed for the attempt. The real stage driver
records the outcome before the engine's next pass, so every failed, canceled,
or lost implementation attempt hit that early return and never raised its
`execution_failure` item. This narrows the skip to the one engine path the
return was added for.

## Decisions

- **Narrowed the skip to the delivery refusal's row pair, rather than removing
  it or documenting it as intended.** The early return (`6f842565`, #842)
  exists so a pre-start delivery refusal stays inert: `recordProductionDelivery
  Refusal` writes a failed outcome record and the `execution-failure-<invocation>`
  attention item, marks the dispatch marker dispatched, and never writes a
  `production_stage_terminal` inbox row. Collecting that attempt would call the
  driver on an invocation it never saw and fail the pass on a dispatched
  marker. The new rule skips only when there is no terminal row **and** both the
  outcome record and the execution-failure item exist, which is exactly what
  the refusal leaves behind. A driver-recorded failed/canceled/lost outcome has
  no item yet, so the engine collects it and `executionFailureFacts` converges
  on the stored record; a blocked outcome carries an `agent_question` item, not
  an execution-failure one, so it is collected too. The prior blocked-status
  exemption is dropped because the item-presence test already covers it.
- **The terminal inbox row is read first, before the outcome/item pair.** Once
  collection records a terminal row, every later pass re-authenticates it
  against the driver through the existing `recorded` path. The old code's
  outcome-first shortcut skipped that re-authentication for a collected failed
  terminal; reading the row first keeps a collected terminal on the same
  re-auth path as every other status, so the skip is reserved for the refusal
  alone.
- **Added a scripted `OutcomeCancel` to the fake stage driver, not a reuse of
  `Cancel()`.** The canceled convergence case needs the driver to end a run
  canceled the way the daemon's own shutdown does, as a scripted terminal the
  engine collects during reconcile. `Cancel()` is a caller-invoked method, not
  a scripted outcome, so it cannot drive the reconcile path the other cases
  use. The shared `fake.Outcome` type is also the ReviewSource's; a review
  never ends on a scripted cancellation, so both ReviewSource dispatch switches
  reject `OutcomeCancel` as a fixture defect, exactly as they reject
  `OutcomeBlocked`.

## Verification Findings

Refute-first pass over the store-row trust boundary this change alters (which
stored rows may suppress an attempt's collection):

- **The skip assumes the delivery refusal is the only path that records an
  outcome record and its item without a terminal row.** At `1951429e` that
  holds: the other engine outcome writers run inside terminal collection (which
  writes the row), and the driver writes the rest. If a new pre-start refusal
  or hold records that pair without a terminal row, the skip would extend to it;
  if it records an outcome without the item, the engine collects and fails on
  `ErrUnknownInvocation`. `TestProductionDeliveryRefusalReplayStaysInert` pins
  the refusal's inertness against a driver that knows nothing about the
  invocation, so a regression that starts collecting it fails loudly.
- **A corrupted or fabricated terminal row still cannot suppress collection.**
  The row-first branch decodes through `decodeProductionTerminal`, whose
  reconstruction gates are unchanged; only a row this lane could itself have
  written survives, and a decode failure fails the pass rather than skipping.
- **Status mismatch fails closed unchanged.** A pre-recorded outcome whose
  status disagrees with the collected terminal still returns
  `ErrParentKeyMismatch` from `executionFailureFacts` and raises no item
  (`TestProductionOutcomeStatusMismatchFailsClosed`).

## Revisit When

- A new pre-start refusal or hold records an `ExecutionOutcome` without a
  `production_stage_terminal` row: re-check whether it should share the refusal
  skip or be collected. The refusal test is the guard.
- The fake's `OutcomeCancel` gains a real ReviewSource meaning (a canceled
  review): the two ReviewSource switches then need distinct handling.
