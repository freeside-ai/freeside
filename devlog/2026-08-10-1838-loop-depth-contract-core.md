# Loop-Depth Contract Core

Date: 2026-08-10. Tracking: #652. Consumers: #655, #659, #525.

## Decisions

**Canonical stage names do not retype or constrain persisted `Stage.Name`.**
`StageName` closes the workflow-definition vocabulary over elaboration,
implementation, review, and verification for #655. Existing runs also persist
engine-internal names such as `implement`, `conversation_feedback`, and
`fake_candidate_publication`; rejecting those at `Stage.Validate` would turn a
new configuration contract into an incompatible data migration. A future
workflow-definition type validates `StageName`; persisted execution stages
stay string-typed until a unit explicitly migrates their older meanings.

**`Artifact.Type` becomes the closed `ArtifactKind` enum after a complete
constructor and persistence sweep.** The set retains every real role already
written by production or pinned by a persisted golden (`specification`,
`policy`, `evidence`, `image`, `verification_report`, `command_transcript`,
`verify_log`, `license_scan`) and adds only #655's `research`. Test-only
placeholders (`log`, `img`, `report`, `verification-evidence`) move to the
corresponding real roles instead of becoming permanent vocabulary. This keeps
the serialized strings unchanged for real rows while making an unknown kind
fail reconstruction closed.

**The public evidence-artifact type is the same closed vocabulary.** The
OpenAPI source and its app mirror enumerate the exact `ArtifactKind` values,
so generated Swift makes an unknown evidence kind unrepresentable before the
daemon's reconstruction boundary. The app fixture and display use that
generated enum rather than a parallel string vocabulary.

**Review disposition is a separate concept from publication-gate finding
disposition.** `ReviewDisposition` (`fixed`, `declined`, `deferred`) and
`ReviewDispositionRecord` serve #525's per-review-finding history. The existing
`FindingDisposition` (`blocking`, `waived`) remains the candidate publication
gate's stance; renaming it would conflate two authorities and widen this unit.

**A disposition is trusted only through its source finding and exact review
round.** The immutable key is `(finding_id, round)`. Every write and read
validates the decoded body, cross-checks copied columns, reconstructs the raw
finding to confirm its run, reconstructs the review record for `(run, round)`,
and confirms that record lists the finding. Migration 0037 adds a trigger for
the same membership invariant so direct SQL cannot create an unbacked row.
Later rounds append history; no disposition or raw finding is rewritten.

**A fixed disposition names a trusted later review.** It stores the invocation
of a later same-run review on the same base and a different head. That review
record already owns the exact remediation base/head and completion evidence;
reconstruction re-derives the pair instead of trusting caller-supplied SHAs.
This prevents a rendered history from presenting an arbitrary head as fixed;
readiness still decides whether the derived head remains current.

**Initiator configuration is a resolved domain shape.** Manual initiators
carry neither label nor mode. Label initiators carry a non-empty label and an
explicit `propose` or `auto_start` mode; parsing #659 applies the conservative
`propose` default before this boundary. `scan` remains absent because its only
consumer is Phase 2.

## Rejected Alternatives

- Globally validate `Stage.Name` against the four canonical names: rejects
  existing engine-owned persisted stages.
- Leave `Artifact.Type` as a validated string: permits invalid states after
  the sweep showed the actual persisted set is finite.
- Reuse or rename `FindingDisposition`: erases the distinction between review
  remediation history and publication authorization.
- Trust the disposition row's copied run and round: permits a well-formed body
  to be rebound away from the review pass that actually produced its finding.

## Refute-First Verification

Three findings were confirmed and fixed. First, filtering a keyed read or
run list on copied columns could omit a row whose `finding_id`, `run_id`, or
`round` had been displaced; reads now reconstruct the complete table before
selection, with adversarial coverage for all three keys. Second, an idempotent
Put replay compared only the canonical body authority and could overlook a
tampered copied column; replay now reconstructs the existing row before the
immutable comparison. Third, the initial migration trigger trusted the
normalized review-to-finding bridge; it now also proves agreement with the
raw finding and both canonical bodies before accepting direct SQL.

The refute pass also confirmed that the original restart test closed the
store gracefully and therefore did not prove the issue's kill-recovery
criterion. A separate test now commits in a helper process, signals readiness,
is forcibly killed without `Store.Close`, and recovers the disposition from a
new process-owned handle.

The lenses could not disprove the compatibility of legacy execution-stage
names, completeness of the artifact-role sweep, separation of review and
publication dispositions, initiator-shape validation, intact-row idempotency,
or the write-time finding/review binding. No finding was accepted without a
fix, and no speculative widening was accepted.

The automated PR review identified one additional stale-attestation gap:
`fixed` previously recorded only the source review round. The contract now
records a later, same-base review invocation and derives the remediation head
from its trusted record, with direct-SQL coverage for a mismatched base.

The same review also caught the public-schema drift created by closing the
domain artifact type alone. The OpenAPI contract and mechanical app mirror now
carry the exact enum, and the generated Swift compile surfaced and closed the
two string-only client call sites.

## Revisit When

- #653 needs a typed persisted stage key and supplies an explicit compatibility
  migration for the Phase 1A internal names.
- A new artifact producer or initiator consumer has a concrete role not in the
  closed set; widen the enum in that consuming contract unit.
