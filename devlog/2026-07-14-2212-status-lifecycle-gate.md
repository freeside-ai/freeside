---
run: manual
stage: status-lifecycle-gate
date: 2026-07-14
branch: fix/status-lifecycle-gate
---

# Gate command acceptance and item transitions on item status (issue #55)

Spine-role session, fiat-assigned. #55 is the Wave 1 contract-chain head
(#55 → #28 → #64, tracking issue #83), relabeled `kind:contract` by the
planning sweep because its acceptance adds a domain terminality rule. No
open PRs, no competing claim; the other open contract units are
downstream in this chain (#28, #64) or unscheduled (#22). Declared
paths: `daemon/internal/domain`, `daemon/internal/store`.

The finding (Wave 0 second-pass adversarial review, reproduced by
execution): `PutCommand` never checked item status, so a new command
bound to a resolved item's *current* version/head/digests was durably
recorded as a decision on a closed item; and
`ValidateAttentionItemTransition` had no status ordering, so
`resolved → open` at an advanced version reopened a decided item.

## Changed assumption vs the recorded deferral

The 2026-07-14-1230 note deferred status gating on the rationale that
"a resolution bumps the version, so a decision on a superseded item is
already caught as stale." That holds only for commands bound to
pre-resolution versions. A command prepared against the resolved item's
current bindings passes `BindsSameAs` exactly; version advance cannot
signal closure at the current version. The deferral's condition
("a separate lifecycle concern") has now been implemented as its own
unit; no prior owner decision is overturned, the version-bump argument
is recorded as insufficient so later units don't inherit it.

## Decisions

- **Terminality is a status-change rule, not a full item freeze.**
  `itemStatusSuccessors` (the issue's table-driven rule, spelled as a
  default-less switch so the enabled `exhaustive` linter forces a future
  status to declare its successors rather than silently defaulting to
  terminal; the `deliveryRank` precedent) lets open move to any terminal
  status, keeps same-status version advances legal, and admits no
  successor out of resolved/superseded/dismissed/expired. Rejected a map
  literal (invisible to the linter, so a new status would strand items)
  and rejected freezing terminal items entirely: the acceptance asks
  only for status ordering, and the command gate independently covers
  the decision surface of a mutated-but-still-terminal item.
- **Reuse `ErrImmutableTransition`; no third sentinel.** The
  transitions file's two-class contract distinguishes "refetch and
  rebase can fix this" (stale) from "recorded history would change"
  (immutable). A terminal outcome is recorded history: no refetch makes
  reopening legal. This also leaves the store's `mapTransition`
  untouched; the rejection surfaces as `ErrImmutableConflict`. Rejected
  a third sentinel as a contract widening with no caller that would
  branch on it.
- **Status check sits after the version check** in
  `ValidateAttentionItemTransition`: a stale copy diagnoses as stale
  first, preserving the existing expectation that resolved-v2 → open-v1
  is a stale write, not a terminality violation.
- **No `Terminal()` predicate on `ItemStatus`.** The successor table is
  the single encoding of terminality; a second predicate could drift.
  The store gate's contract is "not open", not "terminal", so it needs
  no predicate either.
- **The `PutCommand` openness gate runs before `BindsSameAs`.**
  `StaleCommandError` hands back a replacement item and invites a
  rebind-and-retry; on a closed item that retry can never succeed, so
  closure is the more fundamental rejection and reports first (pinned
  by a fixture: a stale-bound command on a closed item gets
  `ErrClosedItem`, not `ErrStaleCommand`). Consequence: the prior stale
  fixture, which conflated resolution with version advance, now
  advances an open item so it tests staleness purely.
- **`ErrClosedItem` / `ClosedItemError` mirror the stale pair** (store
  sentinel + struct carrying the canonical item, `Is`/`As` matching).
  Placed in the store, not domain: like staleness, openness-at-
  submission is a cross-check against the live item, which
  `domain.Command` cannot see. Idempotent retries short-circuit before
  the item load, so §5.14 test 4 is unaffected.
- **`api/openapi.yaml` untouched.** No HTTP surface consumes
  `PutCommand` in Wave 0; #65 owns the API-visible mapping of the new
  rejection (the existing 409 replacement-item shape likely fits).

Revisit when: a legitimate reopen/undo flow or a non-terminal non-open
status (e.g. snoozed) enters the lifecycle (the successor table and the
"not open" gate encode today's open-xor-terminal set); or when #65/#23
place the signet-layer acceptor and decide how `ErrClosedItem` maps to
the API.
