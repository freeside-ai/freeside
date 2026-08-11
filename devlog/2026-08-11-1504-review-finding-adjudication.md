# Finding Adjudication Before Remediation (#697)

Plan revision 31 inserts an adjudication stage between review-finding
ingestion and remediation (§7 "Finding Adjudication"), with a
`finding_adjudication` item type (§4, §9), a second §5.13 ceiling-bounded
annotation site, and wave 6 consumption (§11). The prior wording routed
findings directly to a remediator, assigning nobody the judgment that
decides what a finding means for the approved work unit; that risked
silent scope expansion in one direction and false-ready work (deferring a
finding the acceptance criteria depend on) in the other.

## Decisions

- **`allowed` is engine-derived only.** The deterministic declared-path
  containment check is the sole producer of the `allowed` compatibility
  value; model output structurally cannot mint permission to exit the
  declared surface. Chosen over schema-level parity (letting the model
  propose any axis value and filtering downstream) because a value the
  model cannot emit needs no downstream guard, and the failure mode it
  removes — an adjudicator talked into widening scope by finding text —
  is the unit's core risk.
- **Presumptive permission, not affirmative citation.** In-surface
  remediation is presumptively `allowed`; rule interpretation runs only
  for boundary exits and affirmatively implicated rules, failing
  conservatively to `unknown` or attention. The affirmative-citation
  alternative was rejected because it collapses every ordinary in-scope
  fix to `unknown` and parks the loop.
- **Eight valid axis cells, not a twenty-cell cross product.**
  Compatibility exists exactly when the goal relationship is `required`;
  `required` has no route to a deferred disposition, so defer-and-ready
  on a necessary finding is unrepresentable rather than prohibited by
  prose. Full-cross-product vocabulary was rejected because undefined
  cells become implementation-defined behavior.
- **Deterministic dispatch with a one-directional model residue.** An
  unambiguous finding (credible, confidently material, in-surface)
  routes to the remediator with no model call; contradiction and
  ambiguity reach the adjudicator through structured signals
  (classifier low confidence, remediator labeled pushback, human
  challenge, import-boundary path rejection) rather than a per-batch
  model screen. The fast path is remediation-direction only (refined
  in review, PR #699): the `adjacent` deferral route always takes the
  model adjudication, because the classifier never consumes the
  approved specification and a spec-blind materiality annotation
  cannot decide a spec-relative route; erring toward an in-surface fix
  is bounded by declared paths, so the remediation direction keeps the
  no-model path. A screen-every-batch design was rejected on §3.2
  interruption/cost grounds and because the issue's fast path is
  structural, not an optimization.
- **Separate adjudicator site, not a classifier extension (#656).**
  Argued on inputs (finding plus code context versus specification,
  declared scope, instructions, disposition history), binding cadence
  (per-finding annotation at ingestion versus per-batch digest-bound
  proposal), the one-authority-contract-per-site rule (§5.13), and wave
  6's sampled classification accuracy, which needs classifier output
  measured independently of routing pressure. Merging was considered and
  rejected; folding adjudication into #656 silently was already
  prohibited by the issue either way.
- **Ceiling-bounded annotation, not bounded choice.** The adjudicator's
  output is a labeled proposal consumed by deterministic engine routing;
  bounded choice was rejected because #697 pins model output to
  proposal/explanation and transition authority stays with the engine.
  Ceilings mirror the shadow-finding rule: no declined or deferred
  disposition on a critical or high finding, credible or not, without
  second adjudication or attention.

## Review Adjudication (PR #699)

The revision's adversarial review (Codex) ran sixteen rounds; the
located per-finding record — each disposition with its fixing commit —
lives on the PR's resolved threads. Durable outcomes:

- **Accepted residual (declined finding): fast-path rule
  consultation.** Containment proves path membership, not rule
  permission, so a rules-implicating in-surface fix can pass the
  no-model path unexamined. Confirmed as real and accepted by decision
  rather than remediated: affirmative rule consultation is the
  affirmative-citation shape #697 rejects; implicated rules reach
  adjudication through the structured residue signals or captured
  repository facts; the downstream human merge gate backstops the
  residual. Named in the presumption paragraph.
- **Confirmed-and-fixed contract clarifications.** Review forced: the
  remediation surface to a per-finding engine derivation with
  fail-closed path resolution across both candidate-diff sides; the
  severity scale declared with the per-source mapping (`codex_local`
  P1→high, P2→medium, P3→low; unmapped fails protective); credibility
  made a total fail-safe guard no model output can mint or strip, with
  the critical/high second-adjudication ceiling unconditional;
  materiality and confidence aligned to the landed ordinal vocabulary
  with bounded-below dispatch thresholds (default `high`); the
  `work_unit_revision_required` remedy split into prose revision
  versus replan, since a same-unit revision cannot alter trusted
  scope; review completion made disposition-aware so a fully
  dispositioned round publishes without a futile re-review
  (suppression-aware re-review rejected: identity suppression would
  add a second completion authority beside the disposition record);
  and the `adjacent` route's interim landing defined so wave 6 needs
  no 1B.1 pull-forward.
- **Prose-altitude reframe.** A five-round finding class — a normative
  term without a declared scale, named producer, or fail-safe
  fallback — kept recurring because each prose-added field interacts
  with the eight-cell vocabulary and nothing checks those interactions
  in prose. After a full-term enumeration and a fresh-context refute
  pass, the discipline was restated in §7 as an acceptance requirement
  of the wave 6 `kind:contract` unit, whose typed schema and fixtures
  are the exhaustive enumeration; the subsection states the design's
  constraints, not the field catalogue. Standing disposition for
  future findings of this class: wave 6 fixture material.
- **Fixed-disposition identity conditioning (deferral).** The
  identity-absence proof for `fixed` is valid only under a finding
  identity stable across one work unit's remediation rounds, and the
  landed `codex_local` per-invocation ID does not provide one (it
  hashes invocation and head) — a gap in the landed revision-25
  stable-identity premise that predates this revision. The proof is
  conditioned in §7 accordingly; defining the cross-round identity is
  deferred to the wave 6 contract unit. Follow-up: #702.

## Changed Assumptions

- #527's non-goal ("changing the review loop's internals ... stays as
  landed; only their anchor moves") bound that implementation unit, not
  the plan; revision 31 is the explicit, gated change to the loop shape
  that #427/#527 wording cited verbatim.
- The human `review_dispute` card keeps the verb "adjudicate": daemon
  adjudication proposes; the human still adjudicates disputes. The
  `contradictory` route lands on the existing item rather than a new
  surface.

## Verification Findings

- The production engine today writes no `ReviewDispositionRecord`: a
  review with findings escalates to attention before any remediation
  (production_publication.go, reviewEscalated path). The stage inserts
  into a gap; no landed routing behavior is displaced.
- #652's disposition contract needs no widening: `fixed` already
  requires the later same-base different-head remediation review — now
  one in which the finding's stable identity no longer appears, under
  the #702 identity conditioning — and declines/deferrals carry
  mandatory reasons that can cite
  the adjudication artifact digest. The identity-absence requirement
  is a plan-level completion semantic the wave 6 unit implements over
  the landed shapes, not a #652 schema change, so the no-widening
  claim still holds. No "queued for remediation" state is needed
  because the artifact, not the disposition, carries in-flight
  routing.
- #525's publication completeness rule (exactly one final disposition
  per lineage finding) stays satisfied structurally: parked runs never
  reach publication.

Revisit when wave 6 convergence measurement shows credible, material,
in-surface findings routinely reaching the model residue (dispatch
predicate miscalibrated), or when real usage shows adjacent findings
deserve an in-unit fix path rather than defer-plus-follow-up.
