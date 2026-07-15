# Timing placement on AttentionItem: field kept

Wave 0 exit review gate (#28), deferred from the domain-package unit
(2026-07-13-1528-domain-package.md, `## Deferred`). Decider: owner
choice, fiat-assigned to this session.

Chose keeping `Timing` on `AttentionItem` as a `WithTiming`-only field
over compute-on-demand-with-no-field because the field placement is
what the rest of the contract already assumes, and removing it buys
nothing a consumer has asked for:

- Plan §4 lists "derived timing aggregates" in the AttentionItem model;
  the field matches the plan text.
- The API schema has mirrored it since PR #26 (`TimingSummary` on
  `AttentionItem`, required, "daemon-derived, never client-set");
  reversing is a two-surface `kind:contract` change (domain +
  `api/openapi.yaml`) with consumer churn and no consumer benefit.
- The trust boundary is already hardened around the field: `WithTiming`
  is the sole writer, and `TimingSummary.Validate` is the
  store-reconstruction backstop (PR #19 review rounds 8–10).
  Compute-on-demand discards that backstop and diverges the wire shape
  from the domain type (or forces a wrapper), plus golden-fixture
  rework.
- Keeping the field does not force persisting it: the store may derive
  at read via `WithTiming`. Persist-vs-derive-at-read stays an
  implementation decision for the signet deliveries unit (#69), which
  this decision unblocks.

Rejected alternative: compute-on-demand (no field), the option the
source entry flagged for spine.

Superseded acceptance criterion: #28's criterion 2 asked for a `->`
marker on the source entry's timing bullet. The current devlog
protocol (README, "Historical entries") freezes entries carrying `->`
markers and forbids writing markers back; the criterion predates that
rule, so the marker is deliberately not written and this note is the
record instead.

Revisit when a consumer needs item timing that read-time `WithTiming`
derivation cannot serve, or #69's store path shows a stored aggregate
creating staleness defects.
