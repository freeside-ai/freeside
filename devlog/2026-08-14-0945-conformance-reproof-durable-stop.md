# Same-Configuration Conformance Re-Proof Must Not Durable-Stop In-Flight Work

Issue #761. Trust-boundary change (relaxes a conformance re-gate), so
this note records the decisions and the refute-first findings.

## Problem

The daemon's own startup conformance suite writes a `superseded` marker
for the current backend configuration the instant it begins re-proving
(`internal/ward/suite.go`), before the re-proof's `passed` outcome
lands. An already-admitted in-flight unattended invocation then
re-authenticates (`AuthenticateStart` -> store re-gate), sees the marker
as "no current passed record", and the engine's elaboration collector
treated that refusal as fatal -> `exitDurableStop`. Every restart
re-hits the persisted admission: the state root is bricked for
unattended operation (observed on state-482, wave-5 exit run).

## Decisions

**Two conformance gate modes, one shared core, over a single strict
gate** (`internal/store/conformance.go`). The mint gate
(`RequireBackendConformant`, used by `RecordExecutionAdmission`) stays
strict: unadmitted work loses nothing by holding out a recheck window,
so a non-`passed` latest row always refuses. The authenticate gate
(`AuthenticateBackendConformant`, used by the `claude_driver`
re-gate) additionally tolerates a `superseded` latest row whose config
digest equals the admission's bound digest, by recovering the exact
declaration the marker superseded (the row immediately preceding it in
the append-only log) and gating against it only when it both passed and
matches the admission's digest. A same-configuration re-proof
is a proof refresh, not a policy change; a persisted admission must
survive it. Rejected: relaxing the outcome check alone. Non-`passed`
rows carry nil capabilities by domain invariant
(`domain/conformance.go`), so that would silently drop the
`ExcessCapabilities` ceiling. Recovering the superseded `passed`
declaration keeps the ceiling.

**A reconfiguration or a failed proof stays fatal.** The tolerance is
guarded on `latest.Outcome == superseded` AND
`latest.ConfigurationDigest == admission.BackendConfigurationDigest` AND
the declaration the marker superseded having both passed and matched the
admission's digest. Two digest checks are required and distinct: the
marker must re-prove the admission's configuration (a marker for a
different digest is a reconfiguration; the admission's configuration is
no longer current), and the superseded declaration must itself be a pass
for that configuration (an intervening other-configuration proof or a
`failed` proof is not). Both are checked in
`supersededSameConfigurationProof` rather than left to the downstream
mismatch check, so a reconfiguration or rollback refuses cleanly as "no
current passed record" instead of recovering a stale proof and tripping
`ErrAdmissionConfigurationMismatch`.

**Elaboration collect HOLDS on a mutable-policy refusal, over the
issue's suggested fail-per-invocation-with-AttentionItem**
(`internal/engine/elaboration.go`). Chosen by agent judgment; the issue
plan proposed a per-invocation failure and explicitly invited this
veto. Reasons: (1) consistency — the dispatch path (`dispatchHoldReason`),
the production collector (`production_workflow.go`), and elaboration's
own admissibility re-check already HOLD on `MutableAdmissionPolicyRefusal`;
failing only at elaboration collect would be the lone exception. (2)
Correctness — a `failed` re-proof that later passes recovers
automatically under a hold; a per-invocation failure terminally kills a
run the backend would have recovered. (3) Change A already makes the
observed same-config case authenticate, so the run reaches spec-approval;
the hold is only the safety net for genuine (non-same-config) refusals.
Used the broad `MutableAdmissionPolicyRefusal` predicate, not a
conformance-only subset, to match the production collector and fix the
whole class in elaboration. The hold is applied at *both* elaboration
re-gate sites that inspect through the policy-gated driver: the collect
pass and the expiry cancellation (`cancelExpiredElaboration`), which runs
first, so classifying only the collect site would still durable-stop an
expired attempt (Codex finding, below).

## Refute-First Findings

Trust-boundary change, so successive fresh-context reviewers (plus
Codex on the PR) were tasked to disprove the fix. Three blocker findings,
all one root cause: the recovery must return the *exact* declaration the
supersession marker superseded, which in an append-only log is the row
immediately preceding the marker, not any heuristic "newest matching"
row. Each finding tightened the anchor toward that; the terminal fix
recovers the immediately-preceding row
(`latestBackendConformanceBefore`, no outcome or digest filter) and
tolerates only when it is a pass for the admission's digest under a
same-configuration marker. That is correct by construction: whatever was
latest when the marker was written is exactly what it cleared.

- **Finding 1 (refute-first): intervening failure.**
  passed(A) -> superseded(A) -> failed(A) -> superseded(A). A "newest
  same-digest pass" recovery skipped the failure and authenticated
  against an invalidated pass. Pinned by
  `TestAuthenticateRefusesInterveningFailedProof`.
- **Finding 2 (Codex P1): intervening other-configuration pass.**
  passed(A) -> passed(B) -> superseded(A) (roll to B, back to A while the
  A recheck runs). A "newest *completed* proof" recovery, even
  digest-agnostic, could still skip to an older A pass; filtering by
  digest resurrected it outright. Pinned by
  `TestAuthenticateRefusesInterveningOtherConfigurationPass`.
- **Finding 3 (refute-first, on the finding-2 fix): stacked
  cross-configuration markers.** passed(A) -> superseded(B) ->
  superseded(A): the B reconfiguration cleared A and B never completed,
  yet a "newest completed proof before the marker" recovery skipped the
  B marker and resurrected A. The terminal immediately-preceding-row
  anchor refuses (the row before the A marker is the B marker). Pinned by
  `TestAuthenticateRefusesStackedCrossConfigurationMarkers`. Consequence:
  a same-configuration restart-stacked double marker
  (passed(A) -> superseded(A) -> superseded(A)) now also refuses and the
  engine holds the run until the re-proof completes, rather than
  tolerating; conservative, never a fail-open
  (`TestAuthenticateRefusesRestartStackedSupersession`).
- **Finding 4 (Codex P1, on the engine hold): expiry path bypassed the
  hold.** `cancelExpiredElaboration` inspects through the same
  policy-gated driver and runs *before* the collect hold, so an expired
  attempt under a conformance refusal (exactly the restart-stacked state
  the store now refuses) propagated the error and durable-stopped. Fixed:
  the same `MutableAdmissionPolicyRefusal` hold is applied at the expiry
  call site too. Pinned by `TestElaborationExpiryHoldsOnConformanceRefusal`.
  Both engine re-gate sites (collect, expiry) and the admissibility
  re-check now hold; production has no separate pre-collect inspect.
- **MINOR, accepted:** the elaboration hold now also holds (rather than
  durable-stopping) on `ErrConformanceConfigurationUnbound` from a
  malformed/unbound latest row. Narrow and consistent with the dispatch
  path, which already holds on the same sentinel; raw corruption
  (bad JSON, unparseable time, non-positive id) still returns non-mutable
  errors and stays fatal. No change.
- Axes 2 (ceiling), 3 (mint leak), 4 (scan equivalence), 6 (digest guard)
  could not be refuted.

## Accepted Residual Risks

- **Silent hold on a genuine reconfiguration.** A `superseded`-different-digest
  or `failed` refusal at collect now holds the run indefinitely with no
  dedicated AttentionItem. This visibility gap is pre-existing and shared
  with the production collector and dispatch path; solving it belongs in a
  uniform "long-held unattended work" surfacing across both lanes, not an
  asymmetric fail here. Follow-up: #766.

## Verification

Store gate unit tests (tolerate same-config, refuse
different-config/failed/no-prior-pass, ceiling preserved, mint stays
strict); an engine test that reproduces the exact #761 durable-stop
without change B and holds with it. Refute-first findings recorded in
the PR. The `claude_driver` authenticate wiring (a one-line swap to the
tested `AuthenticateBackendConformant`) is not covered by a dedicated
cmd/freesided unit test — it would require replicating the unattended
admission mint fixture that lives in `store_test` — and is covered by
the store method tests plus the live run-real-work acceptance.

## Revisit When

- A backend legitimately needs to re-prove under a *changed*
  configuration while old-config admissions are in flight (today that is
  intentionally fatal).
- The append-only conformance log gains deletion or compaction (the
  "prior passed proof still exists" assumption).
