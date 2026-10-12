# A Timing Change Keeps the Item's Version (#1946)

The owner chose on 2026-10-11 that a notification delivery or an opened
receipt no longer raises an attention item's `item_version`. The write still
updates the item's `timing` and still raises the row's `entity_version`.

This revises one part of `2026-07-16-1718-signet-deliveries.md`: its
`ItemVersion++` on the timing write. That note's reasoning is about
`entity_version` ("one entity_version must mean one body"), and it still
holds. Its skip of an aggregate-neutral event also stays.

## Why the Old Rule Failed

Engine, store, and signet records bind work to an item's exact
`item_version`: a pending discuss intent, a discussion waiting for its reply,
and every decided item's "the command's version plus one". None of them has a
replacement path, and several answer a mismatch with `ErrParentKeyMismatch`,
which ends the engine loop in a durable stop.

The 2026-07-16 note owned one consequence of moving the version, a client's
prepared command meeting a 409 and converging. It did not consider engine
bindings, and until #1924 nothing in production wrote a delivery row. With the
sender running, an open item's version moved on submission, on acceptance, on
each retry, and on a tap; the acceptance and the receipt have no open-item
gate, so a decided item's version moved too.

## Owner Decisions

- **Stopped the version moving over rebinding the engine.** Binding engine
  records to the decision-surface identity was rejected. Planning found about
  30 exact comparisons in two forms the identity can't express, because a
  discuss, a reply, and a decision all leave the identity unchanged on
  purpose (`docs/plan.md` §4). Each site would need its own new binding, and
  the persisted intent payloads would change format.
- **Edited no comparison.** Every version check in the engine, the store, and
  signet is unchanged, so each still refuses a real change to what the
  operator decided on.
- **Left clients alone.** Signet rejects a stale prepared command on
  `entity_version`, which a delivery still raises, so the 409 path behaves as
  before.

## Implementation Choices

- **The store now protects timing; the version check used to.** A stale
  writer that read an item before a delivery would once fail on the moved
  version. It now passes that check, so `PutAttentionItem` keeps the stored
  `timing` whatever the caller passed, on a version-advancing put and on a
  same-version replay alike. `timing` changes only through
  `PutAttentionItemTiming`, which reads the stored item and replaces that one
  field. Trusting callers to carry timing forward was rejected: every caller
  that builds an item from scratch would have erased it.
- **The timing write has its own transition rule.**
  `ValidateAttentionItemTimingUpdate` requires an unchanged version and no
  other changed field. Relaxing `ValidateAttentionItemTransition` to accept a
  same-version timing change was rejected: every other writer would inherit
  the exception.
- **The timing write refreshes no derived field.** An ordinary put
  re-projects the task name from the task and re-derives the recommendation,
  both from state that moves without the item. The timing write keeps the
  stored values instead. Refreshing them was rejected: a receipt after a task
  rename would then change the stored name under an unchanged version, and
  `requireUnchangedFeedbackItem` reads that as drift. A client still sees the
  current name, which every read projects.
- **`item_version` now means what the operator decides on.** A stored body
  can change under one `item_version`, in `timing` only. A reader that needs
  "the body changed" uses `entity_version`.

## Finding: Whole-Item Comparisons

With the version no longer moving, a comparison of a stored item against a
rebuilt one sees a delivered item at the same version with different timing.
A type-aware sweep of `reflect.DeepEqual` on `domain.AttentionItem` found two
that did not set timing aside, both ending in a durable stop:

- `compatibleTerminalItem` (`daemon/internal/engine/fake_publication.go`),
  which production publication recovery uses. It accepted a delivered item
  only because the delivery had advanced the version, so this change alone
  would have broken it.
- `requireUnchangedFeedbackItem`
  (`daemon/internal/engine/operator_feedback.go`), when a receipt lands
  between a pass's read of the item and the transaction that consumes the
  answer.

Both now ignore `timing`. A third lead, the replay branch of
`recordProductionDeliveryRefusal`
(`daemon/internal/engine/production_workflow.go`), is unaffected: it compares
a stored item, which always carries the store-derived decision surface,
against a rebuilt one that never does, so timing cannot change its answer.
The comparisons that already set timing aside are unchanged.

Stored items are not repaired. A version a delivery advanced before this
change stays advanced.

## Refute-First Findings

The store writer is a persistence trust boundary, so an independent reviewer
tried to prove the change wrong before it was committed.

- **Confirmed, fixed:** the timing write refreshed the task name and the
  recommendation (the choice above), and a test now pins a rename followed
  by a receipt.
- **Confirmed, fixed:** an integration test planted a delivery with the old
  write, `ItemVersion++` through `PutAttentionItem`, which the store now
  ignores. It uses the timing write and expects the version unmoved.
- **Allowed:** the timing write trusts its caller's summary and does not
  check it against the delivery rows. Its one caller derives the summary from
  those rows in the same transaction, and `PutAttentionItem` accepted any
  timing before this change.
- **Disproved by reading and by running the tests:** caller timing reaching
  an existing row through `PutAttentionItem`; the timing write creating an
  item or moving its version; an unchanged summary writing a row; a raw SQL
  writer of `attention_items` touching timing or relying on the version
  moving; a cache or staleness check keyed on `item_version`; a path that
  reuses an item ID to reset timing; an edited or loosened version
  comparison.

Revisit when: a field other than `timing` needs to change without moving
`item_version`; or a reader is found that treats an unchanged `item_version`
as an unchanged body.
