---
run: manual
stage: atomic-admission-regate
date: 2026-08-10
branch: fix/atomic-admission-regate
---

# Atomic Completion Admission Re-Gate

Work unit: #316. Scope: `daemon/internal/signet`,
`daemon/internal/engine`, the engine integration acceptance test, and this
note.

## Decisions

**The mutable admission reconstruction gate runs inside Signet's accepting
transaction.** `AcceptAgentCompletion` takes a per-call read-only pre-commit
gate, and the engine supplies `LookupExecutionAdmission` through it. Signet
runs that gate after inbox dedup and before attachment validation or any
conversation/item write. Chose this over keeping the engine-side read because
a verdict from a separate transaction can become stale before acceptance;
chose a per-call option over a service-level callback because the absence
semantics and error classification belong to the engine's admission policy,
not to every Signet composition.

**A completed replay converges before mutable policy is reconsidered.** Inbox
dedup remains first, so a completion accepted under profile P1 stays accepted
when redelivered after P1 is retired. A genuinely new completion instead
reconstructs its admission against the profile current in the accepting
transaction, and any refusal rolls back the provisional inbox row with the
rest of the transaction. Rejected moving the gate before dedup because that
would turn historical policy drift into a replay failure and break the
exactly-once convergence contract.

**Operating-state gates remain outside completion acceptance.** The hook runs
only `LookupExecutionAdmission`, not `RequireUnattendedAdmissible`: an operator
stop or blocking health item prevents new starts but does not invalidate work
that was already running. This preserves the owner decision recorded in
`devlog/2026-07-27-1846-durable-stop-and-supersession.md`.

## Verification Findings

**Refute pass, all four load-bearing mutations were caught.** M1 ignored the
engine's reconstruction error; the profile-supersession test accepted the
completion. M2 moved the gate before inbox dedup; the redelivery test failed
instead of converging. M3 swallowed the gate error; the rollback/retry test
observed a successful first acceptance instead of the refusal. M4 moved the
gate into a separate preflight read transaction; the serialization test could
not see the provisional inbox row and failed, proving the gate shares the
accepting transaction rather than merely running before it. Each mutation was
reverted before the final suite.

**Fresh-context review found and closed an evidence gap.** The first profile
supersession test established refusal and rollback but would also pass with
the old adjacent-read implementation. The added barrier test observes the
provisional inbox row from the gate, holds that transaction, and proves a
concurrent profile activation cannot commit until acceptance commits. Together
the tests pin atomic placement, rollback, replay convergence, and the engine's
`ErrTrustProfileSuperseded` propagation.

## Revisit When

Revisit the generic per-call option if a second caller needs the same
admission gate; at that point a named Signet acceptance policy may be clearer
than repeated closures.
