# Closure Effect Kind, Approval Binding, and Store Re-Gate

Work unit #1417 (kind:contract). Defines the domain types recipe v2 closure
authoring needs: the `source_issue_closure` effect kind, the closure approval
binding, and the kind-aware store re-gate. This note records the lasting
contract decisions and the refute-first findings for the returned-object trust
boundary.

## Split at the Step 6/7 Seam (Owner Decision)

Chose to deliver only steps 1-6 (closure effect kind, parameters, approval
binding, the `0077` rebuild migration, and the kind-aware store gates) in
#1417, and to move steps 7-8 (the publication-authoring artifact and its table)
to a new `kind:contract` unit that #1418 and #1419 will `starts-after`. The
owner elected the split over running the ~1,850-line unit whole, because the
seam is clean (the authoring artifact shares no code with #1443 or the closure
half) and review bandwidth is the binding constraint on the serialized contract
chain. Cost accepted: one extra serial contract unit on the critical path.

## Digest Stability: omitempty Over a Per-Kind Canonical Form

Chose `omitempty` on the new `ClosureProposal` pointer in both the wire and
canonical structs over a per-kind canonical encoding. A rendered `null` for the
absent arm would change every stored `run_proposal` proposal's content digest
and fail its revalidation after upgrade. `omitempty` keeps the one-kind
encoding byte-identical; a pinned digest fixture and the unchanged
`effect_proposal.golden` guard it. `TaskProposal` keeps no `omitempty`, so a
closure proposal deliberately renders `run_proposal: null`, pinning the absent
arm the way `SpecificationSource` does.

## Approval Binds the Base SHA, Not Only the Base Ref

The closure approval carries the base SHA explicitly, in addition to the base
ref, candidate head, publication identity, and proposal digest.
`publish.IdentityInput` binds `BaseRef` but not the base SHA, so a base advance
under the same ref would otherwise leave a prior approval valid. This folds in
the PR #1420 review round 10 follow-up: the supersession invariant is any change
to the prospective merge (head, base ref, base SHA, or publication identity)
supersedes a prior approval. `domain` does not import `publish`, so the binding
holds the identity as a `domain.Digest` value.

## Migration Rebuild: Re-Insert to Rebalance the Deferred-FK Counter

The `0077` rebuild widens the `effect_kind` CHECK, which SQLite cannot alter in
place. Five tables reference `effect_proposal_instances`, the store runs with
`foreign_keys=ON`, and the migration runner wraps each file in one transaction
where `PRAGMA foreign_keys` is a no-op. `PRAGMA defer_foreign_keys=ON` alone is
insufficient: `DROP TABLE`'s implicit delete raises the deferred violation
counter once per orphaned child, and a rename of a pre-filled replacement never
lowers it, so the commit fails (observed empirically). The migration instead
holds the rows, drops the old table, recreates it, and re-inserts; each parent
insert resolves an outstanding child reference and returns the counter to zero.
No prior migration rebuilt a referenced parent, so the plan's cited precedents
(0057, 0069) did not actually exercise this; this pattern is established here.

## Store Re-Gate: Recommended Cannot Re-Derive the Client's Issue Number

Chose to have `closableSource` derive the closable fact from current rows and
pass it to a pure `GateSourceIssueClosure`, mirroring `NewArtifact(in,
approvedRecipes)`. A daemon-bound `issue_subject` task source yields a
`verified` fact with the exact issue; any other source yields a `recommended`
fact carrying the project repository with issue number zero, because the client
chose the issue and the store cannot re-derive it (only
`engine.ProductionPublication.SourceIssue` holds it; the store keeps only the
publication digest). The verified arm re-checks the exact target; the
recommended arm re-checks the repository only. The approval binds the proposal
digest, so a later change to the client's chosen number voids the approval.

## Refute-First Findings (Returned-Object Trust Boundary)

Attack vectors traced against the store re-gate; each confirmed handled or
allowed by an explicit decision:

- **Provenance elevation (recommended -> verified) or a wrong verified issue:**
  rejected by `GateSourceIssueClosure` (provenance/target mismatch). Tested.
- **Cross-repository recommended target:** rejected (repository mismatch);
  covered by the domain gate test.
- **Cross-repository verified target:** rejected. A daemon-bound issue subject
  is verified only within the project's own repository (plan §5.13: a
  cross-repository source yields no proposal), so `closableSource` re-anchors
  the verified target to `project.RepositoryID` and fails closed on a mismatch.
  The `run.ProjectID`/`task.ProjectID` joins prove which task the run names but
  not the source's repository, so a tampered or mistaken `issue_subject` row
  could otherwise label a foreign-repository close as verified and mislead an
  approver. Covered by a store fail-closed test.
- **Redirect run -> task -> source via a raw column tamper:** caught by the
  existing run and task reconstruction gates (body vs column cross-checks,
  `task_runs` binding), which `closableSource` relies on and re-verifies
  (`run.ProjectID == project.ID`, `task.ProjectID == project.ID`); the verified
  target's own repository is additionally re-anchored to the project (above).
- **Coordinated resolves-flag flip:** the resolves flag is part of the proposal
  digest, so flipping it changes the digest; no prior owner approval (bound to
  the old digest) authorizes the tampered proposal. A forged `daemon_fallback`
  with `resolves=true` is additionally rejected by `Validate`. Allowed by
  design: the store does not police `resolves` against state; the digest
  binding plus the owner approval is the defense.
- **`Present=false` unreachable from `closableSource`:** every project has a
  repository, so the store always admits at least a recommended fact. Allowed
  by explicit decision: a spuriously admitted recommended proposal still cannot
  close anything without a matching owner approval. `ErrClosableSourceAbsent`
  remains exercised by the domain gate test.
- **Both parameter arms set in one body:** rejected by `Validate`'s mutual
  exclusion (`TaskProposal != nil` xor `ClosureProposal != nil` per kind).

## Revisit When

- #1443 adds the `effect_proposal` attention type and the closure decision
  path; it will add a closure revision path, at which point the task-only guard
  in `authenticatedProposalRevision` is lifted for closures.
- The wire values (`source_issue_closure`, the `source_issue_closure` JSON key,
  and the `verified`/`recommended` provenance words) are read by #1443; a later
  change to any must be recorded on #1417 before #1443 starts.
