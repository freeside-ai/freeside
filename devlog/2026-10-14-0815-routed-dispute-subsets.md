# Routed Disputes With A Smaller Displayed Set

Issue: #2009. The owner approved the persistence amendment on 2026-10-14.

## Contract Decisions

Chose a complete round binding in `finding_ids` and a nonempty displayed
subset in `findings`. #1990 needs to ask only about findings still waiting
for a person. This revises #1016's complete-rows guard, recorded in
[its decision note](2026-10-13-1004-human-dispute-adjudication.md), which closed
an incomplete-batch hole. A human command still answers every displayed
finding, rejects other choices, and preserves every omitted prior entry.

Chose the first stored non-human adjudication as evidence that each omitted
finding has an entry. Every successor preserves the finding batch. Reading
the latest artifact could recurse: human artifact validation reads the card,
and card validation would read that artifact again. Reject a human first
revision before reconstruction instead of adding a transaction re-entry flag.

The store checks membership, not the omitted entry's route. Selecting which
findings require a person remains the engine's job in #1990. A stored route
can still park a run or require remediation; omission alone authorizes neither
publication nor a claim that remediation finished.

## Persisted Displayed Set

The refute-first probe confirmed a gap in the initial plan. With just the
subset relaxation and prior-entry check, deleting one row from a stored
full-list card was accepted by both current and history reads. The same
probe passed against the unchanged base because the count check rejected it.
The existing decision-surface identity does not contain these rows.

Chose a separate immutable displayed-ID record over changing the general
decision-surface digest. It is written transactionally when the card first
becomes routed and checked before write replay returns and on both read
paths. Row contents still authenticate against their existing source records.
A legacy card with no binding may gain its first one; existing bindings keep
their ordinary immutability rules.

Migration requires the old complete-row invariant before backfilling a routed
card. A malformed or partial legacy dispute fails the transaction rather than
gaining authority from the relaxed domain contract. Non-routed cards gain no
record. The owner explicitly approved adding `daemon/migrations/` to scope
for this fix. No public field or decision-surface identity changes.

Independent refute-first review found no further reachable defect. The
regressions cover both reconstruction paths after raw row deletion or set
substitution, missing or corrupt set records, immutable writes and replays,
human-answer read-back, and migration rejection with rollback. The human
first-revision case is refused before any recursive reconstruction occurs.

Revisit when an adjudication revision can change its finding set, a round can
have multiple routed dispute items, or a routed card's displayed set must
change after creation.
