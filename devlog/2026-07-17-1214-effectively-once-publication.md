# Effectively-once candidate publication with kill tests (#82)

Publish lane, Phase 1A.1 exit behavior. Builds the durable, restart-
surviving wiring and the permanent kill-test matrix on top of #81's
effectively-once mechanism. Scope: `daemon/internal/publish` only; no
shared-package edits (the store is imported and its existing surfaces
called, never changed).

## Narrowed a prior owner decision (#81's Wave-2 deferral)

The #81 note (`2026-07-16-1622-publication-identities.md`, "Out-of-scope
residuals") deferred the **store-backed IntentLedger adapter and its
transaction placement** to "Wave 2 engine assembly". #82's acceptance
needs durable state that survives a restart, so a store-backed ledger is
required *now*.

Resolution, chosen with the owner (this session): split the deferral
rather than honor or overturn it whole. Build the **standalone-transaction
adapter** (`StoreLedger`, mirroring the `StoreRecorder` audit precedent)
in #82; defer only the **engine-composed transaction placement** — the
intent write riding the same `Write` transaction that commits the
workflow decision the effect belongs to (§5.14) — to Wave 2. That
composition genuinely needs the engine (it owns the decision
transaction); the kill tests do not. What changed since #81: the residual
conflated two separable things (a store adapter vs. its engine-side
transaction composition), and #82's kill-test acceptance forces the first
without the second.

Revisit when: the Wave 2 engine lands. Its `Publish`/drain call must move
onto the engine's decision `Write` transaction, and `StoreLedger`'s
standalone `WriteInternal` becomes the fallback recovery-scan path, not
the primary publication path.

## Outcome recording: existing inbox, not a new table

Chose the store **inbox** surface (`RecordInbox`) over a new `publications`
table for acceptance 3 ("identity, head SHA, evidence-eligibility recorded
as one transactional outcome"). A new table would be a spine-owned
`kind:contract` migration and would contradict #82's declared "no
shared-package edits". The inbox is the store's generic externally-
triggered-intake dedup surface; a converged publication's outcome is
exactly that. Keyed by `OutcomeKey` = the **full** identity digest (not the
16-hex branch prefix), so two identities sharing a branch prefix cannot
alias one outcome row — the same reason the PR marker carries the full
digest. The finalize (`MarkOutboxDispatched` + `RecordInbox`) rides one
`WriteInternal`, so the two commit as one transaction.

Two properties the record must hold, both surfaced by review (Codex P2s)
and now enforced: the key is **namespaced** (`publish.outcome/<full
digest>`), because the inbox is unique by idempotency key alone and a
bare digest could collide with another inbox kind; and the outcome is
**deterministic per identity** — every field is a pure function of the
identity and its one converged PR, with no attempt-axis field. Dropping
the invocation ID (which acceptance 3 does not require; it lives on the
outbox intent) is what lets a §5.9 operator re-run or crash-recovery
re-drive under a *fresh* invocation produce a byte-identical outcome and
converge on the one row instead of conflicting on it. The finalize then
**verifies** the returned inbox row (a returned-object boundary): on a
pre-existing key it must be byte-identical to this outcome, else the
finalize fails closed and leaves the intent pending rather than
dispatching it with no valid outcome recorded.

Rejected: a dedicated table (heavier, cross-lane serialization, breaks the
scope constraint); recording the outcome only as the dispatched outbox row
(loses the PR number and eligibility verdict acceptance 3 names).

## Recovery candidate reconstruction: the CandidateResolver seam

The outbox `Intent` carries only identity coordinates, not the artifacts
or PR prose a re-converge needs, so a store-only scan cannot rebuild a
candidate. Chose to mirror signet's `DispatchPendingInvocations`, which
defers full request reconstruction (its empty `StartSpec`) to Wave 2: the
drain asks a `CandidateResolver` for the rest — the Wave 2 engine in
production (reloading from workflow state), the kill-test harness standing
in for it across the simulated restart. This unit owns the scan,
trust-boundary re-check, idempotent re-converge, and atomic finalize; the
engine owns candidate reconstruction.

## Refute-first verification (returned-object trust boundary)

The drain trusts a returned GitHub object set (branch/PR responses,
already gated inside `Publish`) and a returned resolver value, so the
refute-first pass is mandatory. Findings, all **confirmed** and now
covered by permanent tests:

- **Resolver substitution (zero-effect), both axes.** A resolver
  returning the wrong candidate must produce no external effect, and the
  match must cover *both* identity-bearing axes. Content axis: first
  implementation checked identity *after* `Publish`, which would already
  have created the wrong branch/PR; fixed by re-deriving the resolved
  candidate's identity *before* `Publish` (`deriveCandidateIdentity`) and
  refusing on mismatch. Attempt axis (Codex P2, confirmed): the content
  identity excludes the invocation, so a candidate with the right content
  but a different `InvocationID` passed the identity check yet made
  `Publish` record a *second* outbox row under the resolver's key,
  leaving the original intent to re-drive forever; fixed by also checking
  `cand.InvocationID == intent.InvocationID` before `Publish`. Both
  refusals are zero-effect. Tests: `TestDrainRejectsDivergedResolver`,
  `TestDrainRejectsInvocationMismatch`.
- **Re-gate drift / eligibility provenance.** A recipe un-approved by
  drain time must make the publish re-gate fail closed with no
  false-eligible outcome and the intent left pending. `EvidenceEligible`
  is set from `Publish` success (which fails closed on any ineligible
  artifact), never hardcoded. Test: `TestDrainReGateDriftLeavesPending`.
- **Corrupt/foreign outbox row.** A row whose payload names a different
  invocation than its idempotency key stays pending as loud evidence,
  never reaching GitHub (mirrors signet's corrupt-intent test). Test:
  `TestDrainRejectsCorruptIntent`.
- **Foreign inbox outcome row (Codex P2, confirmed).** The inbox is
  unique by key alone, so a pre-existing row under the outcome key would
  let the finalize mark the intent dispatched with no valid outcome
  recorded. Fixed by namespacing the key and verifying the returned row
  is byte-identical before commit; a different record fails closed and
  leaves the intent pending. Test: `TestDrainRefusesForeignOutcomeRow`.
- **Returned queue-row kind confusion (self-review, confirmed).** The
  inbox and outbox are each unique by idempotency key alone. The first
  returned-row fix compared only payload bytes, so a foreign inbox row
  could still occupy the outcome key under another kind while copying the
  expected payload; similarly, `StoreLedger` returned an existing outbox
  payload without verifying the row's kind, allowing publication with no
  recoverable `publish.publication` intent. The widened class is any
  returned queue row whose key, kind, and payload jointly carry the
  durable claim. Both adapters now verify that tuple and fail closed.
  Tests: `TestDrainRefusesForeignOutcomeKind`,
  `TestStoreLedgerRejectsForeignKind`.
- **Decoded outcome semantic drift (self-review, confirmed).** Structural
  JSON validation alone accepted a branch that did not match the recorded
  identity, or an ineligible verdict on a record that by definition exists
  only after the publish evidence gate passed. `Outcome.Validate` now
  re-derives the deterministic branch and requires the successful gate
  verdict. Covered by `TestOutcomeValidation`.
- **Live mint/token repo name (Codex P2 ×2, confirmed).** The minter and
  token cache grant by the *bare* repository name (`forge.do` keys
  `Token` by `repoRef.name`), but the publish test needs
  `FREESIDE_PUBLISH_LIVE_REPO` as `owner/name` for `Candidate.Repo`, so
  no single env value let both live tests pass: cleanup and the
  pre-existing mint test each passed `owner/name` to a mint/token call
  and would fail grant validation (leaving the branch/PR uncleaned). Swept
  the class: `FREESIDE_PUBLISH_LIVE_REPO` is `owner/name`, and every
  direct mint/token call extracts the bare name (`bareRepoName`), while
  REST URLs keep `owner/name`. (Opt-in path; not CI-exercised.)
- **Live cleanup false success (self-review, confirmed).** `http.Client.Do`
  treats HTTP 4xx/5xx as successful transport, and the cleanup helper
  discarded response status, so it could silently leave the opt-in test's
  PR or branch behind. Cleanup now validates the exact success status for
  both operations and validates the configured repository as exactly
  `owner/name`, while remaining best-effort and logging failures.
- **Live cleanup after partial publish (Codex P2, confirmed).** Cleanup
  was registered only after `Publish` returned success, but a publish can
  create its deterministic branch or PR and then fail a later request or
  returned-object validation without returning the created PR number.
  The test now derives the identity and registers cleanup before Publish;
  when no result was returned, cleanup discovers an open PR by the unique
  `owner:branch` head, closes it if present, then deletes the branch.
  Regression: `TestCleanupLivePublicationDiscoversPartialPR`.
- **Effect/finalize non-atomicity.** The GitHub effect and the SQLite
  finalize cannot share a transaction; that gap is the
  after-effect-before-acceptance boundary, closed by idempotent
  re-converge (no second write), asserted by a write-count delta. Test:
  `TestKillAfterPublishBeforeAcceptance`.

Rejected-by-verification: none (no finding was dismissed as spurious).
Accepted-by-decision: the derivation in `deriveCandidateIdentity` mirrors
`Publisher.Publish`'s input construction and could drift from it; accepted
with a pinning test (`TestPublishIdentityMatchesDerivation`) rather than a
`Publish` refactor, since refactoring the shared #81 trust-gate loop is
out of this unit's scope.

## Acceptance 4 (cross-lane demo) left to wave exit

The end-to-end fake-candidate run (gauntlet → verify → publish) needs the
Wave 2 engine to wire the lanes; there is no cross-lane engine in Wave 1.
This unit ships the publish-side drain + resolver seam and its kill tests;
the cross-lane demonstration is the 1A.1 wave-exit integration
(spine-owned), recorded on #83 when the lanes converge.
