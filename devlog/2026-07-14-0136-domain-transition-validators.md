---
run: manual
stage: domain-transition-validators
date: 2026-07-14
branch: refactor/domain-transition-validators
---

# Domain transition validators (issue #21)

Spine-role session, fiat-assigned #21 (`kind:contract`, `lane:spine`), a
Wave 0 follow-up escalated from the store/migrations unit
(`2026-07-13-1806-store-migrations.md`, already marked `-> Refs #21`).
The store's four persisted-aggregate transition guards (fixed bindings,
append-only history, forward-only lifecycle) lived as private helpers in
`daemon/internal/store/entities.go`; this lifts them into
`daemon/internal/domain` as exported `ValidateXTransition(old, updated)`
validators so the rules live with the vocabulary they constrain and a future
writer (engine, importer) reuses one definition. Behavior-preserving. Declared
paths held: `daemon/internal/domain`, `daemon/internal/store`, `devlog/`. PR #29.

## Decisions

- **Two class sentinels, not one-per-rule.** `ErrImmutableTransition`
  (a fixed field or recorded history would change) and `ErrStaleTransition`
  (an update does not advance a version/lifecycle) are the minimal shape that
  still lets the store recover the exact sentinel each rule produced.
  Rejected one-sentinel-per-rule (premature granularity: no consumer needs it)
  and reusing the store's `ErrImmutableConflict`/`ErrStaleWrite` from the domain
  (would invert the dependency: domain must not know the store's boundary
  errors).
- **The store keeps its public error contract via a `mapTransition` seam.**
  Each `Put*` calls the domain validator and maps the two domain classes back
  onto `store.ErrImmutableConflict`/`ErrStaleWrite` with a double-`%w` wrap, so
  the sentinel stays `errors.Is`-matchable while the domain detail rides along.
  This is what lets the store's guard tests pass **unchanged** — the chosen
  behavior-preservation oracle (Acceptance #2).
- **Byte-identical replay short-circuits stay in the store.** They compare
  persisted `body` bytes (a store-persistence concern) and must run *before* the
  validator: an unchanged update does not advance a version, so a validator
  would (correctly) reject it as stale. The domain validators are documented as
  requiring the caller to converge replays first.
- **One `transitions.go`, not scattered per-aggregate.** The old-vs-new
  "transition" shape is genuinely new and cross-cutting (shared `jsonEqual`),
  so grouping the four validators + moved helpers reads better than splitting
  them across the four aggregate files; each still sits in the same package as
  its field-level `Validate`.
- **`deliveryRank` moved verbatim**, keeping its no-`default` switch so the
  `exhaustive` linter still forces a new `DeliveryStatus` to be ranked.

## Verification

- `go build/test/vet ./...` and `golangci-lint run` (v2.12.2, matching CI): green.
- Acceptance #1: new table-driven `domain` tests mirror the five store guard
  tests (`transitions_test.go`), asserting the domain sentinels.
- Acceptance #2: `store/entities_test.go` unchanged and green.
- Acceptance #3: domain golden fixtures untouched (no serialized surface added).
- **Refute-first differential pass** (this refactor is on a data-integrity /
  deserializer-return-trust path): a throwaway harness reconstructed the base
  (a6b13b6) store guards as independent reference functions and compared their
  accept/reject verdict *and* error class against the new validators over
  300,000 fuzzed `(old, updated)` pairs — **zero mismatches**. Harness deleted,
  not shipped.
  - Confirmed: behavior preserved decision-for-decision (differential + the
    unchanged store tests).
  - Rejected-by-verification: none (no candidate divergence survived).
  - Accepted-by-decision: replay short-circuits remain in the store; two-class
    sentinel granularity.

## Review rounds

- **Round 1 — one Codex P2, accepted and folded.** The transition validators
  omitted an identity check, so a caller that does not fetch `old` by key could
  pass a different aggregate as a legal successor (the store always fetches by
  key, so it was safe, but these are now the reusable contract). Added a
  fixed-identity guard to all four validators (`ErrImmutableTransition`), swept
  as a class per the finding's own "siblings need the same" note: run,
  conversation, and item on `ID`; delivery on its `(item, device, channel,
  attempt)` key. Folded into the validators commit with its tests; store
  behavior unchanged (matching keys), so store tests stay green.

## Notes

- No new queue items. #21 drains via `Closes #21`; its `-> Refs #21` provenance
  marker already exists in the source entry.
- Env only (not a repo change): this session installed `go` 1.26.5 and
  `golangci-lint` 2.12.2 via Homebrew to match `daemon/go.mod` and the
  CI-pinned lint version; the toolchain was absent locally.
