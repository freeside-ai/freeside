# Review Contradiction Recovery

Issue #580 adds the first operator-authorized recovery for a persisted review
failure, so its action contract and returned-object trust boundary need a
durable rationale.

## Decision

Chose a single-use, append-only `recover_review` transition over mutating or
deleting the failed review row because the contradiction is evidence and the
operator decision must authorize only the exact row the card displayed. The
transition binds the run, invocation, round, base SHA, head SHA, and the digest
of the failure's originally persisted body bytes. A contradiction without a
matching transition remains pending behind one deterministic, high-priority
attention item; it does not terminalize the publication task or emit repeated
errors.

Chose to re-establish authority from current stored state at both transition
write and read boundaries. Signet verifies that the accepted command targeted
the recovery action and exact carrier item, then the store independently
cross-checks that carrier against the persisted contradiction and its actual
stored body digest. Reconstruction repeats the command, item, failure-class,
and binding checks. Decoded trust flags or caller-supplied digests never grant
recovery by themselves.

Chose a distinct `production-recovered-review-exhaustion-<run>-<limit>` identity
only for a contradiction recovered at the hard limit. Its fixed prefix is
disjoint from the normal `production-review-` namespace before either appends a
caller-controlled run ID. A refute-first review showed that reusing the round's
normal review-item identity would ask the immutable attention store to turn the
resolved contradiction carrier into a diminishing-returns item, permanently
wedging a recovery accepted at the final round. A second pass showed that a
`production-review-exhausted-` prefix could collide with a normal item for a
run named `exhausted-...`, and that changing every exhaustion identity would
duplicate a legacy non-contradiction item after an upgrade crash. Existing
exhaustion paths therefore retain their historical identity.

## Rejected Alternatives

- Rejected a broad retry or run-level acknowledgement because it could recover
  a different invocation, candidate head, or rewritten failure.
- Rejected rewriting the failure as recovered because that would destroy the
  original bytes and collapse failure evidence with operator authority.
- Rejected treating every review failure as recoverable. Only the durable
  contradiction class has the stranded-run problem and explicit owner intent
  in #580; transient, configuration, and ordinary review outcomes retain their
  existing policy.
- Rejected reusing the contradiction carrier for hard-limit exhaustion because
  attention type and recovery binding are immutable after creation.

## Refute-First Findings

- **Confirmed and fixed:** recovery at `review.hard_round_limit` collided with
  the resolved contradiction carrier's item ID. A limit-one regression now
  proves the recovery completes into a separate open exhaustion item.
- **Confirmed and fixed:** the first alternate namespace could collide across
  runs, and applying it to all exhaustion paths broke upgrade convergence. A
  prefix counterexample and a seeded legacy-item regression now pin the
  disjoint recovered namespace and the historical non-contradiction identity.
- **Confirmed and fixed:** a restart under a different effective reviewer
  configuration could replace an unresolved contradiction with a later
  configuration failure. Reconciliation now checks the parked contradiction
  and its exact recovery transition before configuration drift can advance the
  review state.
- **Confirmed and fixed:** delivery-derived timing legitimately advances an
  open contradiction card's version and timing. Reconciliation now excludes
  only those mutable telemetry fields when it verifies that the parked card's
  identity, presentation, action, and recovery coordinates still match.
- **Confirmed and fixed:** the operator client offered recovery without
  rendering the six coordinates that authorize it, and plan §4 did not yet
  define the new item/action contract. The card now renders every coordinate,
  and the authoritative attention table carries the exact recovery behavior.
- **Rejected by verification:** changing any one of the six binding coordinates,
  removing the backing command, altering the transition, changing the failure
  class, or changing the persisted failure body makes reconstruction fail
  closed.
- **Rejected by verification:** command replay cannot append a second recovery
  or partially resolve the item; signet records the command, transition, and
  item conclusion in one write transaction.
- **Rejected by verification:** a matching recovery does not rewrite the
  failure. The next review round advances while the original domain value and
  exact persisted body digest remain unchanged.
- **Rejected by verification:** the API/client mirror cannot silently omit the
  binding. The required-nullable property is present on every attention item,
  required for `review_contradiction`, forbidden elsewhere, and preserved when
  the carrier resolves.

Revisit when recovery is proposed for another review-failure class or when a
storage-format migration can no longer retain the original failure body bytes
as the binding authority.
