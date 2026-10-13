# Expire Abandoned Tasks' Approval and Recovery Cards

For #1985, chose daemon-side retirement over a client-side abandoned badge.
The stored open decision is the problem: every client and reader should see
that no decision is owed. The existing `expired` status closes the card
without inventing an operator decision, a new API field, or a migration.
The normal closed-item gate already rejects later decisions.

`resolved` requires an approve or stop command in the specification reader;
`superseded` requires request-changes evidence. `dismissed` represents an
operator decision today. `expired` is already a no-op in the specification
reader and does not require a `decided_at` timestamp. Retirement advances the
item version and explains abandonment in its reason, preserving its history.

Only specification approvals and execution-failure recovery cards expire.
Abandonment releases a slot without setting a cancellation fence, so the
engine still reconciles its runs. Other closed-card readers require a
decision, including finding adjudication, review diminishing returns, and
publication holds. Quarantine notices would be raised again while their hold
remains. Retiring all cards would therefore break reconciliation or produce
replacement notices. The existing engine recovery-card prefix distinguishes
failure cards from quarantine notices.

Slot release commits before the retirement transaction. An old card can fail
current persistence policy, so combining the writes would prevent the
operator from freeing a stuck slot. The cost is a retry after a crash between
the writes or a failed retirement. Retirement is all-or-nothing within its
own transaction; repeated calls also retire eligible cards raised later.
The command rolls back each unchanged write to preserve the store revision.
The shared `store.AbandonTask` operation keeps its existing behavior because
confirmed Stop and intake reconciliation call it too.

The retirement helper re-reads the task's current lifecycle inside its write.
A later explicit admission therefore protects the new episode from a delayed
retirement. A task lookup uses the same dual-view SQL guard as the run lookup,
with the binding name substituted, plus a Go-decoder pass for deeply nested
JSON that SQLite cannot parse. Corrupt lookup columns must fail closed rather
than hide a card and falsely report successful retirement.

## Refutation Checks

- Disproved by command and integration tests: retirement accepts a late
  decision, changes another task's cards, or makes reconciliation fail on
  expired approval or recovery cards.
- Disproved by corruption tests: retargeted or cleared task columns, changed
  status/type columns, duplicate or folded JSON keys, and deeply nested bodies
  can hide a selected task's item from the lookup.
- Disproved by the policy-failure test: a stale second card can undo the slot
  release or commit only the first card's retirement.
- Allowed by the issue contract: other kinds remain open, new cards can appear
  after abandonment, and a discussion submitted earlier can still receive a
  reply. Abandonment certifies no execution shutdown.

Revisit when abandonment stops its workflow or every remaining reader has a
defined outcome for a card closed without an operator decision.

Follow-up: #1993 covers remaining kinds, new cards, and pending discussions.
