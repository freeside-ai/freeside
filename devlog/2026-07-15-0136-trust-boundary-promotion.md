---
run: manual
stage: wave1-spine
date: 2026-07-15
branch: docs/trust-boundary-promotion
---

# Store trust-boundary re-gate invariant promoted to AGENTS.md (#52)

Promotion of the deferred `## To promote` item from
`2026-07-14-0939-approved-recipe-boundary.md`, escalated as #52 and
scheduled by the Wave 1 sweep, serialized after #27 on AGENTS.md.
Decider: spine (wording and binding scope delegated by the issue).

## Decisions

- **Promoted the invariant as a daemon-wide bullet in the AGENTS.md
  "Daemon coding conventions" section**, not a store-scoped line. The
  issue's promotion condition (recurrence beyond
  `daemon/internal/store`) is met by PR #51: a reconstructed
  `AttentionItem` re-runs `Provenance.Validate` (the explicit mode,
  not a decoded head, is the source of truth;
  `daemon/internal/domain/artifact.go`) and
  `EligibleForEvidenceSnapshot` against the current approved-recipe
  set, the same decode-then-re-gate shape as the original store
  instance (`daemon/internal/store/entities.go`: `PutArtifact`,
  `GetArtifact`, `gateEvidence`; issue #31 / PR #40). That second
  instance already spans domain+store, and the Wave 1 gauntlet
  importer (plan §5.6) is a hostile decode path in exactly this
  class, so a store-only line would need re-promotion almost
  immediately. Rejected alternative: a line scoped to
  `daemon/internal/store`, per the source entry's original phrasing.
- **Wording follows the section's pointer-bullet pattern**: rule plus
  detail-at-point-of-use pointers, carrying its own provenance
  parenthetical (promoted per #52) rather than widening the section
  intro's #27 origin sentence. Same ratchet semantics as the rest of
  the section: binds new and changed code; a pre-promotion deviation
  drains as its own unit.
- **The source note stays untouched.** Its legacy `-> open` marker is
  frozen history under the current devlog protocol
  (`devlog/README.md`, historical-entries rule); #52's closure plus
  this note carry the drain state.

Revisit when: the gauntlet importer lands; confirm the bullet's
"decodes a row or accepts an exported struct" phrasing covers
external-input decode there, or sharpen it in a follow-up docs unit.
