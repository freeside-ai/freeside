# Repo-manifest decode: reject raw invalid UTF-8, but not canonical-or-reject (#180)

Gauntlet fix, 2026-07-19. Closes the UTF-8-laundering member of the
decoder-leniency class in `importer/handoff.go loadManifest`, the §5.6
hostile-import boundary. Filed from
[2026-07-18-1540-evidence-manifest-contract.md](2026-07-18-1540-evidence-manifest-contract.md)
(line 109-112), which fixed the same class in the sibling boundary
`export.DecodeEvidenceManifest` under #173/PR #179 and deferred the
repo-manifest decoder to #180.

## The bug

`encoding/json` replaces invalid UTF-8 bytes with U+FFFD instead of
failing. `loadManifest` fed the raw bytes straight to a strict decoder,
so a hostile manifest carrying raw invalid UTF-8 inside a `path` decoded
into a valid-looking string that then passed `validCanonicalPath`
(`utf8.ValidString` on the *post-decode* string, where the laundering had
already happened). Fix: a whole-buffer `utf8.Valid(data)` pre-check before
the decoder, failing closed with `ErrManifestInvalid` wrapping
`export.ErrInvalidUTF8`. Non-tautological: with the check neutralized, the
laundered manifest loads with a nil error (the exact defect).

## Decision: UTF-8 pre-check only, NOT full canonical-or-reject

The issue asked to decide between the raw-UTF-8 pre-check alone and full
canonical-or-reject (the sibling boundary's posture: re-encode the decoded
value and require byte-equality with `Manifest.Encode`, which also closes
duplicate-key last-wins and whitespace/ordering variation). The approved
plan initially chose full canonical-or-reject for symmetry with
`DecodeEvidenceManifest`. Implementation surfaced a fact that reverses that
choice.

**Changed assumption — `Manifest.Encode` is not idempotent for this
manifest.** The repo manifest records symlink targets *verbatim*
(`export/walk.go` classifyFile: "a target whose bytes are not valid UTF-8
survives only best-effort in JSON"). Go's `json.Marshal` escapes a raw
invalid byte as a 6-byte ASCII backslash-u escape of the replacement
rune, but re-emits an *already-decoded* U+FFFD rune as the raw 3-byte
character. So for a
legitimate invalid-UTF-8 target, `Encode(m)` and
`Encode(decode(Encode(m)))` differ (measured: 220 vs 217 bytes). A
byte-equality gate would therefore reject honest exporter output. The
existing `TestImportLossySymlinkTargetNeverElides` exercises exactly this
real flow and fails under the canonical gate. The evidence manifest has no
verbatim-bytes field (labels/paths are UTF-8-validated), so it can afford
the stricter gate; this manifest cannot.

**Rejected alternative — full canonical-or-reject.** Would break the
lossy-symlink-target invariant above. Normalizing targets to close the gap
was also rejected: it destroys the very distinction
`TestImportLossySymlinkTargetNeverElides` protects (a real U+FFFD target vs
a laundered one must stay distinguishable). Making `Encode` idempotent is
an export-package contract change, out of this unit's `daemon/internal/importer`
scope and unnecessary for #180.

**Why UTF-8-only is sufficient here.** The leniencies the dropped gate
would also close smuggle nothing in *this* manifest: it carries no trust
bit (no `publish_eligible` equivalent), `DisallowUnknownFields` rejects
undeclared keys, `Manifest.Validate` re-gates whatever value the decoder
resolves, and `Entry.Digest` is verified by re-hashing the actual blob
(`importer/blobs.go`), not trusted verbatim. A duplicate-key last-wins can
only select a value that is then fully validated and processed; there is no
first-key/rendered-value gap for it to exploit. This differs from the
evidence/claim boundary, where the canonical gate was load-bearing against
a real trust-bit smuggle.

## Refute-first verification

Trust-boundary change (hostile-input decode seam), so a fresh-context lens
was prompted to disprove the fix given only the diff and stated intent.

**Confirmed (fix holds):**
- Whole-buffer `utf8.Valid(data)` covers every byte before decode; no
  post-decode consumer sees un-scanned raw bytes.
- Dropping the canonical gate is safe: no trusted-verbatim field in
  `export.Manifest`/`Entry` (digest re-hashed; kind/blob_omitted/size are
  decoded-then-processed, no separate first-key rendering).

**Rejected by verification (not defects):**
- Escaped unpaired surrogate `\uD800` in a `path` decodes to U+FFFD and
  passes the pre-check (the escape is ASCII). Not a #180 bypass: this is
  standard JSON semantics with no hidden raw-byte smuggle, and the decoded
  U+FFFD path is fully validated by `validCanonicalPath`. Rejecting escaped
  surrogates would be scope creep beyond the raw-laundering threat.
- LimitReader truncation at `MaxManifestBytes+1` cannot manufacture a
  spurious reject that matters: a truncated multi-byte rune is invalid
  UTF-8 and rejected, but such a manifest is already malformed/over-cap and
  never a legitimate input.

## Revisit when

- A future manifest field is added that IS trusted verbatim (rendered or
  acted on without re-derivation): the duplicate-key/canonical argument
  above would need re-examination, and canonical-or-reject may become
  load-bearing.
- If `Manifest.Encode` is ever made idempotent for invalid-UTF-8 fields
  (an export contract change), the canonical gate could be reconsidered for
  symmetry with the evidence boundary.
