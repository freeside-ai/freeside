# Explicit System-Health Posture

Chose a required, typed `HealthPosture` (`blocking` or `advisory`) on
`system_health` AttentionItems over an optional advisory marker because the
author must decide the admission effect explicitly. A nil-means-blocking
contract would preserve current rows without a migration, but it would leave
"not decided" indistinguishable from "blocking by design" and would break the
API's required-field convention.

Posture and `BlockingSupersession` remain separate concepts. Advisory items
never block unrelated unattended admission and cannot carry a supersession;
blocking items retain the existing behavior, including live-policy re-gating
of any supersession. Existing health rows are backfilled to `blocking`, which
preserves their pre-change semantics, and reconstruction requires a valid
posture so malformed or partially migrated rows fail closed.

Revisit when a third admission posture has a concrete consumer whose behavior
cannot be represented as blocking, advisory, or a blocking item with a
live-policy supersession.

## Refute-First Verification

Confirmed and fixed:

- A body-only rewrite from `blocking` to `advisory` originally passed domain
  validation and could lift admission. Migration 0035 now adds a store-owned
  `health_posture` column, every write stamps it, reconstruction and the
  whole-table divergence query cross-check it, and an adversarial store test
  proves the rewrite fails closed.
- Migration 0035 originally rewrote synchronized item bodies without advancing
  their entity or server revision, so a client could retain the posture-less
  pre-upgrade body as current. The migration now advances the server revision
  once, stamps every rewritten row at that revision, and increments each row's
  entity version; the migration test pins all three coordinates.
- The prior Section 4 statement that every `system_health` item remains
  blocking contradicted advisory posture. Plan revision 29 now records the
  material safety-policy change and its decision history.
- The real-daemon convergence harness constructed system-health fixtures
  outside the three production producers. Its first run failed; the harness
  now chooses explicit blocking posture and the rerun passes.

Rejected by verification:

- Missing posture cannot reach the admission pointer dereference because
  reconstruction runs `AttentionItem.Validate` first; a raw missing-posture
  row is refused by the store test.
- Legacy health rows are not partially backfilled: the pre-0035 migration test
  starts at schema version 34, migrates every historical item status, and
  reconstructs the result as explicitly blocking.
- Advisory items cannot carry a supersession, ordinary transitions cannot
  change posture, and every production system-health creator remains
  explicitly blocking; domain, transition, and producer tests pin each rule.

Accepted by decision: none. Both confirmed findings changed the implementation
rather than being waived.
