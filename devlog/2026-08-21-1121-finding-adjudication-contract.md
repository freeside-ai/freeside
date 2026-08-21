# FindingAdjudication contract core (#836)

Contract unit landing the §7 FindingAdjudication artifact, its
adjudication vocabulary, and persistence (domain, migration, store).
Records the load-bearing design choices and two forced deviations from
the implementation plan (#836 comment). Source contract: docs/plan.md §7
Finding Adjudication (revision 31); work contract: issue #836 body.

## Decisions

- **`allowed` unreachable from the model, not just rejected.** Chose a
  distinct `ProposedCompatibility` enum (the four-member subset of
  `WorkUnitCompatibility` minus `allowed`) as the model-entry
  constructor's parameter type, over a single shared enum guarded only
  by a runtime check. `NewModelAdjudicationEntry` takes
  `*ProposedCompatibility`, whose `compatibility()` widening has no
  `allowed` image, so a model-origin `allowed` is structurally
  unrepresentable at the type level. `Validate` still backstops a
  hand-built or decoded model+`allowed` entry
  (`ErrModelEntryMintsAllowed`). The engine's `EngineCompatibility` is
  the sole producer of `allowed`. Rationale: §7 makes model-minted
  permission the central trust-boundary risk; making it a type error is
  stronger than a validation error.

- **Nine-route vocabulary; `contradictory` admits two.** `AdjudicationRoute`
  has one member per §7 table outcome. Eight axis rows map to nine route
  assignments because the `contradictory` row admits `decline` or
  `dispute` (the ceiling rule: critical/high severity or below-threshold
  confidence selects `dispute`). Which one applies is engine ceiling
  logic, not an axis function, so both validate in that row. Rejected:
  splitting `contradictory` into a ceiling-qualified sub-axis, which
  would push engine-run severity logic into the contract vocabulary.

- **Domain confidence ordinal beside `inference.Ordinal`.**
  `AdjudicationConfidence` (low/medium/high) duplicates
  `inference.Ordinal` deliberately: `internal/domain` cannot import
  `internal/inference` (import direction). Any unification is the engine
  unit's work. `DispatchThreshold` (medium/high, default high) is a
  separate bounded-below type; `AdjudicationConfidence.meets` compares
  ordinal ranks.

- **Threshold is a parameter, not a policy key.** `Entry.Accepted` takes
  the `DispatchThreshold` as an argument. No resolved-policy key for the
  dispatch threshold exists at the grounding revision; resolving it is
  the engine unit's work, so this contract adds none.

- **Canonical form includes CreatedAt, excludes only Digest.**
  `ComputeDigest` hashes every semantic field except `Digest` itself,
  per the plan's recommendation: the artifact is per-round identity and
  its creation instant is part of that identity. A same-round re-run with
  a new timestamp is therefore a distinct content address and an
  immutable conflict, not a silent converge.

## Deviations from the plan (both forced by discovered constraints)

- **`EngineCompatibility` injects the containment matcher; `domain` does
  not import `pathfold`.** The plan had `domain` import
  `internal/pathfold` directly. It compiles and the real build passes,
  but `pathfold` transitively imports `golang.org/x/text`, which the
  enum-registration test's ad-hoc type-checker
  (`enum_registration_test.go`, `go/importer.Default()`) cannot resolve
  for a daemon package it source-parses — so `domain` importing
  `pathfold` breaks `TestEnumRegistration`. Resolution: `EngineCompatibility`
  takes a `match func([]string, string) bool` parameter, symmetric with
  `DeriveRemediationSurface`'s injected `resolve`. The engine passes
  `func(p []string, s string) bool { return pathfold.MatchAny(p, s, false) }`,
  so the match is still the importer's exact matcher (no reimplementation,
  no case fold) and `domain` stays a dependency-light leaf. The plan's
  "never reimplement the match" intent is preserved; only the seam moves.

- **`(run_id, round)` is the immutable key / conflict target, not
  `content_digest`.** The plan's migration sketch made `content_digest`
  the PRIMARY KEY with a separate `UNIQUE (run_id, round)`. `putImmutable`
  targets exactly one conflict index; with two unique indexes an identical
  replay conflicts on the non-target index and aborts, and a differing
  body for the same round would surface as a raw SQL constraint error, not
  `ErrImmutableConflict`. Chose `PRIMARY KEY (run_id, round)` as the sole
  conflict target (the natural one-per-round key, matching the acceptance's
  "(run_id, round) ... immutable conflict" wording) with `content_digest`
  as a plain indexed, cross-checked column. This mirrors `PutReviewRecord`
  (natural key = conflict target, digest = integrity column).

## Verification findings

- Finding-set binding is enforced by the store accessor, not a SQL
  trigger. Unlike `0037_finding_dispositions` (one row per finding, so a
  trigger can bind), this artifact embeds entries in the JSON body, which
  a trigger cannot iterate; the composite FK binds only round existence.
  Accessor set-equality against the review record's `finding_ids`
  (missing record / foreign / missing / duplicate → `ErrParentKeyMismatch`)
  is the authoritative binding, re-run on every read (decoded-trust-bit
  rule).
- Refute-first pass results recorded in the PR Verification section.

## Review-driven trust-boundary changes (PR #881, Codex)

Automated review hardened the trust boundary in three ways; the
declined findings are recorded so they are not re-raised.

- **Engine-producer row restriction (confirmed).** `Validate` now
  rejects any engine entry outside the single `required`/`allowed`/
  `remediate` fast-path row (`ErrEngineEntryNonDeterministicRow`),
  symmetric with the model→`allowed` backstop. The no-model fast path
  is one-directional toward remediation (§7); `unknown` under
  `required` is a model *not-accepted* representation, not an engine
  fact, and `adjacent`/`contradictory`/`unclear` are spec-relative
  model residue. `EngineCompatibility`'s `unknown` return is a derived
  compatibility value that fails the finding into that residue, never
  an engine-produced entry. (An interim fix wrongly also admitted
  engine+`unknown`; corrected to `allowed`-only.)
- **Every caller-supplied binding digest re-gated (confirmed).**
  `validateFindingAdjudicationBinding` now also binds the artifact's
  `ApprovedSpecDigest`/`ResolvedPolicyDigest` to the run's authoritative
  `SpecDigest`/`PolicyDigest` (mirrors `requireRecordedAttempt`) and its
  `InstructionSnapshotDigest` to the review round's `InstructionDigest`.
  With the finding-set binding above, all four external trust bits the
  artifact copies are re-gated on write and reconstruction; only the
  self-verified content and finding-batch digests are unbound.
- **Replay reconstructs before the byte compare (confirmed).**
  `PutFindingAdjudication` reconstructs the existing `(run_id, round)`
  row before `putImmutable`, so corruption confined to a copied lookup
  column cannot be hidden by an unchanged canonical body (mirrors
  `PutFindingDisposition`).
- **`Accepted` fails closed (confirmed).** `Accepted` is
  authority-bearing (a true result routes to dispatch), so it now
  re-runs `Validate` first: a caller-supplied or decoded entry that
  bypassed a constructor — a model-minted `allowed` with meeting
  confidence, a forged engine row, an out-of-scale confidence — errors
  instead of being accepted. A validated model entry always carries a
  valid confidence, so the ordinal comparison stays total and §7's
  below-threshold not-accepted semantics are preserved.
- **Remediation-surface provenance is opaque (confirmed).** A follow-up
  finding showed that the exported `RemediationSurface.Path` let a future
  engine caller bypass `DeriveRemediationSurface` and mint `allowed` for an
  unresolved or noncanonical path. The earlier trust-boundary sweep followed
  forged authority downstream into entry validation and persistence, but
  missed the intermediate value that carried derivation provenance. The
  surface now has private path/provenance state, only derivation can produce a
  usable value, and `EngineCompatibility` rejects the externally constructible
  zero value before calling the matcher. This keeps canonical-path and
  base/candidate-tree existence checks at the seam that has those inputs.
- **Declined — store-side `allowed` containment re-derivation.** Persistence
  cannot rerun `EngineCompatibility` or `DeriveRemediationSurface`: it has
  neither the bound base/candidate git trees needed for existence resolution
  nor the importer's `pathfold` matcher, and acquiring workspace access would
  violate the store boundary. The confirmed opaque-surface change closes the
  caller-supplied provenance gap before an engine entry is minted; the future
  engine adjudication site must consume that capability, while the store keeps
  re-gating the authoritative IDs and digests it can reconstruct.
- **Declined — severity ceiling in persistence.** A finding asked the
  store to re-gate `contradictory`/`decline` and `adjacent`/`defer`
  against finding severity plus second-adjudication/attention evidence.
  Declined: the decline-vs-dispute ceiling is engine dispatch (the
  nine-route decision above keeps it out of the contract vocabulary),
  and that evidence is a stated non-goal of this unit.
- **Declined — multiple adjudications per round.** A finding asked for
  a non-`(run_id, round)` key so a re-adjudication could store a second
  artifact for the same round. Declined: §7 produces one artifact per
  round and the `(run_id, round)` immutable-key deviation above is a
  recorded owner decision; re-adjudication advances the round (or
  replans the run), which is engine-loop dispatch, a non-goal.
- **Declined — per-entry classification-version binding.** A finding
  asked each entry to record and re-gate the finding's classification
  version. Declined: §7 enumerates what each entry records (the two
  axes, route, rationale, evidence, cited rules, assumptions,
  alternatives, open questions) and what the artifact binds (run,
  finding batch, round, spec, instruction, policy digests); the
  versioned classification is an adjudication *input*, not an
  enumerated artifact or per-entry binding, and the version in force is
  reconstructable from the round and finding. Adding a field plus a
  digest, golden, and store re-gate is a contract and scope expansion
  beyond §7's stated artifact. A future forensic-traceability
  enhancement could revisit it as an owner-gated decision.

## Revisit when

- `internal/inference.Ordinal` is moved into `internal/domain` (then fold
  `AdjudicationConfidence` into it).
- A resolved-policy key for the dispatch threshold lands (then wire
  `Accepted`'s threshold from policy in the engine unit).
- `internal/domain/finding.go` changes `Finding.Location` or the
  fingerprint identity this artifact's batch binds.
