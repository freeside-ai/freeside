# The Blocked Attention Type Is Retired (#1925)

The owner chose on 2026-10-11 to retire the `blocked` attention type instead
of removing only the duplicate card it raised. #1925 began as "show one card
for a waiting specification approval". Planning found that the approval wait
was the type's only producer, so retiring the producer left a card kind
nothing could raise. Plan revision 84 records the change.

## Owner Decisions

- **Retired the type over fixing only the duplicate.** Planning recommended
  removing the second card and keeping the type for the plan's external
  waits. The owner dropped the type, in line with the 2026-10-10 remark that
  blocked cards "in general seem pointless".
- **Kept history over deleting it.** A migration closes each open `blocked`
  item as superseded. The API schema and the domain keep `blocked`,
  `BlockedWaitKind`, `BlockedWait`, and `blocked_on`, marked retired.
  Deleting the rows and the name was rejected: every stored row is validated
  when it is read, so a `blocked` row would stop loading once the enum member
  went; sync has no way to tell a client that an item was deleted; and no
  migration has ever deleted an attention item.
- **Dropped the waiting thresholds over renaming their signal.** Plan §5.12
  keeps two budgeting clocks. A long wait for an approval, PR checks, or an
  external review raises nothing. Keeping the thresholds for a later unit was
  rejected: nothing implemented the PR-check or review thresholds, and the
  one implemented threshold produced the duplicate.
- **Kept one unit over a split.** Planning recommended a small fix that
  stops the engine raising the item, then a contract unit for the type. The
  owner kept the whole change in #1925.
- **Added no waiting text to the approval card.** The row keeps the age it
  shows today. Notification and escalation for a long wait belong to #1924.

## Implementation Choices

- **The domain refuses an open `blocked` item; a closed one stays valid.**
  `AttentionItem.Validate` carries the rule, so creation, every store write,
  and every read share it. A row reopened outside the daemon fails to read
  instead of reaching a client. Refusing the type outright was rejected
  because closed history must still decode.
- **The migration is plain SQL in the shape of migration 0074.** It rewrites
  the status column and the body together and advances the item version, the
  entity version, and the sync revision, so a connected client receives the
  close as an ordinary update. Status is not part of the decision-surface
  preimage, so the rewrite invalidates no stored surface, and a blocked item
  offers no action to record a decision against.
- **The retirement also runs ahead of the three backfills that decode
  items.** Migrations 0058, 0059, and 0070 decode every attention item with
  the current `Validate` and skip a row it refuses. An open `blocked` row in
  a store older than those migrations would reach the head without a
  decision surface or task binding, fail every read, and take the inbox
  listing with it. Each of the three applies migration 0096's statements
  first, read from the embedded file so the close has one definition; 0096
  then finds nothing open. Stating a minimum upgrade version was rejected:
  retained stores at schema 69 hold open `blocked` rows.
- **`waiting.spec_approval_attention_after` is ignored, not rejected.** A
  resolved policy stored before the change carries the key, and its digest
  binds the stored text, so the parser must still accept it.
- **The store's blocked-wait binding gate stays.** It still checks a closed
  item's wait reference when the item is read or rewritten.

## Verification Findings

An independent refute-first pass over the full diff, before the first push.

- **Confirmed and fixed: an upgrade from before schema 70 stranded an open
  `blocked` row.** The older backfills skipped the row the new rule refuses
  (the choice above). Copies of eight retained stores, schema 57 to 80 and
  holding 15 `blocked` rows of which nine were open, now upgrade with every
  `blocked` row readable and superseded.
- **Disproved: a migrated row could fail a read gate.** Status and item
  version are outside the decision-surface identity, the blocked-wait gate
  ignores status, and no other table or trigger caches the status.
- **Disproved: an open `blocked` row could reappear.** No runtime path
  constructs or reopens one, restore requires an equal schema version, and
  an older binary refuses a newer database.
- **Allowed by the issue contract: a closed card keeps its present-tense
  wait text.** It still reads "Waiting on ..." with a duration counted to
  now. #1925 says a closed item keeps the card text it has today.

## Superseded Notes

- `2026-07-16-1214-actionless-blocked-items.md` and
  `2026-07-15-1855-signet-action-policy.md` made `blocked` the one type that
  offers no action. The empty action set remains valid on a closed item, but
  no open item uses it. The policy table's `blocked` entry stays so closed
  items pass the same gate.
- `2026-10-08-1028-refined-decision-cards.md` gave `blocked` a row in the
  card table. The clients keep its presentation for closed items only.
- `2026-09-30-2038-held-work-notice.md` chose a `system_health` advisory over
  a `blocked` item for held work. That choice stands, and the rejected
  alternative no longer exists.

Revisit when the operator needs to be told about a long external wait (PR
checks, an external review, or an approval left open). The answer then is a
notification or escalation on the item the run already waits on (#1924), not
a second card.
