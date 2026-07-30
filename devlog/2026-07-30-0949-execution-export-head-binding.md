# Bind Publication to the Recorded Execution Export

Work unit: #318. Scope: `daemon/internal/publish`, `devlog/`.

## Decision

**Use a distinct execution-bound Publisher input, and authenticate it inside
the reservation-settlement transaction.** `publish.ExecutionCandidate` carries
the producing execution invocation separately from the publishing invocation.
The store-backed decision follows #308's reservation from the publishing
invocation to its owning run, authenticates the producing invocation's
`ExecutionAdmission` against that same run, reconstructs its
`ExecutionExport`, and compares the export head with the candidate head before
promoting the reserved outbox row into a publication intent.

The execution-bound path first reconstructs the reservation row itself. A
caller-derived run claim cannot turn a free invocation key into authority;
only a matching durable reservation (or its already-promoted intent on retry)
may reach the execution gate. This closes the fresh-key insertion path found
in automated review while preserving idempotent recovery.

The execution intent persists the producing invocation and reserving run as an
all-or-nothing pair. The shared store ledger decodes that pair before mutation,
requires a claim for the same key and run, and on committed retries verifies
the stored pair again. This prevents a direct ledger caller from inserting an
execution-shaped intent at a free or different key, promoting another run's
payload, or replaying a committed intent under a foreign owner.

Recovery requires the resolver to reproduce both coordinates and re-enters the
execution-bound Publisher path, so a crash after settlement cannot drain
through the weaker attended path. Legacy and fake intents omit both fields and
retain their existing encoding and recovery behavior. Keeping the coordinates
only in process memory was rejected after the refute pass proved that a pending
intent otherwise loses the stronger gate at restart.

The ordinary `Candidate` path remains the attended fake-publication path. It
predates `ExecutionExport` and cannot truthfully acquire one, so applying the
new requirement to every Publisher call would either break that explicit fake
workflow or tempt it to mint false execution authority. A boolean or optional
field on `Candidate` was rejected because forgetting to set it would silently
select the weaker path. The concrete `ExecutionCandidate` API makes the real
call site choose the execution-bound contract.

**Use immutable terminal authority, not current admission policy.**
`GetExecutionAdmissionRecord` and `GetExecutionExportRecord` re-authenticate
the stored row, extracted columns, content-addressed admission, and
admission-to-export binding without asking whether current policy would admit
that already-completed attempt. `GetExecutionExport` was rejected because a
later capability-floor or trust-profile change would strand valid terminal
history and contradict #318's frozen admission/export chain.

The immutable admission must itself record `unattended` mode. This is not a
live-policy recheck: `ExecutionAdmission.Validate` reconstructs the
content-addressed unattended safety bindings, while the explicit mode check
prevents the weaker attended runner class from becoming automatic-publication
authority. An attended execution with an otherwise matching export is refused
before settlement or effects.

**Bind canonical repository identity and the exact authorized base as well as
run and head.** The admission's `BaseRevision` uses the publication vocabulary
deliberately. The gate therefore requires its repository name and immutable
numeric ID to equal the current trust profile, and its base ref and SHA to equal
the candidate's authorization, before accepting the export head. Comparing only
names and refs was rejected in the refute pass: repository names can be
transferred, refs move, and forks commonly share commit SHAs.

**Do not add an engine composer that has no inputs yet.** The production lane
currently ends at its authenticated terminal inbox record; it has no clean
verification, authorization, publication reservation, title/body, or transport
composition. Adding a dead engine helper would not make that path real. The
execution-bound Publisher and its transport/finalization form are the concrete
settlement APIs that composition must call when those inputs exist.

## Refute-First Verification

The negative cases drive the same store-backed Publisher decision as success.
A missing reservation, a missing export, a different recorded export head, a
source invocation from another run, and same-SHA sources with a different
repository name, repository ID, base ref, or base SHA all commit no publication
intent and issue no forge request. Where a reservation exists, refusals leave
it unchanged. The positive case proves that settlement preserves the
publishing invocation and run reservation while accepting a distinct producing
invocation from that run.

The ledger-boundary matrix additionally rejects an execution intent on a free
key with either no claim or a caller-derived claim, a claim for another key, a
payload naming another run, and a committed retry under another run. Recovery
refuses either a changed source invocation or changed reserving run before
transport.

The recovery attack commits an execution intent, forces the post-intent
transport callback to fail, deletes the temporary test database's export row,
and drains the pending intent. Recovery reads the persisted producing
invocation, reports the missing export, leaves the intent pending, and calls
neither transport nor forge.

The mutable-policy attack reopens the seeded store under a capability floor
that rejects the original admission through the live read API. The
execution-bound publication still succeeds through the immutable record APIs,
confirming that the new gate authenticates terminal history rather than
re-evaluating current admission policy.

Revisit when: the production engine gains clean verification and publication
composition. That caller must construct `ExecutionCandidate` from its
authenticated production terminal record and call
`PublishExecutionAfterGateAndFinalize`; the ordinary fake path is not an
acceptable substitute.
