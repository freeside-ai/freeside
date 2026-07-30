# Fail Closed on Invalid Execution Reservation States

Work unit: #392. Scope: `daemon/internal/publish/`, `devlog/`.

## Decision

**Reject every state outside the execution reservation gate's registered
vocabulary.** The gate now sends its trailing fallback through
`unhandledInvocationState`, matching the other reservation-state dispatches.
Relying on the classifier's current return paths was rejected because a future
regression returning the invalid zero value would otherwise convert an
unrecognized durable state into publication authority.

The switch is extracted into a pure state evaluator so the invalid zero value
can be injected directly in a focused test. A mutable classifier test seam was
rejected because it would weaken the production boundary and introduce shared
state solely for testing.

## Refute-First Verification

The invalid-state regression drives the execution reservation evaluator with
the zero `invocationState` and requires the same fail-closed error used by the
other reservation gates. The committed branch returns success explicitly only
after re-authenticating its persisted source invocation and reserving run; the
refute pass confirmed that preserving this return is necessary for committed
retry convergence. Existing execution publication tests exercise free,
reserved, and committed states through the real classifier, including
successful retry and recovery paths.

Revisit when: the registered invocation-state vocabulary changes. Every
reservation gate must then handle the new state explicitly before its trailing
fallback.
