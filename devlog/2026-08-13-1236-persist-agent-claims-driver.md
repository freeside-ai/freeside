# Persist Production AgentClaims Through the Driver

Work unit: #381 (`kind:fix`, wave-5; tracking #651). Steps 2–5 of the
issue's implementation plan; step 1 (the migration + store accessors)
landed as #732 / PR #741, note
`devlog/2026-08-13-0930-agent-claims-record.md`. Mandatory note: a
returned-object trust boundary rides here (the driver's claim record
feeds a store readback that decodes and re-gates it), plus a
consequential interpretation of the acceptance spec.

## The Fix (Steps 2–3)

`artifactStore.RecordClaims` (`cmd/freesided/claude_driver.go`) already
ran its artifact-row loop inside one `store.Write`; the defect was that
it never wrote the invocation-keyed claim record, so the label and
inline text were dropped and no durable review surface carried the
claims. The fix adds `tx.PutAgentClaims(ctx, id, claims)` to that same
transaction after the loop. One line of behaviour, three properties it
buys, all from #732's substrate:

- **Complete labeled set** survives (label, artifact id, digest,
  provenance incl. sensitivity, inline text), not the artifact rows
  alone.
- **Atomic**: the record and its artifact rows land together or not at
  all, so `RecordClaims` never reports success for a partial record.
- **Write-once**: an identical replay converges; any differing set is
  `ErrImmutableConflict`.

The `Artifacts.RecordClaims` port doc (`internal/exec/stage/ports.go`)
is tightened to state that contract; the signature is unchanged, so
this stays `kind:fix`, not a contract change.

## AC-6 Interpretation (the consequential decision)

Acceptance item 6 reads "shared stage-driver contract tests bind the
fake and production drivers to the new semantics." Taken literally as
the **StageDriver** contract, it cannot be satisfied: the fake
StageDriver (`internal/exec/fake`) has no `Artifacts` dependency and
never records claims, and the production StageDriver's own
`OutcomeComplete` contract scenario cancels before `persistEvidence`
ever runs. So the shared StageDriver contract has no claim-recording
surface to bind.

Read against the code, "the fake and production drivers" are the two
**`Artifacts` implementations**: the in-memory `stubArtifacts` (fake,
the double the stage-driver tests lean on) and the store-backed
`artifactStore` (production). Both genuinely implement `RecordClaims`.
So the unit adds a reusable **Artifacts** contract
(`internal/exec/contract/artifacts.go`, `RunArtifactsContract`,
#665's pattern) with four cases — complete-set readback, idempotent
replay, differing-set conflict, empty-set no-op — and runs it from
both `internal/exec/stage` (fake) and `cmd/freesided` (production).

Binding the fake to that contract required upgrading `stubArtifacts`
to enforce the same write-once/conflict/empty-no-op semantics it
previously ignored (it silently overwrote its map). That is the point
of the binding: the stub the stage tests trust now cannot diverge from
production on these semantics. No existing test drove claims through
the stub, so the upgrade broke nothing.

Durability across a store reopen and crash/atomicity convergence are
production-only (the fake has no persistence), so they live as
`cmd/freesided` adapter tests, not in the shared contract.

## Refute-First Pass (Step 5)

Scope: the driver transaction path and the shared contract-test
surface. The readback trust boundary itself is store-side and had its
refute pass in #732 (`TestGetAgentClaimsRejectsTamperedRow`). No
confirmed defect.

- **Claim-set order determinism — the one real risk, CONFIRMED SAFE.**
  Order-sensitive byte identity means a re-run that reorders the claim
  set would spuriously conflict instead of converging. `buildClaims`
  (`internal/importer/evidence.go:191`) appends in `EvidenceManifest.
  Entries` **slice** order; the `blobs` map is lookup-only. The export
  manifest is immutable and content-addressed, so a crash-convergence
  re-run re-imports the identical entry order → byte-identical claim
  set → `putImmutable` converges. This discharges the #732 note's
  "Revisit when #381 binds RecordClaims" ordering condition.
- **Partial success — refuted.** Single `store.Write`; the
  atomicity test forces a claim-record FK failure (no invocation row)
  and asserts no artifact row and no claim record survive the
  rollback, and `RecordClaims` returns the error.
- **Label/text drop — refuted.** The complete-readback case asserts
  full JSON identity, including inline text and sensitivity class.
- **Silent overwrite — refuted.** `putImmutable` conflict, covered at
  both the store (#732) and adapter/contract layers.
- **Laundered trust bit — N/A on write.** `domain.AgentClaim`'s wire
  shape carries no `publish_eligible`; the read re-gate is store-side.

### Accepted by Decision (not enforced)

- **A claim's `provenance.producer_invocation_id` is not coupled to the
  record key.** The #732 note deferred this to #381 as a "does the
  writer guarantee it?" question. It does not, and need not: the writer
  (`buildClaims` → `mapEvidenceProvenance`) takes each claim's producer
  provenance from the evidence entry, which is provenance about the
  referenced artifact's origin, semantically distinct from the owning
  invocation key. No privilege rides on it (agent claims are never
  publish-eligible) and no consumer relies on the equality, so adding a
  domain invariant or a store re-gate would be the read-re-gate
  over-reach #732 rejected. No enforcement added.

## Rejected Alternatives

- **Bind the StageDriver contract instead** (literal AC-6 reading):
  impossible, as above — the fake StageDriver has no claims surface.
- **An AttentionItem at record time**: no fitting `AttentionType` and
  no consumer (plan assumption, unchanged).
- **Skip the shared contract, test only the production adapter**:
  leaves the fake free to diverge, which is exactly what AC-6 forbids.

## Revisit When

- **A consumer needs the claim record sync-carried** (run timeline,
  EvidencePublisher labeled-claim rendering): that promotes it to a
  wire contract (domain + api + app), a separate `kind:contract` unit,
  per the #732 note's same condition.
