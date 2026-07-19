# Publication consumes the candidate authorization (#168)

Publish-lane fix unit #168, claimed by fiat 2026-07-19 (the publish item
in Parallel Pass B of the #83 "Finding remediation lanes" comment).
Dependencies #166, #169, #172 merged. Wires the fail-closed gate that
consumes the `AuthorizesPublication` bit #172 defined: before this,
`Publisher.Publish` re-gated artifact provenance and trust-profile drift
(#169) but never checked authorization, so a candidate that failed
verification or carried publish-blocking importer/verifier findings could
still create a branch and PR.

Scope: `daemon/internal/publish` and the new test-only
`daemon/internal/integration`. Assembling the authorization from importer
and verify results is the Wave-2 engine's (spine) job; this unit is the
consuming gate only.

## Decisions

- **Consuming gate, not an authorizer.** `publish.Candidate` already
  carried `AuthorizationID` "not yet enforced" (#172's #f1a68ff). The gate
  resolves it through a new store-backed `AuthorizationSource` port
  (mirroring `TrustSource`), re-`Validate()`s the decoded record, binds it
  to the candidate, and requires `AuthorizesPublication`. No production
  code composes `importer.Result` + `verify.Result` into an authorization
  yet, and building that authorizer would reach across lane boundaries
  into engine territory; it stays Wave-2. Rejected: adding a
  `verify.Finding.Candidate()` lift (gauntlet-owned) here — out of scope;
  the integration test supplies verify-origin findings directly.
- **Resolve by content id (`GetCandidateAuthorization`), not by head
  (`ListCandidateAuthorizations`).** The candidate carries the exact
  authorization id as its lookup coordinate, the way it carries the
  trust-profile digest for the drift gate. Profile *currency* is already
  the drift gate's concern; the immutable authorization bound to the
  now-current profile is the right record, so a by-id resolve plus the
  binding cross-check is sufficient and avoids re-deciding "which record
  is current" in two places.
- **Binding cross-check is head + recipe + repo + trust-profile; not base,
  not invocation.** An authorization id is a content address over one
  candidate's facts, so an id resolving to a record for a different head,
  recipe, repository, or trust profile must not authorize this candidate.
  Base is deliberately excluded: the record binds `BaseSHA` (a commit)
  while the candidate carries `BaseRef` (a branch) — distinct coordinates
  the identity derivation already pins. **Invocation is excluded, a
  correction to the plan:** the authorization's `InvocationID` is the
  *producing* (verification) invocation, keyed so a re-run mints a
  distinct record; the candidate's `InvocationID` is the *publishing*
  invocation (the outbox attempt axis). They are different axes, so
  equating them would reject every legitimate publication.
- **Gate placement: after the trust-drift gate, before `recordIntent`.**
  Same rule the #169 comment states — an unauthorized candidate must
  commit no outbox intent and touch no GitHub resource. A `Validate()`
  failure surfaces the domain error itself (e.g.
  `ErrAuthorizationInconsistent`); the gate's own fail-closed cases (nil
  id, not found, binding mismatch, unauthorized bit) share the new
  `ErrUnauthorizedPublication` sentinel, mirroring how `gateTrustDrift`
  surfaces `Validate` errors directly but wraps its own cases as
  `ErrTrustProfileDrift`.
- **Pinned `AuthorizationID` in the outbox `Intent` (Codex follow-up on
  #168).** The derived identity excludes the authorization binding, so a
  crash after intent-commit but before the GitHub effect could, on
  `DrainPendingPublications`, re-converge under a *different* current
  authorization if the resolver reconstructed the same head with a new one
  — the invocation and identity divergence checks would both pass. Intent
  now carries the committed `AuthorizationID` (validated as a sha256
  digest on both sides of the ledger), and the drain fails closed on
  divergence, mirroring `errPublicationIntentDiverged`, so recovery
  reproduces the committed decision. Publish's gate still re-checks the
  record is current and authorizing. Rejected (by #169): a
  trust-digest-only partial pin — churn this unit reworks.

## Refute-first verification (returned-object-trust boundary)

The gate trusts fields of a value handed back by the store (a decoded
authorization). Adversarial lenses, each written as a test that tries to
publish something it should not:

- **Forged trust bit** — a truthfully-failed authorization with
  `AuthorizesPublication` flipped to true post-construction. *Confirmed
  closed:* `Validate()` recomputes the bit from the bound facts and
  returns `ErrAuthorizationInconsistent`; nothing recorded or dispatched.
- **Valid authorization for a different candidate** — an authorizing
  record whose head / recipe / trust-profile differs from the candidate.
  *Confirmed closed:* binding cross-check rejects each with
  `ErrUnauthorizedPublication` before any effect.
- **Every publish-blocking finding class** (secret, repo-change-policy,
  automation-control, reviewer-instruction, control-plane,
  verification-control) and a failed verification. *Confirmed closed:*
  `AuthorizesPublication` is false; gate refuses.
- **Drain retarget** — a resolver returning the same identity/invocation
  but a candidate bound to a different authorization. *Confirmed closed:*
  the drain's authorization-axis check refuses, row stays pending.
- **Source read failure / missing record / nil id.** *Confirmed closed:*
  all fail closed with no effect.
- **Waivable acceptance** — a `repo_change_policy` finding with a valid
  non-agent `WaiverRecord`. *Accepted by design:* authorizes and
  converges (§5.12); non-waivable classes remain unrepresentable as
  waived (domain `Waivable()`), so no test can construct a waived
  control-plane/secret/integrity finding.

Cross-package proof: `daemon/internal/integration` runs the real importer
over a workspace adding a §5.8 control-plane path, lifts the real findings
into the authorization, and asserts the real publisher refuses before a
fake forge (which fails the test on any request) sees a single call —
closing the "no composed importer→publication path" gap the issue named.

## Revisit when

The Wave-2 engine gains the authorizer that assembles a
`CandidateAuthorization` from live `importer.Result` + `verify.Result`:
the integration test's directly-supplied verify finding should then be
produced by a real verify run, and the engine composes the outbox-intent
write into the same transaction (the `IntentLedger` production form).
