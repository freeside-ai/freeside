# Intake Admission Reserves the Elaboration Run, Not the Implementation Run

Work unit: #744 (kind:contract, wave-5 contract chain, tracking #651;
owner-directed insertion ahead of #659). Mandatory note: `kind:contract`
change and a returned-object trust boundary (the admission binding's
reserved-run field). Scope: `daemon/internal/domain`,
`daemon/internal/store`.

## Decision

`IntakeSubjectBinding.ImplementationRunID` is renamed to `ElaborationRunID`,
and `MintIntakeDeclaration` reserves/mints against the pre-approval
**elaboration** run. This resolves the "Revisit when #659 begins the
reconciliation loop" condition #720 recorded
(`2026-08-12-2015-label-intake-contracts.md`): #720 designed admission to
reserve "the implementation-run identity" with a persisted `Run`, but that
model cannot compose with the merged elaboration lane (#655/#687).

## Why the Implementation Run Cannot Be the Reserved Run

Admission must persist a `Run` at mint time: `MintIntakeDeclaration` calls
`GetRun(runID)`, and the work-unit-declaration re-gate
(`regateWorkUnitDeclaration`) requires the run to exist. But the elaboration
lane creates the implementation run **fresh at spec approval**:

- `SubmitElaborationRun` refuses a pre-existing implementation run
  (`elaboration.go`, `GetRun(ImplementationRunID) == nil` → `ErrImmutableTransition`).
- `submitProductionRun` at approval requires `existing.SpecDigest ==` the
  approved spec, and `SpecDigest` is immutable (`transitions.go`). A run
  reserved at admission carries a placeholder spec, so it can never be
  reconciled with the elaborated spec.

The only run that can legitimately exist at admission and compose with the
lane is the elaboration run: its resolved policy is keyed to the elaboration
run (`SubmitElaborationRun` enforces `ResolvedPolicy.RunID == ElaborationRunID`),
and #659's issue-subject arm adopts the reserved elaboration run at start,
minting the implementation run fresh at approval as the lane already does.

## Why Rename, Not Add a Second Field

The implementation run id is derivable one-way from nothing the binding holds
(`ElaborationRunIDForImplementation` is a SHA-256 preimage: impl→elab only).
#659 derives the implementation run id deterministically from the occurrence's
own coordinates (repo, issue, label, ordinal), so the binding does not need to
store it; recording it would couple the contract layer to #659's derivation.
The binding names the one run it authenticates — the reserved elaboration run —
and stays a truthful trust bit.

## Rejected Alternatives

- **Reserve the implementation run with a placeholder spec.** Breaks approval
  (immutable `SpecDigest`) and is refused by `SubmitElaborationRun`.
- **Relax `MintIntakeDeclaration` to not require a `Run`.** The
  work-unit-declaration re-gate's run FK still requires it; the resolved
  policy and declaration would dangle.
- **Store both `ElaborationRunID` and `ImplementationRunID`.** The impl id is
  not derivable at the store layer and would push #659's occurrence→run
  derivation into the contract, for a value #659 recomputes anyway.

## Returned-Object Trust Boundary (Refute-First Pass)

The admission binding is a returned-object trust boundary (#720's whole
point): the reconstruction re-gate re-derives the binding from the durable
declaration and requires the stored row to match. This change renames the
run field the binding carries, so the high-assurance profile requires a
refute-first pass proving the rename does not weaken that boundary. Lenses
ran to *disprove* preservation; all confirmed it or were rejected by
verification.

- **Confirmed — no read path bypasses the re-gate or reads a stale binding
  name.** An intake-scoped grep leaves no reference to the renamed binding
  field: `deriveIntakeAdmission` sets and `verifyIntakeAdmission` / `Validate`
  authenticate `ElaborationRunID`, and every occurrence reconstruction still
  funnels through them. The `ImplementationRunID` fields and
  `implementation_run_id` JSON keys that remain in `engine/elaboration.go` and
  `cmd/freesided/submit.go` are the elaboration lane's *own* implementation-run
  identity (`ElaborationRunSpec.ImplementationRunID`, `elaborationRequest`), a
  different field this rename does not touch. `go build/test/vet ./...` and
  `golangci-lint` are green.
- **Confirmed by harness — the authenticated invariant is unchanged, only its
  operand renamed.** A diff-read only *asserts* this, so
  `TestIntakeSubjectBindingRenameEquivalence` *measures* it: it reconstructs
  the pre-rename `Validate` (`git show 04271c82~1`) and, over a combinatorial
  corpus feeding each version its own field layout carrying the same logical
  values, asserts old and new reach the same accept/reject verdict
  sentinel-for-sentinel. Every corpus point agrees, so no check was reordered,
  no operand swapped, and no identity dimension dropped or newly trusted.
  `Validate` still binds `WorkUnitID == WorkUnitIDForRun(ElaborationRunID)`
  (previously the same over `ImplementationRunID`); the store re-gate's logic
  is textually unchanged (only the struct-literal field name in
  `deriveIntakeAdmission`) and is exercised by #720's unchanged reconstruction
  corpus, which passes.
- **Rejected by verification — "an old-key row could be silently misread."**
  The JSON key changed (`implementation_run_id` → `elaboration_run_id`), so a
  row persisted under the old key decodes the field empty, which `Validate`
  rejects (`ErrEmptyID`) — it fails closed, never silently mis-binds. No such
  row can exist anyway: the `intake_occurrences` table carries no production
  data (unreleased), so no migration is needed and none is added.
- **Accepted by decision — the implementation run id is not stored.** The
  binding names only the reserved elaboration run it authenticates; #659
  re-derives the implementation run id from the occurrence's own coordinates
  (Rejected Alternatives above), so no new decoded trust bit is introduced.

The golden round-trip and the #720 domain/store reconstruction tests
exercise the re-gate with the renamed field and pass.

## Revisit When

#659's issue-subject start arm lands: confirm the arm adopts the reserved
elaboration run (creates the invocation/markers on the pre-existing run) and
derives the implementation run id from the occurrence, asserting
`ElaborationRunIDForImplementation(derived) == binding.ElaborationRunID`.
