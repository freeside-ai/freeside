---
run: manual
stage: head-independent-evidence
date: 2026-07-14
branch: feat/head-independent-evidence
---

# Represent head-independent evidence provenance (issue #37)

Spine-role session. #37 is the third and final Track A (`kind:contract`) unit of
the Wave 0 exit-fixes batch (#4), self-selected: predecessors #33 (PR #43) and
#32 (PR #45) merged, no active claim (zero open PRs), and contract serialization
holds because the only other open contract units, #28 and #22, are unmilestoned
deferrals (dormant per the session-start rule). Declared paths:
`daemon/internal/domain`, `daemon/internal/store`, `api`. PR #51.
Cross-component: the api schema and both consumers moved as one unit.

The finding (plan §5.15 rule 2): the plan promises "a remediation head
invalidates prior-head evidence *unless explicitly head-independent*," but that
exception was unrepresentable. `Provenance.SourceHeadSHA` was unconditionally
required non-empty, and `AttentionItem.Validate` rejected any head-mismatched
evidence with no exemption. There was no value meaning "this evidence is
intentionally decoupled from repository head," so the exception could be neither
enforced nor rendered; deferring it would have forced a breaking contract/schema
migration after Phase 1 data exists.

## Decisions

- **Typed mode on `Provenance`, enforced in the domain (the real boundary).**
  New `HeadBinding` enum (`head_bound` / `head_independent`) mirroring the
  established enum idiom (named string type, `All…` slice, `valid()` membership
  switch with default; `errors.go` sentinel). No valid zero value, so an omitted
  or unknown binding is rejected (`ErrInvalidHeadBinding`) — absence is *never*
  silently read as independence, which is the crux of the acceptance. Same
  "rules live with the vocabulary" shape as #33/#32: the store gets it for free
  via `decode`→`Validate` on every reconstructed body.
- **Mode determines the head rule, symmetrically.** `head_bound` requires a
  non-empty `source_head_sha` (the prior rule, now scoped to the mode);
  `head_independent` requires an *empty* one — a head on head-independent
  evidence is a machine-checkable contradiction (`ErrProvenanceInconsistent`,
  reused; same "provenance asserts a falsehood" category as the agent+recipe
  case, no new sentinel). Rejected a one-directional rule (only requiring the
  head for head_bound) because it would let a head-independent artifact carry a
  stale head, reintroducing the ambiguity the mode is meant to remove.
- **`source_head_sha` gains `omitempty`.** Head-independent provenance omits the
  field entirely; head-bound always emits it. The serialized shape is thus a
  deterministic function of the *validated* mode (bound⇒present, independent⇒
  omitted), with no nil/empty ambiguity, and it matches the wire oneOf where
  `source_head_sha` exists only on the head-bound branch. Provenance is not
  content-hashed anywhere (`Artifact.Digest` is supplied, not computed), so this
  does not touch idempotency; the write-once store compares whole-body bytes and
  `json.Marshal` is deterministic.
- **AttentionItem invalidation gated on mode.** The evidence head-equality check
  now fires only for `head_bound` artifacts, so head-independent evidence is
  preserved across any `pr_head_sha`, including a remediation head; head-bound
  evidence still must match. Each artifact's own `Validate` runs *before* the
  item's head check, so a head-independent artifact reaching the check is
  provably headless — the "head_independent-with-a-head reaching the guard" case
  is impossible by construction, not by a second check.
- **No migration.** Evidence/provenance rides the opaque canonical-JSON item
  body (`0002_domain.sql`), so the new field serializes into the existing column
  with no schema change (like #33; `migrations/` stayed out of declared scope).
  No data backfill — there are no persisted rows; Wave 0 exit is deliberately
  the moment to make this breaking representation change, before Phase 1 data.
- **Wire: oneOf discriminator (user choice).** Consulted the user on wire depth
  (as #32 did); they chose the faithful representation over the lightweight
  single-object+enum. `EvidenceProvenance` becomes a `head_binding`-discriminated
  `oneOf` over `HeadBoundProvenance` (requires `source_head_sha`) and
  `HeadIndependentProvenance` (`additionalProperties: false`, no `source_head_sha`
  member), mirroring the existing `Subject` oneOf precedent. This rejects *both*
  ambiguous combos at the wire, not just in the domain. Each branch carries an
  example so vacuum's `oas3-valid-schema-example` exercises both modes; no Go↔
  OpenAPI round-trip harness (matches #32's provisional-API convention).

## Refute-first pass (returned-object-trust / data-integrity boundary)

One fresh-context reviewer, diff + stated intent only, prompted to disprove each
contract point. Ledger:

- **Confirmed sound (no defect):** (1) mode/head consistency and switch
  exhaustiveness — the `valid()` gate runs before the `switch`, so zero/unknown
  bindings are rejected before it, and the no-default switch is exhaustive over
  the two members. (2) Fail-closed on decode — `decode`→`Validate` on both
  `GetArtifact` (+`ValidatePublishEligibility`) and `GetAttentionItem`
  (+`gateEvidence`) paths rejects a legacy/forged headless body; no
  default-to-bound/independent path. (3) AttentionItem guard — per-artifact
  `Validate` precedes the head check, so head-independent evidence is provably
  headless at the guard and exempt under any head, while head-bound mismatch
  still fails. (4) Serialization determinism — `omitempty` shape is a function of
  the validated value; provenance is never hashed; whole-body idempotency
  unaffected. (5) Wire — both ambiguous combos fail under both oneOf and
  discriminator semantics; branch examples each match exactly one branch;
  `additionalProperties:false` asymmetry is intentional and safe.
- **Rejected by verification:** none — no confirmed defect to reject.
- **Accepted by decision (not a defect):** the reviewer's one forward-looking
  note — a *future third* `HeadBinding` member would validate with neither
  head-presence rule firing (the no-default switch would no-op on it). This is
  already mechanically guarded: `exhaustive` is enabled with
  `default-signifies-exhaustive: true` (`daemon/.golangci.yml`), so a new member
  without a switch case fails lint — exactly the forcing function the enums.go
  convention documents. No change made.

## Verification

- `go build ./...`, `go test -race ./...`, `go vet ./...`, `golangci-lint run`
  (0 issues): all green. `TestMigrateFreshAndIdempotent` unaffected (no new
  migration).
- api `vacuum` 100/100 with both discriminator branches exercised by valid
  examples.
- New tests: `TestProvenanceHeadBinding` (head_bound requires a head;
  head_independent forbids one; zero/unknown rejected),
  `TestHeadIndependentEvidenceSurvivesRemediation` (admitted under two differing
  heads; head-bound control still rejected), a store round-trip
  (`TestHeadIndependentEvidenceRoundTrips`), and the `HeadBinding` case in
  `TestEnumValidity`. Domain + store goldens regenerated with a head-independent
  fixture (`head_independent_provenance`/`_artifact`) so both modes are covered.
- Acceptance mapped: (1) typed mode in domain + OpenAPI; (2) head-bound requires
  a head, head-independent defined as marker + no head; (3) AttentionItem
  preserves only explicitly independent evidence across remediation heads; (4)
  golden/validation/OpenAPI tests cover both modes and reject ambiguous combos.

## To promote

- None this session. (This unit *uses and reinforces* the domain enum/switch
  convention — `HeadBinding` adds a third exhaustive-linter-guarded
  behaviour-dispatch switch alongside ProducerClass — but the pattern is already
  documented in the enums.go header and `doc.go`; nothing new to lift.)
- Queue: grepped the open `## To promote` / deferred / `needs-human` queue. Two
  open items — the `approved-recipe-boundary` store trust-boundary invariant
  (`-> open`) and the `domain-package` conventions flagged for Wave 0 exit spine
  review — are both self-marked point-of-use-documented with promotion gated on
  recurrence/spine intent, and promoting either is its own reviewed docs change
  (Document gating), outside this contract unit's declared scope. Neither
  drained, no spurious re-defer; both remain open.
