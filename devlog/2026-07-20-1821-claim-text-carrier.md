---
run: manual
stage: claim-text-carrier
date: 2026-07-20
branch: feat/claim-text-carrier
---

# Claim Text Carrier: Inline, Digest-Bound

Contract change (#217, fiat-assigned), the carrier plan §9 rev 13
requires before any card-summary implementing unit. Declared paths:
`api/`, `daemon/`, `app/` (one cross-component contract unit),
`devlog/` (this note).

## Context

§9's summary layer renders as a labeled agent claim, but `AgentClaim`
carried only an artifact reference (`label`/`artifact_id`/`digest`/
`provenance`) and the clients rendered non-image claim bytes as a bare
digest row, so no summary could render (raised by Codex review on
PR #210; recorded in the 2026-07-20-1137 note's revisit condition).

## Decision

`AgentClaim` gains an optional `text` member (`ClaimText`:
`media_type` + `content`), null on every non-text claim per the
always-present wire convention. Owner decisions:

- **Inline and digest-bound, not attachment-referenced.** The content
  rides in the item payload, and the claim's existing digest MUST
  equal sha256 over the content's UTF-8 bytes;
  `domain.AgentClaim.Validate` recomputes it and fails closed, at the
  choke point every construction, decode, and import path shares. A
  card's lead layer therefore renders with zero fetches, the binding
  set (`artifact_digests`) covers the rendered prose, and a claim
  cannot display one text while binding another digest (the
  stale-approval class, §3.1). The digest stays a valid
  attachment-store address for the same bytes.
- **Media types: `text/plain` and `text/markdown` only.** Markdown is
  admitted at introduction because §9's digested-feedback and
  change-summary digests want structure, and a later widening would
  cost another serialized contract round; the set stays closed to
  seconds-readable prose (images remain on the referenced path).
- **64 KiB content cap** (`domain.MaxClaimTextBytes`): inline content
  is re-sent on every list/bootstrap read, unlike out-of-line
  attachments under the loader's 8 MiB cap; 64 KiB bounds item blobs
  while staying far beyond a seconds-readable summary. UTF-8 validity
  is checked explicitly (`utf8.ValidString`), since a JSON decode
  admits escaped invalid bytes (#180).
- **No inline text at `high_sensitivity`** (raised by Codex on the
  PR): clients persist item metadata in disk caches, and §5.14's
  no-high-sensitivity-at-rest default holds only because such bytes
  stay out-of-line and memory-only. A high-sensitivity claim therefore
  never carries inline text (`ErrHighSensitivityClaimText`, fail
  closed, mirrored in the mock); memory-only prose travels the
  referenced attachment path. The middle `sensitive` tier persists
  at rest today, so it keeps inline carriage.

Rejected alternative: **reference-only carrier** (a `media_type` on the
claim, bytes via the attachment channel). Keeps item payloads lean, but
the card's lead layer would need a fetch per claim, the loader treats
non-image bytes as invisible, and sync/preview surfaces would carry no
renderable summary; rejected for the layer §9 defines as
absorbable-in-seconds.

Trust posture unchanged: the carrier is `producer_class: agent`,
agent-pinned provenance intact, never `evidence_snapshot` (§5.15).
Producing summaries (prompting, stage outputs, importer media-type
widening beyond images) stays implementing-unit work.

## Verification Findings

- swift-openapi-generator 1.13.0 accepts the required + nullable-$ref
  member (the `NullRecipeDigest` unrepresentability concern does not
  bite here: `ClaimText` has real content, so the generated optional
  round-trips both null and object).
- The Swift digest twin is pinned to the FIPS 180-2 `"abc"` vector and
  backed by swift-crypto (exact 4.5.1), because FreesideAPI's tests run
  on Linux CI where CryptoKit is absent.
- Real-daemon convergence: a harness-seeded markdown claim reaches the
  generated client intact (content, media type, binding-set
  membership) through a live bootstrap.

## Revisit When ...

- An implementing unit needs a text media type beyond plain/markdown,
  or a summary exceeding the 64 KiB inline cap; either is a new
  `kind:contract` unit, not a rendering workaround.
- The §9 implementing units land and per-type leads become real
  layouts; the uniform "summary" fixture claims here are placeholders
  for that work, not a presentation decision.
