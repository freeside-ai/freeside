# Mixed-Origin Finding Adjudication (#956)

## Decision

Represent the model-judged, engine-authorized `required` / `allowed` /
`remediate` row with a third persisted producer value, `engine_model`.
Its goal relationship, explanation, and confidence are model-produced;
its compatibility and route are fixed by the domain constructor to the
engine-owned allowed-remediation row. Acceptance remains confidence-gated.

This preserves the eight-row adjudication table and the producer field's
inspectable provenance without changing the canonical shape of any existing
artifact. Existing `engine` and `model` encodings and digests remain unchanged;
only new mixed-origin entries carry the new enum value.

The producer is also copied into the public finding-adjudication proposal
binding. The same contract unit therefore extends the OpenAPI enum and both
generated-client inputs, validates the mixed row in the app mock, and labels it
as a model judgment with engine-authorized remediation. Treating every
non-model value as a daemon recommendation would erase the confidence-bearing
judgment source at the operator boundary.

## Rejected Alternatives

- **Let model output include `allowed` and `remediate`.** Rejected because
  `allowed` is an authority-bearing declared-path containment result. The
  model-output constructor continues to accept only `ProposedCompatibility`,
  whose vocabulary has no `allowed` member.
- **Relabel the composed entry as `engine`.** Rejected because the goal
  relationship was model-judged and its confidence controls acceptance. A pure
  engine entry is always accepted and carries no confidence, so that label
  would erase the judgment source and bypass the dispatch threshold.
- **Add separate goal and route producer fields.** Rejected for this narrow
  contract repair because it would rewrite the canonical shape and digest of
  every existing persisted artifact. The third enum value records the only
  demonstrated mixed shape without a migration.
- **Leave API and app synchronization to the later inference consumer.**
  Rejected after automated review traced every artifact entry through the
  attention-item binding. The serialized producer vocabulary is already a
  public sync contract, while the dependent unit's daemon-only scope had no
  authority to repair either generated client or the operator label.

## Refute-First Verification

The returned-object trust boundary is the decoded producer, compatibility,
route, and confidence tuple.

- **Confirmed and fixed:** a re-signed decoded pure-model entry carrying
  `allowed` still fails with `ErrModelEntryMintsAllowed`; a re-signed decoded
  `engine_model` entry carrying a model-only row fails with
  `ErrEngineModelEntryNonRemediationRow`; the existing direct tests continue to
  reject pure-engine confidence and non-remediation rows.
- **Confirmed and fixed:** `engine_model` remains model-confidence-gated.
  Medium confidence is not accepted at the high threshold and is accepted at
  the medium threshold; the engine-derived `allowed` value does not bypass the
  judgment threshold.
- **Rejected by verification:** adding the producer did not require an encoding
  migration or rewrite existing artifacts. The unchanged engine/model artifact
  and entry goldens passed, while a new mixed-origin entry round-tripped through
  the store and survived checkpoint restore with its producer and confidence
  intact.
- **Confirmed and fixed:** the API and app schema mirrors now enumerate the
  complete domain producer registration; the app fixture round-trips
  `engine_model`, revalidates its confidence and fixed remediation row, and
  renders a producer-specific label rather than a pure daemon recommendation.
- **Accepted by decision:** one composite enum value is narrower than
  orthogonal provenance fields and avoids rewriting every existing digest. The
  revisit condition below prevents silently extending that choice to another
  mixed shape.

## Revisit When

Another mixed producer shape is required. At that point, replace the composite
enum with orthogonal judgment and authority provenance fields under an explicit
encoding migration rather than accumulating more composite values.
