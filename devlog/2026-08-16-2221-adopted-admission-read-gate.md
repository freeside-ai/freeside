# Adopted Admission Read Gate

Issue: #805

## Decision

Allow the strict execution-admission reconstruction gate to accept one
run-scoped trust-profile supersession when the store's existing
`LatestReviewConfigurationRecoveryTransition` authority re-gate proves that
the admission's pinned revision is exactly the recorded superseded revision.
Keep absence, an ineffective transition, a later activation, and any other
profile hop on the existing `ErrTrustProfileSuperseded` fail-closed path.

The authorization is run-scoped because the recovery transition's invocation
identifies the parked review while the recovered durable record belongs to the
implementation invocation. The operator adopted continued processing of that
run under the review-only revision; requiring invocation equality would leave
the production verifier unable to reconstruct the implementation evidence the
adoption explicitly allowed the engine to publish.

Keep effective reviewer-configuration equality in the engine rather than the
store. That equality is launch currency, which depends on the running daemon's
effective configuration digest. The store gate answers the narrower provenance
question using durable profiles, failure evidence, the accepted command, and
the recovery transition it can fully re-gate itself.

## Rejected Alternatives

- Rejected weakening the newest-profile comparison for every review-only
  profile revision. A content delta is not operator authority; only the
  command-backed recovery transition authorizes the historical admission.
- Rejected a verifier-only workaround. Both `GetExecutionAdmission` and
  `GetExecutionExport` share the store gate, and bypassing either would split
  strict reconstruction semantics across callers.
- Rejected walking multiple recovery transitions. The engine and publisher
  authorize one exact superseded-to-latest hop today; allowing older admissions
  through a chain would create a broader policy than the recovery path uses.

## Verification Findings

The refute-first pass proved the positive case through both strict getters and
confirmed their record variants remain unchanged. It also disproved acceptance
for an unrecorded supersession, a command-backing tamper classified as an
ineffective transition, an adoption outlived by a later profile activation, an
adoption for a different profile hop, and a trust-widening revision. Every
unauthorized case retained `ErrTrustProfileSuperseded`; ineffective-transition
details did not leak as an alternate caller-visible authorization result.

The production integration harness replayed the live verifier's strict
admission-then-export read sequence after a successful adoption. Its
trust-widening sibling still refused the strict admission read.

Revisit when the engine and publisher deliberately support multi-hop review
configuration recovery, or when reviewer launch currency becomes durable
store-owned authority rather than daemon process configuration.
