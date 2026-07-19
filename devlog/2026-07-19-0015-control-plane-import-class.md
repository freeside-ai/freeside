# Block the full §5.8 control-plane class during import (#166)

Gauntlet unit, 2026-07-19. #172(#176) delivered the six-member
`ControlPlaneCategory` and `CandidateFinding{Class, Category}` shapes; #177(#184)
widened `domain.ProtectedPathConfig` to seven `Extra*` lists. This unit wires the
importer to that class: it enforces all six §5.8 categories at import, lifts every
importer finding into a `domain.CandidateFinding`, and loads the repo-specific
protected paths from a validated trust profile fail-closed. This note records the
two owner decisions and the returned-object-trust-boundary refute pass; status
lives on #166.

## Owner decisions

- **All six categories enforced at import, including `verification_recipes`.**
  The verify package independently risk-flags a broad verification-steering
  surface (go.mod, Makefile, lint config) at the *verify* stage via its own
  `verification_control_path`. #166's evidence lists "recipe" among the *import*-
  stage gaps, so the importer gains a config-driven `verification_recipes`
  control-plane gate distinct from verify's. A one-line crosswalk comment at
  `WithProtectedPaths` guards against a future maintainer wiring the two together
  (domain's field is named `ExtraVerificationControlPatterns`; its §5.8 category
  is `verification_recipes`). Rejected: leaving verification solely to verify
  (would make "block the full class during import" false).

- **The four newly-covered categories are config-driven only, no universal
  defaults.** `verification_recipes`, `prompts_and_policy`,
  `egress_and_trust_profiles`, and `materiality_rules` have no natural universal
  file locations, so their patterns come solely from the repo's trust-profile
  `ProtectedPathConfig`; `workflow_configuration` and `reviewer_instructions`
  keep their strong universal defaults. This matches #166's required outcome
  ("load the repository-specific protected paths from trusted configuration, fail
  closed if absent/invalid"). Rejected: baking Freeside-specific path guesses
  (`policy/**`, `prompts/**`) into a generic importer (false-flags ordinary
  repos and still misses repo-specific locations). **Consequence (the boundary):**
  a repo with no profile, or an empty widening for one of these four categories,
  gets zero import-stage coverage of it. The fail-closed burden therefore lives
  in `WithProtectedPaths` (an absent/invalid profile errors, never defaults to
  empty) and on the caller always routing imports through it; the widen-only
  invariant holds trivially because the default is empty.

## Design

- **Importer now imports `internal/domain`** (verify already does; domain is the
  base, no cycle). The `FindingKind → CandidateFinding{Class, Category}` lift
  lives on the emitter side (`Finding.Candidate()`), so domain need not enumerate
  importer kinds. The lift's switch omits `default` (exhaustive lint forces a new
  kind to be classed); an unclassed zero kind falls through with an empty Class
  and fails `CandidateFinding.Validate` closed.
- **A single `controlPlaneClasses` table** (one row per §5.8 category: kind,
  category, widen-only pattern accessor) is the one source both `applyPolicy`
  emits from and `categoryFor` resolves categories from, so gate and lift cannot
  disagree. `TestControlPlaneCategoryCoverage` asserts runtime completeness the
  linter cannot see in a table literal. git-metadata stays its own block (a
  `repo_change_policy` finding, not a §5.8 category).
- **Scope held to `daemon/` and read-only against shared packages.** No edit to
  `internal/domain`/`internal/store`; adding a `verification_recipes`-named field
  to `ProtectedPathConfig` to drop the crosswalk would be a contract/encoding-
  version change and belongs in a separate issue.

## Refute-first pass (returned-object trust boundary)

`WithProtectedPaths` trusts fields of a decoded `AutomationTrustProfile`. An
independent refuting lens attacked six claims (fail-closed loading, widen-only,
crosswalk correctness, lift exhaustiveness/correctness, behavior preservation of
the applyPolicy rewrite, alias/case/PathHex evasion). **No confirmed findings.**

- **Confirmed sound:** `profile.Validate()` is the first statement and recomputes
  the content digest *and* validates `ProtectedPaths` before any field is
  trusted; the applyPolicy table loop is byte-identical to the old two-block form
  when no config is present; config-only categories run the same
  `normalizeAliases` + fold match path as the defaulted ones (no evasion
  asymmetry).
- **Accepted by decision (not a defect):** a *fully* attacker-authored profile
  with a self-consistent recomputed digest passes `Validate` — but it can only
  *widen* protections, and binding the profile digest to an *approved* digest is
  the store/publication gate's job, not `WithProtectedPaths`. Out of this unit's
  scope by design.

Codex (independent later lens) then confirmed two real findings the first pass
missed, both fixed in the implementation commit:

- **P1, confirmed:** the lift dropped `Finding.Rule`/`Line` (secret-only fields
  the domain `CandidateFinding` lacks), so two secret matches in one file lifted
  to identical findings that `NewCandidateAuthorization` rejects as duplicates,
  sinking the authorization. Fixed by folding `rule=<id> line=<n>` into `Detail`
  (adding the fields to `CandidateFinding` would be a spine-owned contract
  change). Regression: `TestFindingCandidateLiftSecretsDistinct`.
- **P2, confirmed:** `WithProtectedPaths` assigned the profile's slices directly,
  so the returned `Policy` aliased the profile's backing arrays; an in-place
  mutation after the boundary (a valid glob edit re-runs no digest check) could
  narrow or redirect control-plane coverage. Fixed by `slices.Clone`ing all seven
  lists, matching the domain's own `canonicalize`. Regression:
  `TestWithProtectedPathsSnapshots`.

## Revisit when

- A workflow-engine caller wires imports to a live trust profile: it must route
  every import through `WithProtectedPaths` (a nil/zero profile must error, not
  default to empty), and onboarding should verify a repo's control-plane widening
  is non-empty. That coverage-completeness check is out of scope here.
- `ProtectedPathConfig` gains a `verification_recipes`-named field (a contract
  change): the `ExtraVerificationControlPatterns` → `ExtraVerificationRecipePatterns`
  crosswalk in `trustconfig.go` is retired at that point.
