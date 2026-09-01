# Readiness Detail on Ready Items (#982)

Contract change: `AttentionItem` gains `readiness_detail`, the daemon-built
projection of the §6 evaluation behind a `ready_for_final_review` item's
verdict (the evaluation-set digest, the bound candidate head and base, and
every requirement's state, proof recipe, or waiver identity and granting
authority), persisted in a store-owned column (migration 0063) and rendered
by the clients as per-requirement rows. Four readiness enums join the wire
registry. The verdict itself and `ReadinessSummary` do not change.

## Decisions

- **A new creation-immutable field over extending `ReadinessSummary`.**
  Chose a sibling `readiness_detail` over widening the summary in place
  because the summary has a store-owned authority column since 2026-08-24
  whose rows would need a third "partially legacy" tolerance, and because
  the summary is the narrow identity the digest binding was designed around
  (devlog/2026-08-24-0927-ready-verdict-payload.md). The detail follows the
  summary's exact legacy rule: nil is valid for items persisted before it
  and for fake-mode items, and it never appears without the summary.
- **Projected at creation, never derived at read time.** Chose to build the
  detail once in `currentReadinessVerdict`, from the same target, verdict,
  and recorded states, over reconstructing it from persisted proofs and
  waiver gates on each read. A read-time projection would let the card's
  facts move after creation, contradicting the digest binding the summary
  carries, and the store has no per-item index over proofs. `NewReadinessDetail`
  never recomputes the evaluation-set digest; it copies the verdict's and
  refuses a class its entries contradict, so an item cannot carry two
  disagreeing verdicts.
- **Blocked stays representable in the detail, never on a ready item.**
  Reconciled at planning: a `ready_for_final_review` item never carries a
  blocked verdict because the summary admits only the two ready classes and
  `completePublishedTask` holds a run whose re-gate is not clean. The detail
  shape can express a required non-pass without a waiver so its golden pins
  the reasons a blocked verdict would carry, and item validation rejects it
  through the class comparison. "Blocking reasons" therefore render as
  per-requirement states on a ready card, with a waived failure showing the
  failure beside the waiver that permits progress. Today's production set is
  two required, waiver-ineligible checks, so every production item is
  `ready_clean`; the degraded and stale shapes are exercised through domain
  goldens, the mock, and client rendering.
- **The daemon's own labels, not derived reasons.** The card labels each
  row by the requirement key the daemon evaluated and reads state and the
  waiver's id, dimension, and granting authority from typed fields. The id
  sits on the card rather than only in the technical details because the
  SURFACES.md rule this unit flips to Done names waiver IDs, and a
  free-form id (`minLength: 1`, not a digest) costs one clause. Staleness
  is either daemon fact (`readiness_invalidation`, or
  `base_freshness.advanced`), and the
  card shows both sides of the divergence the daemon recorded; it compares
  nothing itself.
- **Store comparison by canonical encoding.** The detail carries slices, so
  the trust-boundary check compares the column and the body's copy through
  their canonical encodings, as the yield history does, rather than `==`.
- **That comparison stays the detail's store authority.** Review asked for
  the projection to be re-authenticated at read time against the §6
  `requirement_resolutions`, `check_proofs`, and `degraded_waivers` rows.
  Declined: no caller or admitted input can forge the field. `AttentionItem`
  is response-only on the wire, every write derives the column from the
  already-validated item, and the transition gate makes the field
  creation-immutable, so only direct write access to the SQLite file makes
  both copies agree on a forged value. That same access can insert matching
  §6 rows, which are content-addressed with unkeyed digests in the same
  file, so the re-gate would raise the forgery cost rather than
  authenticate the facts. It would also differ in kind from
  `validateReadyItemPRBinding`, whose independent records defend a reachable
  non-SQL path (a synchronized body claiming a different pull request); for
  the detail the column comparison is that same defense. What is already
  re-authenticated: read-time validation binds the detail's candidate head
  to `pr_head_sha`, its digest and class to the summary, and rejects a
  waiver on a waiver-ineligible requirement, and `gateReadyItemPRReference`
  re-anchors that head to the execution export and publication outcome.
- **The 0062 data migration validates against its own scan.** Adding a
  column after a Go data migration that reads `attention_items` through the
  head-schema SELECT would have made that migration's candidate filter skip
  every item silently on a store still at 0061. It now authenticates the
  binding against the item it already scanned, projecting the missing
  column as NULL. Any later migration that adds an `attention_items` column
  inherits this constraint: the shared scanner's column list must line up at
  every schema version a Go migration reads it under.

## Rejected

- Copying the field into the pre-`pr_reference` legacy terminal digest in
  `fakepublication`: that shape pins a historical digest that excludes the
  summary too, and adding the detail would change every digest it
  authenticates.
- Freezing the fake-mode terminal digest: `fakepublication.TerminalDigest`
  hashes the whole item, so the new field moves it for every fake-mode
  terminal binding persisted before this change, as each earlier field
  addition did. Fake mode is a development path with no durable operator
  data to keep, so the precedent stands rather than a second frozen shape.
- Truncating nothing in the card: full 40-character SHAs do not fit a card
  row, so the card abbreviates a hex object name to twelve characters and
  the inspector keeps the full daemon values. Truncating every coordinate
  was rejected in review: an invalidation's axis also carries a base ref
  (`retargeted`) and a `repository_id#pr_number` identity
  (`identity_changed`), and a real GitHub repository ID puts the PR number
  at or past the twelfth character, so a shortened pair could render the
  bound and observed values identically and hide the change the row exists
  to name.

Revisit when `ProductionRequirementDefinitions()` gains a waiver-eligible
class: the degraded shape then becomes producible and needs an engine test
against a real waiver grant, and the SURFACES.md rule row should cite it.
