# Durable Project↔Repository Authority for Label-Intake Run Binding

Issue #740 (kind:contract, wave-5 contract-chain head #651). Closes the
caller trust assumption #720/PR #735 documented in `MintIntakeDeclaration`
(see `2026-08-12-2015-label-intake-contracts.md`, "Read-re-gate over-reach
cluster"; owner ratified option (a): #720 lands the assumption, this unit adds
the durable authority ahead of #659).

## Decision: A Write-Once `Project` Entity as the Authority

Chose a new immutable `Project` record (`ID ProjectID`, `Repo`,
`RepositoryID`), one row per project, over two rejected alternatives (owner
GQ1, defaults ratified by fiat assignment of the plan):

- **Rejected `ProjectID` on `ProjectImage`.** ProjectImage is per-build and
  many-per-repository, so project→repository authority would be derived from
  possibly conflicting rows; its content-addressed encoding
  (`project_image.go` ComputeID) would also need a version bump for the new
  field.
- **Rejected `RepositoryID` on `Run`.** That moves the same unverifiable
  caller assertion onto every run mint instead of removing it — the run body
  is caller-supplied, so it is not an authority.

One immutable row per project is the smallest thing that is actually an
authority: the binding is asserted once (by #659's reconciliation config, or
later by `freesided onboard`), made immutable and undeletable by the store,
and then every run's project→repository tie is verified against it rather than
re-assumed per run.

## Decision: Where the Tie Is Enforced, and the Availability-vs-Integrity Split

Two enforcement points, deliberately different in what a missing/foreign row
means:

- **Mint gate (`MintIntakeDeclaration`).** Resolves `run.ProjectID` through
  `GetProject` and refuses a run whose project belongs to another repository
  (`ErrIntakeProjectRepositoryMismatch`); an unregistered project fails closed
  (`ErrNotFound` propagates). This replaces the deleted caller-trust comment
  block.
- **Read re-gate (`deriveIntakeAdmission`).** Re-runs the same check on
  reconstruction, so a stored admission whose durable declaration names a
  cross-repo project is unreadable, not actionable.

The load-bearing distinction (owner-ratified, #720 round 11): the projects row
is **write-once and undeletable**, unlike the policy artifact whose *current
availability* the read boundary deliberately tolerates
(`subject_input_missing/stale` is #659's start-time refusal, not a read
error). So the project IS re-resolved and required on read, while the policy
artifact is only named. This is why the check belongs in the read boundary:
for an authentic binding the row can never legitimately be absent, so a
missing or foreign row is corruption to hold on, not a transient the boundary
must pass through. The read-path rejection wraps
`ErrIntakeAdmissionInconsistent` (the established "hold, don't act" signal) and
additionally tags `ErrIntakeProjectRepositoryMismatch`.

## Decision: No `UNIQUE(repository_id)`

Multiple projects may share one repository (`UNIQUE` on `project_id` only).
Which project a repository's label intake mints under is #659's configuration;
a premature repository-uniqueness invariant would forbid a legitimate
two-projects-one-repository setup. Revisit only if one-project-per-repository
becomes a real invariant.

## Refute-First Verification (Returned-Object Trust Boundary)

Lenses run to *disprove* that the fix closes the cross-repo hole; all
confirmed the fix or were rejected-by-verification:

- **Confirmed — no write path creates a cross-repo binding.** Both entry
  points gate: the mint gate before the declaration is recorded, and
  `BindIntakeAdmission` via the same `deriveIntakeAdmission` check. Projects
  are write-once, so no TOCTOU between mint and bind.
- **Confirmed — every occurrence reconstruction re-gates.** Both
  `GetIntakeOccurrence` and `LatestIntakeOccurrence` funnel through
  `scanIntakeOccurrence` → `verifyIntakeAdmission` → `deriveIntakeAdmission`;
  there is no decode path that skips it.
- **Confirmed — the compared values are trustworthy.** `o.RepositoryID` is
  cross-checked against its extracted column (and is baked into the admission
  key), `declaration.ProjectID` is re-derived from the durable declaration
  (not the stored occurrence body), and `GetProject` re-validates its row
  (column vs body) via `scanProject`.
- **Rejected-by-verification — "a tampered projects row could substitute a
  foreign repository."** `scanProject` cross-checks the extracted
  `repository_id` against the body and fails closed (`errRowInconsistent`),
  covered by `TestGetProjectRejectsTamperedRow`.
- **Accepted-by-decision — the residual trust root is registration.** The
  store trusts whoever calls `RegisterProject` to assert the true
  project↔repository binding; that is the definition of an authority root, and
  it is a strict reduction from the prior per-run, unverifiable, repeated
  assumption to one write-once, immutable, auditable row that every run is then
  checked against. #659 registers-or-verifies before minting.

Standard gates all pass (`go build/test/vet ./...`, `golangci-lint`); the full
`go test ./...` is the migration exclusion-list sweep (0045 joins both
`migrationsBeforeX` lists; the checkpoint threshold variant self-adjusts).

Revisit when: onboarding (`freesided onboard`) or `freesided submit` needs to
register projects at a different lifecycle point, or the project concept
arrives as the onboarding trust profile — this entity's name and table are
what onboarding inherits.
