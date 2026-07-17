# Opened receipts get a wire path outside the ClientCommand surface

Work unit: #130 (kind:contract, spine). Scope: `api/`,
`daemon/internal/signet`, `daemon/cmd/freeside-signet-dev` (mechanical
test adaptation), `app/` (generated client), `docs/plan.md` (the §5.14
narrowing below), `devlog/`.

## The receipt is not a ClientCommand (narrows plan §5.14)

Owner decision (2026-07-16): the delivery opened receipt is an
idempotent `PUT /attention/items/{item_id}/deliveries/{channel}/
{attempt}/opened` with no request body, returning the recorded
`AttentionDeliverySnapshot`; it does not ride the ClientCommand
surface. A receipt is monotonic telemetry about the past — "this
notification was opened" — not a judgment on canonical state, and the
ClientCommand contract's `expected_entity_version` precondition would
reject legitimate receipts whenever the delivery row advanced
(submitted → channel_accepted) between notification and open. Retrying
a receipt needs convergence, not staleness rejection.

This required narrowing plan §5.14's "Every mutation is a
ClientCommand" to "Every judgment-bearing mutation …", naming the
receipt alongside the two exceptions the spec already carried (the
credential control surface and digest-addressed attachment upload).
The plan change is material and is the direct subject of this contract
PR, called out in its body per the document-gating rule.

- Rejected: a ClientCommand payload for the receipt — wrong semantics
  (above), and it would force a judgment-shaped envelope
  (command_id, expected_entity_version) onto a fact record.
- Rejected: side-effecting the deep-link GET — re-rejected; #69's note
  already named the conflation ("client fetched item detail" is not
  "user opened the notification").
- Rejected: POST with a body carrying identity — the attempt is fully
  identified by (item, channel, attempt) in the path plus the
  authenticated device; a body is a second place for identity to
  disagree. The device is never a payload or path field: a device can
  open only its own deliveries, and another device's attempt is 404,
  indistinguishable from absent.

## Wire semantics delegate to `RecordDeliveryOpened`

The endpoint delegates to the #69 boundary unchanged: idempotent
replay (rollback via `errReplay`, so no revision movement), the
active-device gate, and no open-item gate (a late receipt for a
resolved item is honest telemetry; canonical rendering stays the
deep-link GET's concern, §5.14 test 9). A normally revoked credential
fails authentication with 401 before the handler runs; 403 is the
residual race where authorization succeeded but the in-transaction
active-device re-gate observes the revocation, with no effect. The
operation documents that narrower 403 alongside its request-specific
400 even though the global security scheme, like the spec's other
authenticated operations, carries 401 without repeating it per
operation.

The wire wrapper needs `{as_of_revision, entity_version}`, which
`RecordDeliveryOpened` does not return. The service's wire method
performs the write, then builds the snapshot in a follow-up Read
(list + pick, the store's existing access pattern). Rejected:
capturing the snapshot inside the Write — the replay path rolls the
transaction back, so an in-tx snapshot would describe a state that
never committed; and a new single-row snapshot getter in
`daemon/internal/store` — a shared spine package outside this unit's
declared scope. The write→read gap is benign: `opened` is terminal and
receipts are immutable.

## Deep link carries channel and attempt (widens #69's metadata surface)

The notification's Click link becomes
`{ClickBaseURL}/attention/items/{id}?channel=ntfy&attempt={n}`: query
parameters on the canonical item URL, so the click target remains the
side-effect-free GET (§5.14 test 9) and the client derives the exact
receipt PUT from the link. Rejected: pointing Click at the receipt URL
(side-effects a navigation target); a custom URL scheme (the app has
no URL handling yet, and the daemon must not depend on it).

This widens the accepted ntfy metadata surface that
2026-07-16-1718-signet-deliveries.md froze at "item ID (inside the
deep link) and priority": the attempt number (and the constant channel
name) now also reach the hosted provider inside the Click URL. Owner
re-decision, 2026-07-16, made with #130's assignment: the receipt is
unreportable without attempt identity, and an attempt counter is the
same order of sensitivity as the priority level already sent.

Revisit when: a second delivery channel lands (the channel path
segment is a free string keyed to the AttentionDelivery channel field,
deliberately not an enum) or APNs arrives in Phase 2 (its receipt path
should reuse this operation, not grow a sibling).
