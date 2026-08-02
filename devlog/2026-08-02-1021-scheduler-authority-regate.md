# Scheduler Authority Re-Gate and One-Shot Closure (#461, #462)

Work unit: issues #461 and #462 as one `kind:contract` remediation. This note
records the safety-contract decisions that close the two gaps found in the
Wave 3 scheduler audit.

## Decisions

- **Workload schedules carry an independent exact-run authority binding.** The
  three publication kinds persist `run_id` and `policy_digest`, fixed across
  every generation and extracted into cross-checked store columns. At each
  fire the scheduler requires the item's exact subject, the run, and the
  authenticated resolved policy to match that schedule binding before any
  terminal outcome, occurrence, or event construction. Missing,
  malformed, retargeted, cross-run, cross-project, or digest-mismatched state
  fails loud and leaves the fire unconsumed, even when the item is already
  terminal and would otherwise resolve the schedule. The migration snapshots
  existing workload rows from their item/run chain once; the OpenAPI schema
  and Swift generator input expose the new nullable-by-kind fields. Rejected after
  refutation: deriving authority only from the live item. Coherent corruption
  could retarget both item run fields to another valid same-project run and
  pass that self-referential check.
- **Non-run schedule kinds prove their separate authority class in the same
  closed dispatch.** `installation_poll` continues to use the pending
  authority document's registration, active epoch, and durable intent revision;
  doctor and janitor continue to derive only from daemon-owned trusted
  configuration. Rejected: adding an optional registration callback for policy
  checks, because optional composition wiring could recreate the omission this
  remediation closes.
- **Legacy fake-publication runs converge through one exact migration gate.**
  Pre-upgrade v1 tasks stored the trust-profile digest directly as the run's
  policy digest and had no resolved-policy row. Replay recognizes only that
  exact run/task representation, then atomically changes only the digest and
  inserts the authenticated one-key policy; any other run difference or an
  existing or unrelated policy fails closed. Both direct replay and bulk
  recovery converge the binding, including already-dispatched tasks whose
  ready-item watches remain live. Rejected: accepting the legacy representation
  indefinitely, because those watches would still lack the policy they must
  authenticate at fire time.
- **One-shot deadlines reject expiry as both shape and outcome.** The two
  one-shot kinds reject `expires_at`, and independently reject terminal status
  `expired`. Constructors, direct validation, transitions, and store
  reconstruction therefore admit only `fired` or `resolved` terminal states.
  Installation-poll expiry and recurring-watch expiry remain unchanged.

## Verification Findings

- The first refute-first pass confirmed a P1 in the initial no-shape-change
  design: the item alone was not an independent authority anchor. Persisting
  the binding therefore requires migration 0027, the OpenAPI contract, and the
  app's generated-client input to move with the daemon domain.
- The final refute-first pass confirmed a second ordering P1: dead-subject
  resolution ran before policy authentication and could consume a fire whose
  item was terminal while its policy was missing. Authority validation now
  precedes expiry and subject-conclusion outcomes.
- The first automated PR review confirmed a P2 upgrade gap in the stricter
  fake-publication producer: a durable v1 task could not replay once its run
  expected the new content-addressed policy. Exact migration and idempotent
  convergence tests now cover both the compatibility path and its refusal
  boundaries.
- The review-fix refute pass found that the first compatibility patch rolled
  direct replay back with its success sentinel, trusted a caller-selected
  legacy shape at the store boundary, and omitted dispatched tasks from bulk
  recovery. Replay now commits only an actual migration while current-policy
  replay retains its revision-neutral rollback, the store requires the exact
  one-key trust-profile translation, and integration tests drive both recovery
  paths.
- The second automated review found that 0027 could bind an already-dispatched
  v1 publication watch to the legacy digest before replay migrated its run.
  Policy convergence now moves every exact run-bound schedule in the same
  transaction, and both the store gate and dispatched recovery regression
  prove the run, policy, and schedule land on one digest. Final refutation
  added rollback proof for a conflicting schedule after the run update and
  exercised armed, fired, resolved, and expired schedule states.
- The third automated review found that bulk reconciliation would otherwise
  rescan all dispatched publication history every 100 ms after the upgrade.
  Dispatched convergence is now one successful recovery pass per store epoch;
  a failure remains loud and retries, while daemon restart or in-place restore
  receives a fresh pass. Final refutation caught the restore case before the
  marker was bound to `sync_epoch`.
- The fourth automated review found a startup race between the engine's first
  asynchronous recovery pass and the scheduler's immediate due-fire pass.
  Daemon composition now completes the epoch's dispatched convergence
  synchronously in both fake and Claude modes before launching any scheduler
  goroutine; reconciliation keeps the same gate for restore and non-daemon
  entry paths.
- A later startup review found the same legacy authority can own watches while
  its task is still pending: publication arms watches before marking dispatch,
  so a crash can persist that state. Epoch-bound synchronous convergence now
  scans both pending and dispatched owners before scheduler startup.
- The fifth automated review found that 0027 preserved one-shot expiry shapes
  accepted before #462. Migration now removes expiry from every legacy
  one-shot. An already-expired one reopens armed at the next generation so a
  current scheduler pass can truthfully fire or resolve it; the generation
  bump avoids collision with any occurrence consumed by the legacy expiry
  path. Rejected: translating it directly to `fired`, which would falsely claim
  the handler ran. A preflight guard rejects column/body contradictions and
  malformed expiry state before any rewrite, preventing normalization from
  laundering an already unreadable row into valid authority. Exact nanosecond
  comparison parses whole seconds without SQLite's fractional-second rounding.
  Because schedules are synchronized entities, the migration advances the
  server revision once and stamps every rewritten row with an incremented
  entity version at that revision; preserving the old cursor would strand
  pre-upgrade clients on stale schedule bodies.
- Refute-first fixtures write store-impossible malformed state past the public
  boundaries. They cover all three workload kinds against missing, malformed,
  item/run mismatch, valid-run retargeting, cross-run, and digest-mismatched
  policy authority and prove no handler runs;
  separate forged-row fixtures prove both one-shot kinds fail reconstruction
  when expiry is present.

## Revisit When

- A new schedule kind lands: its authority class must be added to the
  exhaustive fire-time dispatch before it can construct an event.
- A workload schedule no longer binds an immutable run-scoped attention item:
  add the new exact authority binding to the schedule contract and move its
  migration, API, and generated consumers together.
