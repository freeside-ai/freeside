# Retain Workflow-Audit Evidence for Review

Work unit: #274. This is a mandatory note because the change retains
repository workflow contents as provenance and defines their trust-boundary,
access, retention, and deletion contract.

## Decision

Retain the exact canonical JSON body addressed by
`WorkflowAuditDigest` in a separate content-addressed
`workflow_audit_evidence` table keyed by repository and digest. Keep
`workflow_audits` as the existing insert-only observation ledger of reduced
facts. A repeated identical audit therefore adds another observation row but
deduplicates its large evidence body; evidence deletion never rewrites audit
history.

While the active profile binding authenticates, retain at most two distinct
bodies per repository: the evidence bound by that approved profile and the
latest observed audit evidence. A new observation prunes any body in neither
role. This is the minimum set that supports the one-time profile review and a
current approved-versus-observed drift review without retaining workflow
contents indefinitely.
Profile activation participates in the same pruning boundary. A profile whose
digest has an audit observation may activate only while that evidence remains
retained; activating a pruned historical profile requires a fresh audit first.
Legacy profiles with no audit observation preserve their pre-#274 activation
behavior but cannot satisfy the evidence review projection.
Each activation also records the profile's workflow-audit digest, but pruning
uses it only after decoding the joined profile, recomputing its content digest,
and matching both coordinates. A stale or tampered profile or activation row
therefore suspends pruning and retains extra evidence until a validated
activation re-establishes deletion authority. This fail-safe exception may
temporarily exceed two bodies, but never lets unauthenticated state select
content for deletion.
The audit ledger is held to the same boundary: both the legacy-profile
exception and selection of the latest observed digest reconstruct and
cross-check audit bodies against their extracted columns before those
coordinates can influence retention.

Cap each evidence body at 16 MiB in both domain validation and SQLite. A live
audit exceeding the cap fails closed before persistence. Access is only
through the daemon-internal store review projection, not general audit
listing, synchronization, or an API. The projection accepts a validated
proposed profile for the initial pre-activation review, or resolves the active
profile for later drift review. It returns the complete approved and observed
bodies plus a stable sorted list of changed top-level sections, so #238 can
present the digest-bound evidence directly without reconstructing it from
GitHub.

Ordinary formatting and `WorkflowAudit` serialization redact or omit the
evidence. JSON exposure exists only when a caller deliberately serializes the
review projection's evidence value. Store reads re-hash the body and
cross-check its repository and digest before returning it.

Explicit deletion removes all retained evidence for one repository while
leaving the observation and profile history intact. A later review fails
honestly with missing evidence until a fresh audit reintroduces the required
body; Freeside never reconstructs deleted provenance from live state. This is
logical database deletion, not a claim of physical secure erasure from SQLite
free pages, WAL history, or an already-created encrypted checkpoint.

## Changed Assumption

The #172 contract accepted `WorkflowAudit` as a trusted observation that was
not self-certifying because only its digest and derived facts survived. #274
now retains the digest-addressed body, so a present evidence body can and must
re-certify its repository/digest binding at construction, persistence, and
review. Legacy audit rows without evidence remain readable as reduced history,
but they cannot satisfy the new review projection.

This does not change workflow-authority derivation or drift semantics.
`EvaluateTrustDrift` continues to compare the same digest and explicit
authority facts.

## Rejected Alternatives

- **Embed evidence in every audit row.** This duplicates large workflow
  contents on every publication and makes retention require rewriting or
  deleting the insert-only audit ledger.
- **Retain every unique digest indefinitely.** Content addressing removes
  duplicates but does not bound repositories whose automation changes over
  time; it violates #274's explicit containment requirement.
- **Retain only the latest body.** A drifted observation would evict the
  approved body needed to explain what changed.
- **Re-fetch evidence for review.** Live GitHub state is not the body the
  approved digest bound and would turn provenance into an unauthenticated
  reconstruction.

## Verification

Tests cover digest/repository binding, the size ceiling, formatting and JSON
redaction boundaries, migration of legacy audit rows, exact-body persistence,
approved-versus-observed section comparison, two-body retention, explicit
deletion, activation pruning/refusal after evidence eviction, stale-profile
recovery, evidence tamper rejection, refusal to prune from tampered profile or
activation bindings, and workflow-content non-disclosure through errors and
formatting. Refute-first and automated review passes found and closed the
unsafe deletion-authority class across profile bodies, activation coordinates,
and audit-ledger coordinates; tampered state now retains evidence or fails the
operation instead of selecting content for deletion.

## Revisit When

The review surface needs historical drift forensics beyond the active
approved and latest observed pair. Add an explicit owner retention policy or
exported immutable artifact at that point; do not silently widen the database
retention set.
