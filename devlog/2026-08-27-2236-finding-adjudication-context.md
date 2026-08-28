# Projected Finding Context Bound to Its Immutable Sources (#892)

## Decision

Give `FindingAdjudicationProposal` three projected fields — `FindingMessage`
(whitespace-normalized), `FindingLocation` (nullable), and `Evidence` — and
authenticate each against the record it copies from. The message and location
have no `FindingAdjudicationEntry` counterpart, so the store re-gate joins the
immutable stored `Finding` (`GetFinding`) and compares the message through the
one shared `NormalizeFindingMessage` derivation and the location by value; the
evidence rides the existing per-proposal comparison against the digest-bound
artifact entry. The engine producer projects all three at both item-creation
sites, loading the findings through the caller's own transaction handle.

The item-level comparison only proves a proposal repeats its artifact entry;
it cannot prove the entry's own evidence is genuine. For the engine producer
specifically, the artifact-level binding (`validateFindingAdjudicationBinding`,
on every put and every reconstruction) independently re-derives the fast
path's one production invariant — evidence is the named finding's own
containment location, and nothing else — against the immutable stored
`Finding`, so a hand-built or decoded engine entry cannot carry fabricated
evidence through to the card's daemon-fact register (#984 review).

The authority is the daemon-authored source, not the item creator. A message,
location, or evidence line introduced or rewritten only in the item payload
fails closed with `ErrParentKeyMismatch` on the write and on every actionable
snapshot reconstruction; the immutable-history record path stays un-gated. The
card renders the message and location in the solid daemon-fact register.
Rationale and evidence render inside each proposal's own producer register
instead — solid for the engine fast path, dashed for a model-backed producer —
so model free-text is never presented as a daemon-verified fact, and the
engine fast path's daemon-derived containment evidence is never presented as
unverified model output (#984 review).

## Rejected Alternatives

- **Reject an empty `FindingMessage` in domain validation (the plan's
  "message present").** Rejected because an unfingerprintable finding — one with
  an empty normalized message — is a handled case elsewhere (it is carried, not
  refused; `review_yield` counts it as new), and review sources do not guarantee
  a non-empty message. A hard domain check would turn such a finding into a
  deterministic item-creation failure that wedges the adjudication reconcile.
  The store re-gate compares the projected message against
  `NormalizeFindingMessage(Finding.Message)`, so an empty message matches an
  empty stored message and tampering still fails closed; the store is the sole
  authority on the copied content, and domain validation keeps only the
  structural checks (location well-formed when present, message valid UTF-8).
- **Carry the message and location on the artifact entry instead of joining the
  `Finding`.** Rejected as out of scope and wrong-sourced: the #836 artifact
  encoding is frozen for this unit, and the message and location are
  daemon-authenticated finding coordinates, not model residue. Joining the
  immutable `Finding` at the re-gate keeps them bound to their true source
  without a second copy or an encoding-version bump.
- **Render the finding context in the model register.** Rejected. The message
  and location are daemon-authenticated, so they belong in the solid
  daemon-fact register, distinct from the model's evidence.
- **Leave the finding context in `.factBlock`, after the action region, since
  the shared `actionInsertionIndex = 1` composition puts only the
  recommendation module ahead of actions (the original form of this
  decision).** Rejected on review (Codex, PR #984): the §9 `finding_adjudication`
  row leads with *two* things, "the recommended route and why, as a labeled
  proposal" and "the finding and the daemon's binding and containment facts in
  a separate register" — both belong ahead of the action region, not only the
  recommendation. On the linear layout (iOS and compact macOS) the shared
  composition inserted actions right after the recommendation module, so an
  operator could reach "Accept recommended route" before the finding message,
  location, or daemon facts ever rendered. `finding_adjudication` now gets its
  own `DecisionCardComposition` (a new `.findingFacts` module, `modules:
  [.recommendation, .findingFacts, .factBlock, .claims, .evidence, .details]`,
  `actionInsertionIndex: 2`): the labeled proposal and the daemon-fact register
  (split out of the former single `findingAdjudication` renderer as
  `findingAdjudicationLead`) render ahead of actions on every layout, while
  assumptions, cited rules, alternatives, and gating questions
  (`findingAdjudicationDetail`) stay in `.factBlock`, after actions, matching
  §9's "Below" column. This changes only `finding_adjudication`'s composition;
  the eight other types sharing the prior arm are unaffected.
- **Backfill or gate-exempt an item body written before this contract (a
  legacy row decoding with an empty `finding_message` and a nil
  `finding_location`).** Declined on review (Codex, PR #984): the immediately
  preceding #893/#976 unit already bumped `FindingAdjudicationEncodingVersion`
  to 2 on this same artifact type and recorded that "a version-1 body no
  longer decodes, which is a clean break for this pre-release artifact type
  rather than a graceful upgrade" (`daemon/internal/domain/finding_adjudication.go`).
  This contract's new `finding_message`/`finding_location` fields are the same
  class of change to the same pre-release surface, so the store re-gate
  correctly fails a legacy body closed rather than trusting its
  unauthenticated zero-valued fields; see Revisit When.
- **Leave `daemon/internal/signet/sync.go` untouched (the plan's scope).**
  Rejected. The sync normalizer already renders every sibling proposal array
  (`cited_rules`, `assumptions`, `open_questions`, `offered_alternatives`) as an
  empty array rather than null; the new `evidence` array must join them or it
  would serialize as `null` against an always-present array contract. This is
  the new field's serialization completeness, so it moves with the field.

## Refute-First Verification

The returned-object trust boundary is the decoded item's projected finding
context measured against the stored `Finding` and the digest-bound artifact
entry.

- **Confirmed and fixed:** the shared `gateFindingAdjudicationItem` rejects a
  message that differs from the normalized stored message, a location that
  differs by value, and evidence that is altered, reordered, or dropped, with
  `ErrParentKeyMismatch` — on the write and on every snapshot reconstruction
  (`GetAttentionItemSnapshot` and the open/all list reads), including after a
  raw-row body rewrite. A missing stored `Finding` fails closed exactly as a
  mismatch does.
- **Confirmed and fixed:** the nullable review-level location round-trips — a
  nil-location finding accepts a nil-location proposal, while a location minted
  into the proposal for that finding fails closed.
- **Confirmed and fixed:** the immutable-history record path
  (`GetAttentionItemRecord`) stays structural-only, so terminal recovery still
  reads a historical item without the current-`Finding` join.
- **Accepted by decision:** an empty finding message is representable and
  authenticated rather than rejected; the revisit condition guards this.
- Verified end-to-end against the real daemon through the convergence harness,
  including the `finding_adjudication` type, with the OpenAPI schema, its
  byte-identical app mirror, the MockServer parity, and the card re-recorded
  screenshots all consistent.
- **Confirmed and fixed (Codex, PR #984 review):** on the linear layout (iOS
  and compact macOS), the shared composition inserted the action region right
  after the recommendation module, before the finding message, location, and
  daemon facts ever rendered, so an operator could reach "Accept recommended
  route" without seeing them — contrary to §9's `finding_adjudication` row,
  which leads with both the labeled proposal and the daemon-fact register.
  See the corrected "Leave the finding context in `.factBlock`..." entry above.
- **Declined by precedent (Codex, PR #984 review):** backfilling, or exempting
  from the re-gate, a stored item body written before this contract. See the
  "Backfill or gate-exempt..." entry above.
- **Confirmed and fixed (Codex, PR #984 review):** the card labeled every
  proposal's evidence "model-derived" unconditionally, but the engine fast
  path also populates `Evidence` with a daemon fact (the finding's own
  containment location, `internal/engine/finding_adjudication.go`'s
  `NewEngineAdjudicationEntry` call), so a daemon-verified line could read as
  unverified model output. The label now follows `producer.modelBacked`, the
  same value that already governs the surrounding solid-vs-dashed register.
  No covered fixture exercises the engine producer with the card renderer, so
  this fix adds no new screenshot digest; a card-level regression test for
  that combination is not currently justified given the shared fixture's
  brittle index-based mutations in `MockContractValidationTests.swift`
  (`proposals[0]`/`proposals[1]`), so this remains covered by inspection and
  the existing `adjudicationProducerPresentation` mapping alone.
- **Confirmed and fixed (Codex, PR #984 review):** the prior fix labeled
  engine evidence as daemon-derived without re-deriving it — the item-level
  gate only proves a proposal repeats its artifact entry's evidence, never
  that the entry's evidence is itself genuine, so a hand-built or decoded
  `FindingAdjudicationEntry` with `Producer: engine` and fabricated evidence
  passed every existing check and would have rendered as an authenticated
  fact. `validateFindingAdjudicationBinding` (`internal/store/
  finding_adjudication.go`, shared by `PutFindingAdjudication` and every
  artifact reconstruction) now re-derives the fast path's one invariant for
  every engine entry with non-empty evidence: it must equal the named
  finding's own containment location, nothing else. Empty evidence is left
  to the structural check alone, since the card renders nothing for it to
  mislabel. Reachability today is narrow — every real writer
  (`PutFindingAdjudication`'s three callers) already threads the correct
  value — but this project's own trust-boundary convention re-verifies a
  decoded or caller-supplied trust bit against current state regardless of
  today's callers, precisely so a later caller cannot reintroduce the gap
  silently; this fix brings evidence up to that same standard. Required
  correcting two pre-existing test fixtures outside the direct
  finding_adjudication surface (`internal/engine/remediation_test.go`,
  `cmd/freesided/submit_test.go`) that used the finding's bare path as an
  evidence placeholder, one line short of the canonical `path:line` string;
  neither test exercises evidence content, so the correction is inert to
  what they verify.

## Revisit When

The adjudication pipeline comes to guarantee that every finding it processes is
fingerprintable (non-empty normalized message and a path), or the finding
message and location gain a daemon-authored home on the artifact entry. Either
change would let domain validation require a non-empty message, or let the
re-gate authenticate the coordinates against the artifact instead of a
`Finding` join.

Freeside's persistence needs to survive a binary upgrade in a real deployment
(the pre-release "clean break" posture this note and #893/#976 rely on no
longer applies). A stored `finding_adjudication` item body written before this
contract, or before #976's `OfferedAlternatives`, would then need an explicit
backfill or migration path instead of failing the re-gate closed.
