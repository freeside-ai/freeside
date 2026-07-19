# Bind publication to a current automation trust profile (#169)

Publish lane, 2026-07-18. Consumer of the #172 trust-profile/authorization
contract (merged as #176). Adds the §5.5 fail-closed drift gate to
publication; picked up by fiat (direct assignment), not a scheduling sweep.
Status lives on #169/its PR; this records the decisions and the
refute-first results.

## Scope decisions

- **In-lane comparison now; live-audit producer deferred (#182).** The
  literal issue text says "re-read/re-audit at the decision point," but no
  live-audit data source exists: `WorkflowAudit` is only ever constructed in
  tests and the publish forge client exposes ref/PR operations only (no
  GitHub API for token permissions, OIDC, branch protection, rulesets).
  Building that producer is a cross-lane, contract-scale surface (new App
  scopes, domain/store/api). #169 therefore ships the *comparison* half:
  re-read the persisted profile + latest audit and fail closed. This matches
  how #172 designed `WorkflowAudit` ("the drift comparison happens at the
  publication decision point, which consumes these rows"). The producer and
  the explicit allow-axes below are filed as #182. Rejected: building the
  live auditor inside #169 (out of lane, contract-scale, several units).

- **Comparator in `domain`, declared scope `domain` + `publish`.**
  `EvaluateTrustDrift` is a pure predicate over existing types, placed next
  to them mirroring `domain.EligibleForEvidenceSnapshot` (the artifact
  re-gate publish already calls). It adds no persisted type, field,
  migration, or interface, so it is the enforcement the #172 note delegated
  to consumers, not a contract-shape change. Rejected: housing domain trust
  logic inside `publish` to stay in one lane path (diverges from the
  `EligibleForEvidenceSnapshot` precedent).

- **Port, not parameter, for current trust state (reversed the plan's
  wiring choice).** The approved plan threaded current trust through
  `Publish` as a parameter, mirroring `approvedRecipes`, on the stated
  worry that a store read inside `Publish` would nest inside the drain's
  read transaction. On inspection that worry is moot: the drain's `s.Read`
  closes before the `Publish` loop. And `Publisher` already depends on a
  store-backed *port* (`IntentLedger`/`StoreLedger`, faked by `memoryLedger`
  in unit tests), so a `TrustSource` port is the identical idiomatic shape,
  not a new coupling. Current trust is store state (latest recorded profile
  + audit rows), unlike `approvedRecipes` which is engine/workflow-held
  state the resolver reconstructs, so a store-backed port models it more
  accurately and needs no `CandidateResolver` change. Net: `Publish`'s
  signature and ~45 call sites stay untouched; the gate re-reads on every
  call and so covers the recovery drain automatically.

- **Nil binding and superseded binding both fail closed.** A candidate with
  no `TrustProfileDigest` cannot be proven drift-free, so it fails closed; a
  candidate bound to a profile that is no longer the current recorded one
  (a §5.5 drift-recovery re-approval superseded it) fails closed until
  re-authorized. The mandatory *authorizing-record* requirement
  (`AuthorizesPublication`, findings-free) is the orthogonal #168 gate,
  serialized after this; #169 owns only the profile-drift axis.

- **Bound digest is a lookup key, never a verdict (#52).** The gate re-reads
  and re-`Validate`s the current profile from the store; `c.TrustProfileDigest`
  is only compared for equality against it. A forged or stale pointer cannot
  buy a favorable outcome.

## Refute-first pass (returned-object trust boundary + safety policy)

A fresh-context reviewer was told to disprove the gate: find a drift that
slips through, a token-comparison hole, a bound-digest forge, an ordering
leak, a digest-coverage gap, and nil slips.

**Confirmed and fixed (folded into the domain commit):**

- **Three authority axes slipped through.** `EvaluateTrustDrift` compared
  only the five privileges the profile has an `Allow*` axis for; the audit's
  `ReusableWorkflows`, `PackagePublishing`, and `ArtifactConsumers` bits
  were never compared. The original code leaned on the audited-surface
  digest to cover them, but `WorkflowAudit` is not self-certifying (its
  digest is not recomputed from the attested facts, per #172) and the digest
  is documented as covering the *file* surface, so a settings-derived
  privilege can drift with the digest unchanged. Fix: compare every attested
  privilege explicitly; the three with no profile axis fail closed whenever
  observed (no approval axis = not approved = drift). Explicit allow-axes to
  make them approvable are filed as #182.

**Accepted by decision (not #169 bugs):**

- **`WorkflowAudit` is not self-certifying.** An explicit #172 decision (the
  audit is a trusted daemon observation, not a self-authenticating record).
  #169's gate is stricter than pure digest reliance (it compares every axis
  explicitly), so it does not newly depend on the non-self-certifying
  digest for the security-critical privileges. Revisiting self-certification
  is a #172/#182 contract question, not this unit's.

- **The audit is not bound to the candidate head/base; "latest recorded"
  wins.** Automation authority is repository-level, not per-commit, so the
  gate compares the current repo posture, not a per-head audit. Freshness at
  the decision point (a stale conformant audit must not mask a drift that a
  fresh audit would show) is the *producer's* responsibility, deferred to
  #182 — the "re-audit at the decision point" the issue asks for is the
  producer recording a current row; #169 reads the latest available.

- **`PRExecution` / `CandidateAutomationChanges` have no attested audit
  counterpart.** They are caught only via profile supersession (a changed
  profile mints a new digest, failing the binding); a reality drift with no
  new profile is invisible. A contract limitation for #182 to consider, not
  a code bug here.

**Refuted under attack (defenses hold, demonstrated by tests):** the
token-permission "exceeds" condition is complete (`Validate` rejects the
empty/invalid enum before comparison, so `read_write` under `read_only` is
the only drift); a forged or superseded bound digest fails closed; the gate
runs before `DeriveIdentity`, `recordIntent`, and every forge call, and the
recovery drain routes through `Publish` so it is gated too; nil profile, nil
audit, and a trust-source read error all fail closed.

## Automated review (Codex)

- **Re-approving an exact prior profile is not selectable as current
  (P2) — accepted by decision, routed to #182.** With profiles A then B
  recorded, re-approving byte-identical A is a store no-op
  (`RecordTrustProfile` is write-once by `profile_digest`,
  `ON CONFLICT DO NOTHING`), so `StoreTrustSource`'s "latest recorded row is
  current" selection keeps B current and a candidate bound to the
  re-approved A fails closed until the profile content differs enough to mint
  a new digest. Confirmed and real, but (a) it fails *closed* — a legitimate
  publication is blocked, never a drifted one allowed, so it is not a
  security regression — and (b) the only correct fix is an approval/revision
  event or an explicit active-profile pointer in the store, a #172 contract
  change outside this unit's `domain`+`publish` lane (a publish-only
  workaround that treated any historically recorded digest as current would
  defeat drift detection). Added to #182's scope; declined here.

- **Recovery does not pin the committed trust binding (P2) — accepted by
  decision, routed to #168.** The durable outbox `Intent` carries identity,
  invocation, repo, base, and head, but not the bound trust digest, so a
  publication that commits its intent under profile A and crashes before the
  GitHub effect could, on the recovery drain, be re-converged under a
  *different* current profile B if the resolver reconstructs the head with a
  new binding. This is **not a drift bypass**: `gateTrustDrift` runs inside
  `Publish`, which the drain calls, so publication never proceeds under a
  *drifted* profile — B must itself be current and non-drifted. It is a
  recovery-*provenance* point (the committed decision under A vs a new
  decision under B, both valid). The complete fix is pinning the committed
  *authorization* (which binds the trust digest) in the durable intent and
  failing closed on divergence at drain, mirroring the existing invocation/
  identity divergence checks. That belongs with #168, which introduces the
  `AuthorizationID` enforcement and owns the recovery/authorization seam; a
  trust-digest-only pin here would be a partial version #168 reworks. No
  production exposure: nothing wires the engine to publish until Wave 2, and
  #168 is serialized immediately after this unit. Routed to #168; declined
  here.

- **The trust check is not held atomically across the external effect
  (P2, TOCTOU) — accepted by decision, routed to #182.** `gateTrustDrift`
  reads current trust in its own transaction and returns; `recordIntent` and
  the GitHub writes follow, so a profile revision or fresh audit recorded
  concurrently in that window is not observed by the in-flight publication.
  It fails safe (the gate reflects trust current at the decision point, and a
  later drift is re-caught on the next publish/re-audit) and is inherent: no
  SQLite trust read can be atomic with a GitHub API write, the same
  SQLite-vs-GitHub boundary `recover.go` already documents for the
  effect/acceptance gap. The atomic fix is to compose the trust read and
  intent write into one transaction — the Wave 2 engine-composed publication
  transaction the `IntentLedger`/`StoreLedger` docs reserve; a lease in the
  pre-Wave-2 standalone path would still not span the external effect. No
  exposure today: no live audit producer exists, profile revisions are
  human-paced, and the check→intent window is local computation. Routed to
  #182.

## Review convergence

Codex raised three P2s across four rounds, all in the trust-lifecycle
*atomicity/provenance* area beyond this unit's drift-gate remit: store
re-approval selection (→ #182), recovery-intent pinning (→ #168), and the
check-to-effect TOCTOU (→ #182). Each was confirmed valid, each fails closed
or fails safe (none is a drift bypass — `gateTrustDrift` always checks
current trust at the decision point), and each is owned by the unit or wave
that introduces the surrounding mechanism (the store contract, the
authorization binding, the Wave 2 engine-composed transaction). This unit's
own remit — a fail-closed drift comparison against current trust at the
publication decision point — is complete and verified; the architectural
hardening is deliberately routed, not folded in.

## Revisit when

- #182 lands the live producer and the explicit allow-axes: the three
  always-fail-closed privileges become approvable, and the gate consumes a
  fresh audit rather than the latest recorded one.
- #168 wires the mandatory authorizing-record gate on the same publish path.
