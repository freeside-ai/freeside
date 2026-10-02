# Review Drift Contract: Stop Cause and Drift Card Facts

Work unit #1667, plan §7 "Review Drift". This note covers the fourth part of
the contract that #1048 split: the review stop cause as a shared enum with a
`drift_audit` member, and typed card facts on the
`review_diminishing_returns` item. It adds the shape and the store's re-check
only. #1051 produces the facts and routes on them; #1052 renders them. The
third part's note is `2026-10-01-1547-disposition-supersession-contract.md`.

## Chose an Explicit Null Over an Omitted Key

`review_diminishing` renders `null` when unset, like every other card fact.
#1048's yield-history fields had to be omitted instead, because their column
is compared byte for byte with the body. This field has no column. The row
check compares lookup columns with the decoded body, never the body's bytes,
and the write path re-encodes an older row before its idempotence compare. A
test removes the key from a stored row and shows the row still reconstructs,
loads as a decision, and converges on an unchanged replay.

The one stored commitment an explicit null would break is the
fake-publication terminal digest, which hashes the whole item.

## Chose a Shadowing Wrapper Over a Third Frozen Mirror

`fakepublication.TerminalDigest` marshals a wrapper that embeds the item and
shadows `review_diminishing` with a field that is always omitted, so the
digest bytes are what they were before the field existed. The two earlier
item shape changes each added a frozen copy of the old item in
`terminal_legacy.go`.

Rejected: a third frozen mirror. It is a full copy of the item to maintain,
and it buys nothing here: a terminal item is a ready item and can never carry
these facts, so no digest needs to cover them. A test pins the digest for
three fixed vectors; it was written against the old code first and passes
unchanged with the wrapper.

Revisit when a terminal item type can carry review-diminishing facts. The
wrapper would then hide a fact the digest should bind.

## Chose to Gate at Write and Every Reconstruction, Not Only at the Decision Load

The card facts are a copy that a client renders without the records behind
it. The store re-proves the copy wherever it writes or reconstructs the item,
which is where the other card-fact gates run.

Rejected: checking only in `ReviewDiminishingDecision`. The inbox and list
reads never load the decision, so they would serve an unproven copy.

The gate proves three things:

- The cause is the one the item's own `Reason` binds.
- That binding is this item's: the id the bound run and round mint, the
  item's run subject, and its head.
- Drift facts equal the stored `DriftAudit` they name, for that run and round.

A missing audit reports `ErrParentKeyMismatch`, not `ErrNotFound`. Callers
read `ErrNotFound` from an item load as "no such item", and the engine
swallows it there.

An item without facts passes. Items stored before the field have none, and
the review-escalation producer writes a diminishing item whose `Reason`
carries no binding.

Rejected: re-deriving the cause at every reconstruction. The gate proves the
card cause equals the cause the item's own `Reason` binds, which is what the
issue contract specifies. It does not recompute the cause from the review
record, policy, yield history, and dispositions, so an item whose `Reason`
and facts were rewritten together passes. Four reasons:

- The cause authorizes nothing. Every command path goes through
  `ReviewDiminishingDecision`, which re-derives it with
  `EvaluateReviewConvergence`.
- The exposure is not new. The `Reason` sentence already told clients the
  cause, with the same check, before the typed field existed.
- Sharing the decision load recurses: the load calls `GetAttentionItem`,
  which runs this gate. The load also walks prior rounds' decisions, so one
  inconsistent prior round would make an inbox item unreadable.
- A `drift_audit` cause cannot be re-derived before #1051, so the gate would
  exempt the one cause this unit adds.

## Refused the Simplification Promise Until #1051

`simplification_on_continue` tells the user that `continue_under_policy` will
run the audit's reversals. That holds only when the reversal list passed the
route gate, and the gate arrives with #1051. Until then nothing can re-prove
the claim, so the store refuses it with
`ErrReviewDiminishingSimplificationUnproven`. The domain and the API allow
the bool under `over_hardened`, so #1051 changes the store rule, not the
contract.

## Left `drift_audit` Decisions Failing Closed

The decision load re-derives the stop cause with `EvaluateReviewConvergence`,
which never returns `drift_audit`. An item bound to that cause reads as an
item, with its facts, but does not load as a decision. A test pins both
halves. #1051 teaches the load about the audit, and adds the cause check to
the supersession authority, the first unit in which such a decision can load.

## Kept All Three Verdicts Representable

The facts accept `converged`, `over_hardened`, and `stuck`. Plan §7 routes a
`converged` verdict onward instead of parking, so a `converged` card may
never exist. Whether it can park is #1051's routing question; the contract
does not encode a routing rule it cannot check.

## Refute-First Findings

The store gate is a returned-object trust boundary, so an independent
reviewer tried to break it before the commit (`docs/agent-workflow.md`
§refute-first).

- **Confirmed and fixed: the binding was not tied to the item.** The first
  gate compared the facts with the `Reason` and the audit but accepted any
  self-consistent pair. An item on run B could carry a `Reason` and facts
  that both named run A's real audit, and a client would see run A's verdict
  on run B's card. Only a daemon producer bug or a tampered row reaches it,
  which is the case the gate exists for. The gate now applies the same
  id, subject, and head checks the decision load applies.
- **Confirmed and fixed: a replay with facts over a row without them was a
  stale write.** No producer sets the facts yet. The first one to do so would
  fail when it replayed an open item stored before the upgrade.
  `PutAttentionItem` now drops the facts on a same-version replay when the
  stored row has none, as it does for the other card facts. The
  implementation plan had left this out because the engine returns before
  re-putting an existing diminishing item; the cost of matching the other
  facts is three lines, and the failure would surface as an upgrade bug in a
  later unit.
- **Disproved: an ungated read path.** Every reconstruction goes through
  `scanAttentionItemSnapshot` or `scanAttentionItemHistory`. The one other
  body decode, `getAttentionItemBindingRecord`, feeds two internal gates and
  returns no item.
- **Disproved: a body write outside `PutAttentionItem`.** Only migrations
  write bodies elsewhere. Dropping or changing the facts on a version advance
  is an immutable-transition error.
- **Disproved: a gate failure read as "no item".** The missing-audit mapping
  holds, and a tampered audit row reports a row inconsistency.
- **Disproved: an incomplete comparison.** The digest is the lookup key, the
  four copied fields are compared, and the bool is refused. Nil reversals
  fail domain validation before the gate.
- **Disproved: facts validation drifting from the audit's.** The rules mirror
  `DriftAudit.Validate`.

## Kept the Unit Whole

Planning estimated about 2,300 changed lines against a budget the unit
exceeds, and proposed one seam: the cause first, the drift facts second. The
plan recommended against it because both halves stay over budget and the
second regenerates the client, both digest constants, and the screenshot
digests again while #1051 and #1622 wait behind another contract review. The
owner started the unit whole by fiat (`Handle #1667`); no split was applied.

## Revisit When

- #1051 adds the route gate: relax the bool refusal to "true only when the
  gate re-proves the reversal list", and let a `drift_audit` decision load.
- #1051 teaches the decision load about the audit: reconsider whether the
  reconstruction gate should share a cause re-derivation that does not load
  the item, now that every cause can be re-derived.
- The card needs a finding's title. Each reversal carries only the finding
  id, `undo`, and `rationale`; a title is another contract change.
