# Evidence-manifest and helper-in-image interface contract (#173)

Spine contract unit, 2026-07-18; second of the three #83 finding-remediation
contracts (chain #172 → #173 → #171, filed in
[2026-07-18-1125-finding-contract-prereqs.md](2026-07-18-1125-finding-contract-prereqs.md)).
Defines the second declared workspace-exit channel (plan §5.6) and the fixed
helper-in-image interface; consumers #167 (importer parsing/enforcement,
malicious fixtures) and #170 (real exporter image, conformance) implement
against it.

## Decisions and rejected alternatives

- **Evidence entries are self-contained; the channels stay physically
  separate.** Each evidence-manifest entry carries its own label, media type,
  size, digest, and typed provenance, and its blob lives under a dedicated
  `evidence/` handoff subdirectory (`EvidenceBlobsDirname`), never under the
  repo channel's blobs. Rejected: resolving evidence content through the
  repo-change manifest entry at a path. §5.6 says the two channels never mix,
  and #167's required malicious fixtures include repo-content/evidence
  cross-channel substitution; a path-reference design would make the importer
  arbitrate exactly the coupling the plan forbids, while separate
  digest-addressed storage makes substitution structurally impossible rather
  than merely checked.

- **The schema lives in `daemon/internal/export` with local types.** The
  helper is a standalone binary that imports nothing from domain (the
  existing `export.Digest` precedent), so the wire provenance mirrors the
  domain enum tokens as local `Evidence*` types with `valid()` predicates.
  Rejected: importing domain into export (breaks the standalone constraint)
  or a new shared package (a third home for two consumers that both already
  import export).

- **No `publish_eligible` and no `verification_recipe_digest` anywhere on the
  agent wire.** Trusted policy computes eligibility on `Artifact`; agent
  output is never recipe-produced. The fields are absent from the schema (not
  optional), a reflection test pins their absence, and a strict-decode test
  pins that a manifest smuggling either at any level is malformed input to
  the importer's `DisallowUnknownFields` re-gate.

- **Wire provenance carries `head_binding`** although the issue's field
  sketch omitted it: domain's explicit-mode rule (artifact.go) makes the
  binding mode, not head presence, the single source of truth, and omitting
  it would reintroduce the ambiguity the domain type was designed to kill.

- **`AgentClaim` gains the existing `domain.Provenance`, agent-pinned.**
  Claim validation requires valid provenance and `ProducerClass == agent`
  (`ErrNonAgentClaim`), so a decoded claim cannot launder agent output into a
  trusted producer class. Rejected: a new claim-specific provenance type
  (duplicates a shape domain already owns) and leaving claims untyped until
  #167 (the contract's whole point is that the claim shape exists before the
  importer routes into it). No migration: items persist as a JSON body column
  whose decode re-runs Validate; pre-release rows without claim provenance
  fail closed, the same posture as #172/PR #176.

- **Helper path/command constants live in export; ward adapts mechanically.**
  `HelperPath`, `HelperWorkspaceDir`, `HelperHandoffDir`, `HelperCommand()`
  are the single declaration point; ward's defaults, the helper's flag
  defaults, and ward's spec fixture now reference them (the contract-PR
  mechanical-adapter allowance). Ward's `ExporterCommand` stays required
  runtime config; wiring the probe composition to the real image is #170.

- **The API mirror moves in the same unit.** The sync surface serves
  `domain.AttentionItem` directly, so the claim's new provenance lands on the
  client wire; `api/openapi.yaml` gains an agent-pinned `ClaimProvenance`
  (head-binding oneOf mirroring `EvidenceProvenance`, producer enum `[agent]`,
  recipe digest pinned null) and the app fixtures/mock validation gain
  daemon parity in the same push. Rejected: deferring the spec sync to a
  follow-up (leaves the spec claiming a field-for-field mirror it no longer
  is, and splits a `kind:contract` spec change from its generated consumers
  against the one-work-unit rule).

## Refute-first verification

Two independent fresh-context lenses (trust-boundary; contract-consistency)
were prompted to disprove the diff before push.

**Confirmed, fixed in the folded commits:**

- Mock/daemon wire divergence on the claim recipe digest: the generator
  renders a null-only nullable member as an optional
  `OpenAPIValueContainer`, whose encoder omits the key and whose
  `decodeIfPresent` reads JSON null back as absent, so present-null cannot
  round-trip a client cache. Resolution: the member stays declared and
  null-pinned but is not in the claim branches' required lists (daemon
  always emits explicit null; absent and null are the same statement;
  non-null is invalid), recorded on the `NullRecipeDigest` schema. The
  mock additionally rejects a non-null claim recipe digest, which the
  container type leaves representable, with a negative seed in the
  invariant test.
- No strict-decode entry point bound the "unknown fields are hostile"
  guarantee to code: `export.DecodeEvidenceManifest` now owns strict
  decode + trailing-content rejection + validation, so #167 consumes the
  boundary instead of reimplementing it.
- Codex review (three rounds, all P2, all confirmed) surfaced successive
  members of the same decoder-leniency class in that boundary: `dec.More`
  misses a bare trailing delimiter; `encoding/json` launders invalid
  UTF-8 to U+FFFD instead of failing, making the label UTF-8 check
  unreachable on the decode path; and a nil entries slice gave empty two
  accepted encodings (`[]` and `null`). Per the widen-on-recurrence rule
  the fix is the class, not the members: the boundary is now
  **canonical-or-reject** (a raw UTF-8 pre-check, EOF-after-one-value,
  nil-entries invalid so empty has exactly one wire form, and a final
  re-encode byte-equality gate against `Encode`'s deterministic output),
  which also closes duplicate-key last-wins and whitespace variation,
  leniencies no field check can see. Legitimate producers write the file
  via `Encode` (the fixed helper command), so byte equality costs them
  nothing. Round three also closed the `AgentClaim` object schema to
  undeclared extras, completing the no-representable-trust-bit claim at
  the object level. Follow-up: the pre-existing repo-manifest decoder
  (`importer/handoff.go loadManifest`) shares the UTF-8-laundering
  member; filed as #180 (gauntlet deferral) rather than fixed in
  passing.
- `HeadBoundClaimProvenance` lacked `additionalProperties: false` (the
  head-independent branch had it), leaving `publish_eligible`
  schema-conformant as an undeclared extra on one branch; both claim
  branches now forbid extras. The evidence-side `HeadBoundProvenance`
  shares the openness pre-existing and merged; tightening it is out of
  this unit's scope.
- A stale `export.go` comment claimed the handoff output holds exactly
  manifest.json + blobs; updated for the evidence channel.

**Rejected by verification (attacked, not refuted):** forged producer
class or recipe digest through store decode (re-gated, fails closed);
publish_eligible smuggling through persisted claim JSON or the evidence
wire (no field exists; strict decode rejects); channel mixing or
cross-substitution given separate digest-addressed blob dirs; helper-const
drift (single declaration point, greps clean); token drift across
domain/export/spec; post-validation mutation through `HelperCommand`,
claim clones, or `normalizeAttentionItem`.

**Accepted by decision:** claims are not head-matched against the item
(only evidence is; claims never feed publishing); ward validates
`ExporterCommand` non-empty rather than pinning it to `HelperCommand()`
(production wiring is #170's composition).

## Revisit when

- #167 lands the importer's evidence parser: the malicious-evidence corpus
  may surface schema gaps (e.g. media-type allowlists, size caps) that belong
  in a v2 of the evidence wire format.
- #170 builds the exporter image: if the image layout cannot honor
  `HelperPath`, the constant, not the image, is the contract to renegotiate.
