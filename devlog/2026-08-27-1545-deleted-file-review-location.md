# Deleted-File Review Finding Location Representation (§7)

Work unit: #855 (`kind:contract`, wave-6 contract queue, tracker #835).
Resolves the #679 review-deferred gap: a finding on a candidate-deleted
file has no new-side line, so the shared ward review-output schema
(`start_line`/`end_line ≥ 1`) could not express a valid location for it,
blocking §7's requirement that such findings retain deterministic
routing. The issue body is the authoritative contract; this note records
the load-bearing decisions.

## Decision: Whole-File Affordance in the Ward Schema, Admitted Only Under an Explicit Marker

The chosen representation adds a whole-file variant to the shared
review-output JSON schema and accepts it in ward normalization, mapping
it to the domain whole-file location `FindingLocation{Path, 0, 0}`. It is
**mostly ward-local**: `domain.FindingLocation.Validate` already accepts
`(0,0)` and `diffscope.Overlaps` already routes a whole-file location on
a touched (including deleted) path, so neither changed. (**Corrected in
review:** a second domain boundary, the shadow persistence validator, did
need to change in lockstep — see "Review correction" below; the original
"ward-local, no domain change" claim was wrong.) This reverses the
#679 "whole-file is a native-ingest shape, never a review shape"
narrowing *only* for an explicitly marked review finding, leaving the
anti-schema-escape gate intact for every other shape.

Schema shape: `location` is a JSON-Schema `anyOf` of the unchanged range
object `{path, start_line≥1, end_line≥1}` and a whole-file object
`{path, whole_file:true}`. `additionalProperties:false` on each keeps the
two variants mutually exclusive. The range branch is **byte-for-byte the
pre-#855 shape**, so the common line-range case is unchanged.

### `oneOf` → `anyOf` (Plan Adjustment)

The implementation plan recommended `oneOf`. OpenAI Structured Outputs
(consumed by `codex exec --output-schema`) supports `anyOf`, not `oneOf`;
Claude `--json-schema` supports both. Realized as `anyOf`. Because the two
branches cannot both match a single object (mutually exclusive required
fields under `additionalProperties:false`), `anyOf` and `oneOf` are
semantically identical here, so the substitution costs nothing.

### Normalization Is the Hard Gate, Not the Schema

The schema only steers the model; `normalizeCollection`
(`codex_review_source.go`) is the fail-closed gate and is **marker-driven**:
the domain `(0,0)` location is admitted only when `whole_file:true` is
explicitly present. A bare/unmarked `(0,0)`, a partial or non-positive
range, and an inverted range are still rejected as
`ReviewFailureContradiction` (the #679 narrowing, preserved for the range
variant). A `whole_file:true` paired with a concrete line range is
itself a contradiction and rejected. Codex and Claude share this one
normalization, so the fix covers both providers (the "fix the class"
sweep: the `StartLine < 1` narrowing existed only at this single site in
ward; a second, non-ward site surfaced in review — see below).

## Review Corrections (Codex)

Two findings corrected gaps this note's original scope claim and the
refute-first pass missed. Both are within the unit's declared `daemon/`
path scope and complete the same contract (no representation change).

- **P1 (blocker): the shadow persistence contract had to move in
  lockstep with the shared schema.** The "ward-local, no domain change"
  claim was wrong. The Claude shadow reviewer shares this exact
  normalization (`NewClaudeReviewSource` → `NewCodexReviewSource`) and
  the one schema constant, so it now emits the whole-file variant — but
  `domain.ValidateShadowReviewFinding` (the shadow persistence boundary,
  re-run on the engine completion path `engine/shadow_review.go` and on
  both store write and read) still mirrored the pre-#855 `StartLine >= 1`
  contract and rejected the `(0,0)` location with `ErrNonPositive`. The
  engine then abandoned the *entire* shadow pass, discarding exactly the
  candidate-deleted-file finding this unit exists to preserve. Fix: drop
  the stale `StartLine < 1` case; the location shape is already enforced
  by `Finding.Validate` (→ `FindingLocation.Validate`), which admits
  `(0,0)` and rejects partial/non-positive/inverted ranges — the same
  contract normalization enforces — so the shadow-specific cases keep
  only their required-ness checks (severity, present location,
  explanation). The PR's own tests had encoded the old rejection at both
  the domain and store boundaries; they now assert whole-file
  *acceptance* plus a positive store round-trip. The `StartLine < 1`
  class now has two sites, both fixed: ward normalization (original) and
  this shadow validator.

- **P2 (two rounds): reject every present-but-non-true `whole_file`
  marker via key presence.** `whole_file` is `enum:[true]`, so any
  present marker other than the literal `true` is a schema-escaping
  shape. Round 1 keyed the gate on the decoded `*bool`
  (`WholeFile != nil && *WholeFile`) and rejected a present `false`.
  Round 2 (Codex) showed that was the wrong granularity: an explicit
  `null` decodes to a nil `*bool`, indistinguishable from an absent key,
  so `{whole_file:null, start_line:1, end_line:1}` still slipped through
  as an accepted range. Root cause: a `*bool` cannot express key
  presence. Fix: decode `whole_file` as a `json.RawMessage` and match by
  presence — absent selects the range variant, the literal `true`
  selects whole-file, and every other present token (`null`, `false`, a
  number, a string, an object) fails closed as a contradiction. The
  change is local to the one raw-struct field and the one gate;
  `WholeFile` has no other consumer. It also consolidates non-boolean
  marker rejection at the hard gate (it previously fell to `strictjson`'s
  type check). A fresh-context read-only refute pass re-enumerated the
  full marker × range input space against the widened gate — including
  `null`, `false`, numeric/string/object tokens, duplicate and
  case-folded keys, whitespace padding, and the marker-plus-range and
  inverted/partial/empty-path residues — and found no escape.
  **Declined** the finding's other half (presence-aware `*int` line
  fields to reject `{whole_file:true, start_line:0, end_line:0}`): a
  present-zero range on a valid `true` marker resolves to the correct
  `(0,0)` whole-file location — no schema-escape, no misrouting — and a
  marker paired with a *nonzero* range is already rejected, so the
  `*int` churn across every `FindingLocation` consumer is
  disproportionate to a non-harmful case.

- **Fuzz oracle: restrict the #872 equivalence proof to the pre-#855
  input surface.** `FuzzReviewNormalizeCollectionEquivalence`
  (`review_equivalence_test.go`) proves the #872 provider-neutral
  refactor is decision-for-decision identical to a verbatim pre-#872
  base reconstruction. That reconstruction has no `whole_file` field, so
  its strict decode rejects the key as unknown, while the current path
  admits `{whole_file:true}` — a genuine, intended divergence #855
  introduces. Off that one surface the two are still identical (an
  absent marker takes the same range path with the same failure
  messages), so the fix skips the equivalence assertion only when the
  input carries a `whole_file` marker, seeds the three marker tokens,
  and corrects the stale seed-list comment. Verified by a 25 s live
  `-fuzz` run (~726k execs, no off-surface divergence); the whole_file
  behavior itself stays pinned in `codex_review_source_test.go`.
  Editing the base reconstruction to match was rejected: it would
  destroy its role as the independent reference.

## Rejected Alternatives

- **Explicit `whole_file` boolean + nullable line fields, single object**
  (the plan's stated fallback). Changes the common-case location shape for
  *every* finding (required boolean, nullable ints) and loses the schema
  `minimum:1` steering. `anyOf` preserves the range branch exactly.
  **Retained as the fallback** if a live engine ever rejects `anyOf`: the
  normalization is already marker-driven and shape-agnostic, so switching
  the schema literal to the boolean form needs no normalization change.
- **Diff-side (old/new) marker or old-side range validation.** Larger: it
  adds a `domain.FindingLocation` field (golden regeneration across
  domain/store/exec plus `Fingerprint`/comparability review) and requires
  `diffscope` to parse old-side ranges (today only new-side). Not required
  by §7, whose routing is path-based, and out of scope for this unit.

## Discovered, Not in the Plan: The Configuration Digest Changes (Safe)

The schema is embedded in the review command, which feeds
`CommandTemplateDigest` in the review configuration envelope, so changing
the schema changes the review **configuration digest**. Four goldens
capture schema/digest-derived values and were regenerated: the ward
`codex-review-spec.golden` (the review command's inline schema) and
`codex-review-configuration-digest.golden`, plus two `cmd/freesided`
composition goldens the digest ripples into — `preflight-manifest.golden`
(its `review_configuration_digest` field only) and
`shadow-review-composition-digest.golden` (the Claude shadow-review
composition digest, which binds the Claude review config digest). This is
correct and expected: the schema is part of the review trust profile, and
those two `cmd/freesided` tests exist to flag any review-composition
change — here an intended one. The engine compares this digest
(`production_publication.go`) to decide whether a persisted review is
stale under the current configuration, so after this change prior reviews
are treated as a **different configuration and re-reviewed** — the
designed invalidation, not a break.

It is **not** a new upgrade-in-place reconstruction break (so it adds
nothing to #854's scope): a persisted review outcome re-validates against
its *own stored* configuration digest through the completion evidence,
which `verifyCompletionEvidence` recomputes from the persisted fields, not
from the live schema. The engine's digest comparison is a staleness
check, not a reconstruction gate, so a mismatch means "re-review," never
"fail to reconstruct." No domain shape changed, so no persisted-row decode
regresses. No `configurationVersion` bump is needed: the content-addressed
digest already changes, and the envelope structure is unchanged.

## Residual Risk: Live-Engine Schema Acceptance Is Reasoned, Not Verified Here

The live Codex/Claude review suites are opt-in and CI-blind, so this
session could not empirically confirm that both engines accept
`anyOf` + a single-value boolean `enum:[true]`. Assessment: `anyOf` is the
documented union construct for OpenAI Structured Outputs and standard for
Claude, so acceptance is expected. Mitigations if the assessment is wrong:
a schema-load rejection is **loud** (every review fails immediately, not a
silent corruption) and trivially revertable; and normalization is the hard
gate, so any imperfect engine steering degrades to a rejected contradiction,
never to admitting an invalid location.

**Revisit when:** a live review run rejects the schema at load — switch the
schema literal to the boolean + nullable-line fallback above (no
normalization change needed); or a diff-side location representation is
required by a later §7 consumer; or `diffscope`/`domain` deleted-path
handling changes.

## Refute-First Verification (Returned-Object Trust Boundary)

Ward normalization decodes external agent output, so a refute-first pass
(fresh-context lens prompted to disprove) ran before commit, plus an
adversarial enumeration of the location input space encoded as tests.

- **Confirmed and fixed:** none. No input in the enumerated space admits
  the domain `(0,0)` location without an explicit marker, or admits an
  invalid range.
- **Rejected by verification (not re-raised):** (a) unmarked `(0,0)`, and
  a nil/`false` marker with `(0,0)`, hit the range branch's `< 1` guard →
  contradiction; `(0,0)` never reaches `Validate()` unmarked (#679 gate
  intact). (**Corrected in review (P2 above):** this only covered a
  non-true marker with `(0,0)`; a `false` (round 1) or `null` (round 2)
  marker paired with a *valid* range slipped through as an accepted
  range, because the old gate keyed on the decoded `*bool` value rather
  than on the marker's key presence — and a `*bool` collapses `null` and
  an absent key to nil. `whole_file` is now a `json.RawMessage` and the
  gate rejects any present-non-true marker.) (b) `whole_file:true` with any non-zero endpoint (including a
  set endpoint alongside a JSON `null`, which decodes to 0) →
  contradiction. (c) partial/negative ranges → the `< 1` guard; an
  inverted range passes the guard and is caught by the final
  `FindingLocation.Validate()` backstop (`ErrInvertedRange`), which is
  therefore live, not dead. (d) empty path with a marker → `Validate()`
  `ErrEmptyField`. (e) duplicate keys and unknown fields fail closed at
  `RejectDuplicateJSONKeys`/`strictjson.Decode` before the gate; a
  non-boolean, `null`, or `false` `whole_file` now fails closed at the
  marker gate itself (the `json.RawMessage` field captures any token and
  the gate admits only the literal `true`; see P2 above). (f) provider parity: Codex and Claude share this one
  normalization and the one schema constant, so the gate is identical. (g)
  round-trip: the normalized `(0,0)` routes `true` through
  `diffscope.Overlaps` on a deletion diff, a range on the deleted path
  `false`. (h) no regression: the range branch object is byte-identical to
  pre-#855, and only the two intended goldens embed the schema.
- **Accepted by decision:** the residual live-engine schema-load risk (an
  engine rejecting the `anyOf` schema at load would break every review, and
  the live suites are CI-blind). The fresh-context lens independently
  judged the `anyOf` + `enum:[true]` shape low-risk and explicitly would
  not redesign it to a nullable-fields single object (which would weaken
  the branches' mutual exclusivity). It is surfaced at handoff with a
  recommended one-shot live smoke run (`codex exec --output-schema`,
  `claude -p --json-schema`) before merge; the boolean+nullable fallback
  above needs no normalization change if it is ever required.
