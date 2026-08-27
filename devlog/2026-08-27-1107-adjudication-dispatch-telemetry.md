# Adjudication Dispatch Telemetry In-Surface Axis (#969)

## Decision

Compute the supervision snapshot's `in_surface` telemetry axis as the
**declared-scope containment half** of the engine's allowed-compatibility
check: the canonical-repository-path gate `EngineCompatibility` applies before
containment (mirrored from `domain.canonicalRepoPath`, unexported and in the
read-only `domain` dependency), then
`pathfold.MatchAny(CanonicalDeclaredPaths(GetResolvedPolicy(runID)),
finding.Location.Path, false)`, failing closed to `false` on an absent
location, a non-canonical path, or a missing resolved policy. It deliberately
does **not** re-derive the tree-existence half (`DeriveRemediationSurface`),
which needs a live worktree a read-only observer cannot reach.

Chose this over surfacing `in_surface` to the owner as an unresolved
contract/owner question (the plan's fallback for its highest-risk item),
because the plan's designated candidate source — the run's resolved-policy
declared scope tested against `Finding.Location` — is reachable and
authoritative from a `*store.ReadTx` (`GetResolvedPolicy` +
`CanonicalDeclaredPaths`), so the plan's own decision procedure resolves to
"proceed," not "stop." Decider: agent judgment under the issue #969 plan
(comment `freeside-implementation-plan:v1`).

Omitting tree-existence is not a fidelity loss but the point of the axis. The
whole calibration question (revision 31's revisit condition) is whether
critical/high severity, material, **in-surface** findings reach model residue rather than the
deterministic engine fast path. A finding whose path is in declared scope but
not yet present in either tree fails the engine's `allowed` check at production
time and becomes `model`/`engine_model` residue; a tree-existence-gated
`in_surface` would read that finding `false` and make exactly the miscalibration
case invisible. Declared-scope containment keeps it visible. `in_surface` is
therefore an independent property of the finding location, never a restatement
of the entry's `allowed` compatibility (which would be redundant with
`producer` and unable to detect residue).

The projection stays `kind:fix`, non-contract: it consumes `domain`/`store`
read-only through the re-gated `ListFindingAdjudications` accessor and adds only
`observedb.AdjudicationDispatch` plus `Snapshot.Adjudications`. All store access
lives in `observedb` (package `observe` may not import `internal/store`); the
new exported names are pinned in `wantSurface`.

## Rejected Alternatives

- **Surface `in_surface` to the owner as an unresolved question.** Rejected:
  the run's resolved-policy declared scope is reachable and authoritative, so
  the plan's candidate (2) applies and no owner decision is needed.
- **Derive `in_surface` from the entry's `allowed` compatibility.** Rejected:
  `engine`/`engine_model` entries always carry `allowed`, so this collapses
  `in_surface` into `producer` and cannot express an in-surface finding that
  fell to `model`/`engine_model` residue — the calibration signal itself.
- **Gate `in_surface` on tree existence for full parity with
  `EngineCompatibility`.** Rejected: the base/candidate trees are unreachable
  to a read-only observer, and even if reachable the gate would hide the
  in-scope-but-absent-path residue case the metric exists to surface.
- **Avoid per-finding fan-out via `ListFindingDispositions`.** Rejected: that
  record carries neither severity nor classifier materiality/confidence, so
  severity comes from `GetFinding` and materiality/confidence from
  `GetClassification(findingID, round)` (classification version == review
  round).

## Revisit when

A digest-keyed resolved-policy accessor exists (today only one resolved policy
per run is reachable, so an artifact's `resolved_policy_digest` that differs
from the run's current policy digest cannot be resolved to its own declared
paths), or a read-only observer gains an authenticated base/candidate tree
handle (then `in_surface` could offer the full tree-existence-gated parity as a
distinct axis), or #397 promotes Claude to a routed reviewer and the calibration
consumer needs a routed-vs-shadow producer split this projection does not carry.
