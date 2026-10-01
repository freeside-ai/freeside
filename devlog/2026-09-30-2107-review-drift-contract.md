# Review Drift Contract: Growth Floor Types

Work unit #1048, plan §7 "Review Drift". This note covers the first part of
the contract: the growth policy key, the `growth_without_blockers` stop
cause, per-round diff metrics in yield history, and the adjudication input
field. It adds types and persistence only; #1049 adds the behaviour.

## Owner Decisions on 2026-10-01

- **Split #1048 in four.** Planning put the whole contract at about four
  times the unit size budget. The owner applied the plan's split: #1048
  keeps this part, #1665 takes the `DriftAudit` artifact, the audit policy
  keys, and the verdict in yield history, #1666 takes the disposition
  supersession record, and #1667 takes the stop cause in the API and the
  card facts. Rejected: one pull request for all four parts.
- **Bind a reversal through the disposition, not the model.** A drift
  audit's reversal entry doesn't name the remediation that fixed its
  finding. The supersession record copies `RemediationInvocationID` from
  the `fixed` disposition it supersedes, and the store checks the copy
  against that row on write and on every read. The entry's "what to undo"
  text stays advisory prose, and the engine derives the files to touch
  (#1051). Rejected: a model-supplied binding, which the store couldn't
  verify. #1666 implements this.

The spine, not the owner, chose where the new contracts sit in the
serialized chain: #1048, #1421, #1665, #1666, #1667, #1622. That keeps the
slot the owner gave #1421 on 2026-09-30. The cost: #1050 through #1053
now wait for #1421. That decision put #1421 after #1048 so the drift units
could proceed during it, and only #1049 still can.

## Chose an Omitted Field Over an Explicit Null

`ReviewYieldRound.DiffMetrics` is `json:"diff_metrics,omitempty"`, against
the daemon convention that an optional contract field renders an explicit
null. The store compares each stored item's yield history byte for byte
with its re-encoding and deep-equal with the history re-derived from the
review rows. An explicit null would change the bytes of every history
stored before the field existed, so no earlier `ready_for_final_review` or
`review_diminishing_returns` item would load. The API declares the property
optional and not nullable for the same reason: an absent property is how a
gap shows.

The existing goldens are unchanged and keep pinning the bytes of a history
without metrics. A separate fixture pins the new shape. A store test loads
an item whose stored history is the literal pre-change JSON.

Rejected: a migration that rewrites stored histories. Item bodies are
immutable and digest-bound, and rewriting them to add a null would be a
larger change than the field.

## Chose a Metrics Table Over a Field on `ReviewRecord`

The metrics live in `review_round_diff_metrics`, one immutable row per
`(run_id, round)`, with a foreign key to the round's review record.
Rejected: a field on `ReviewRecord`. A review record's body is immutable
and compared on replay, review-state recovery rebuilds records from
reviewer evidence that doesn't hold diff counts, and old records would need
the same omitted-field treatment as yield history. A separate row leaves
all of that alone, and a round without a row is a gap.

Each pair is a `domain.DiffStats`, the type the ready card already uses for
a base-to-head count. A new pair type would have duplicated its fields and
validation.

## Chose to Bind Both Pairs to the Review Records

The store never trusts the commits a metrics row names. On write, on every
read, and again when yield history is derived, it checks that the
cumulative pair is the round's recorded base and head, and that the round
pair starts at the previous recorded round's head (or at the bound base
when the run has no earlier round) and ends at the same head. The domain
check adds what needs no records: both pairs end at one head, and two pairs
over the same base are the same comparison.

The counts themselves are not re-derived. The store has no repository
access; #1049's engine computes them from git and is the authority for
them.

## Chose Never to Backfill Metrics

Metrics are written when the round is recorded, never later. An item
snapshots its yield history, and the store requires the snapshot to equal
the re-derived history, so a row that appears after an item read the round
makes that item fail to load.

- **Refused by the store:** a put for a round that already has a later
  round, recorded or failed. Failed rounds take the run's round numbers,
  so either proves the run moved on.
- **Not refusable by the store:** a put for the latest round after an item
  snapshotted it. The store can't tell that write from a correct one. A
  `review_diminishing_returns` item then fails to load
  (`ErrParentKeyMismatch`) instead of showing a history it never displayed,
  and so does the run's later convergence state, which re-reads earlier
  decisions; a test pins the item failure. A `ready_for_final_review` item
  snapshots the final round, which never has a successor, so the engine's
  ready-item comparison would refuse it the same way. The row is immutable,
  so neither failure heals.

So #1049 must write the row in the transaction that records the round,
before any item reads the history. Rejected: a guard that scans items for a
snapshot of the round. It would couple this accessor to the item table to
catch a caller bug that already fails closed.

## Chose to Add the Adjudication Field Without Sending It

`FindingAdjudicationInput.DiffMetrics` exists so #1049 can populate it, but
`AdjudicatorSite.Call` doesn't send it. The site's field allowlist, its
request map, and the Claude driver's field-count check must change
together, and sending the field changes what the adjudicator sees. That is
behaviour, and it belongs to #1049, which must also edit
`daemon/internal/claudeinference`.

## Refute-First Findings

An independent reviewer tried to break the accessor before it was
committed (`docs/agent-workflow.md` §refute-first).

- **Confirmed and fixed:** the backfill guard counted only later review
  records, so a put for a round followed by a failed round was accepted.
  It now counts later failures too.
- **Confirmed and fixed:** no test reached the derivation's own re-check
  of its metrics argument. One does now.
- **Allowed by decision:** a review record inserted for an earlier round
  after a later round's metrics exist makes that metrics row inconsistent,
  and the whole-table list then fails for every run. The engine allocates
  the latest round plus one, so only direct store seeding can do it, and
  failing closed is the store's rule for an inconsistent row.
- **Allowed by decision:** a row rewritten with forged counts and a
  recomputed digest loads. The store can't re-derive counts; this is the
  same limit as a `FindingAdjudication` body, and a snapshotted item still
  fails its equality check.
- **Disproved:** a row leaking into another run's list (the body and key
  cross-check and the run filter stop it); a body changed under its digest
  loading (`ErrRowInconsistent`); a put below a decision's round
  (refused); the foreign key blocking or orphaning anything (nothing
  updates or deletes a review record, and restore copies tables with
  foreign keys off); legacy bytes changing (no existing golden moved, and
  a stored explicit null fails closed); the accessor and the derivation
  disagreeing on the previous round across round gaps, configuration
  segments, or base changes; a set policy key degrading to off.

## Revisit When

- A second optional field joins `ReviewYieldRound` (the drift verdict,
  #1665): it needs the same omitted-field rule.
- The owner wants the drift units ahead of #1421: move #1665, or all
  three new contracts, before it in the chain.
- #854 lands a way to reconstruct stored review state. If stored histories
  can then be rewritten safely, explicit nulls become possible.
- The engine needs metrics for a round recorded without them. That is a
  backfill, and it needs a design that keeps snapshotted items loading.
