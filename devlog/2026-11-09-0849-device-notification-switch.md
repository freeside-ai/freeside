# A Per-Device Notifications Switch, Off by Default (#2931)

Work unit: #2931. Scope: `api/`, `daemon/`, `app/`, and this note.
Implements the first decision in
`devlog/2026-11-09-0820-notifications-default-off.md`. Tier A by path: the
route's one registration line in `daemon/internal/signet/http.go`, and the
list field and helper in `pairing.go` and `device.go`, sit on the
client-authority list. The switch itself is Tier B state: it grants no
authority.

## Decisions

- **The switch is on `DeviceListEntry`, not `Device`.** `Device` mirrors
  `domain.Device`, the record the client-authority gates read. The list
  entry already carries `last_seen_at`, another fact kept beside the device
  row, so `notifications_enabled` joins it there. Changing it moves the
  server revision, so other clients reload their Devices pane, but not the
  device's `entity_version`.
- **`PUT /devices/{device_id}/notifications` follows `revokeDevice`.** The
  target comes from the path and the caller from the credential. Any active
  device may set any active device's switch, as any may revoke any. A
  revoked or unknown target is 404: a revoked device has no switch. A
  request for the stored value writes nothing and moves no revision, so a
  replay never moves the instant the switch went on.
- **The preference is written through `store.Write`.** The device list reads
  it, so a change must move the server revision the heartbeat reports
  (`daemon/internal/store/tx.go`, the InternalTx comment).
- **Turning a device on owes it only what opens afterwards.** The sender
  measures from the preference's `updated_at`, the same rule pairing
  already applies, so turning a device on never floods it with the items
  already open. Turning it off stops retries too.
- **An item with no `CreatedAt` is owed nothing.** Only items from before
  the field existed lack one, so they predate every switch. Every
  production item constructor stamps it.
- **The Mac has a switch with nothing behind it.** Nothing on the Mac
  listens for ntfy, so turning it on has no visible effect yet. Chose a
  uniform switch over hiding it on the Mac: the daemon's rule is per
  device, and native macOS push (plan §11, Phase 2) will give it an effect
  (the owner note's Revisit When). The Mac's pane says nothing listens
  there yet.
- **The sender re-checks the switch in each attempt's write.** A pass reads
  every switch once, then records each attempt in its own write and
  publishes it, one pair at a time, so the gap between the read and a
  late pair's write grows with the pass. The issue requires that turning a
  device off records no further attempt, so the attempt's write re-reads
  the switch, with the same rule the pass applies (`switchOwes`).
- **The switch gates the sender, not every send.** `SubmitDelivery` (which
  the dev harness `cmd/freeside-signet-dev` calls to notify one device on
  purpose) and `freesided notify-test` (which checks a phone's setup)
  ignore it. Both are explicit operator actions about one device, and the
  test notice lets a phone be checked before its switch goes on.
- **A revoked device lists off.** Revocation leaves the preference row, so
  the list reads the switch only for an active device.

## Departures From the Plan Comment

- **The Devices pane's switch sits on its own labelled line,** not beside
  Revoke. At phone width the row already wraps the This Device chip, and a
  third control broke it mid-word.
- **`notifications-phone-hosted` replaces the `notification-setup-phone`
  surfaces** rather than leaving nothing in their place. It draws the
  Notifications pane switched on, on ntfy's own server, before any
  notification was opened: the variant only the removed sheet covered.
- **Two tests outside the declared paths change.** The freesided SIGKILL
  recovery test turns its paired device on through the new route, since it
  waits for a notification. The production review upgrade test in
  `daemon/internal/integration` drops the new table before it replays every
  migration since 0067.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong. Each claim is
recorded with its outcome.

- **Confirmed and fixed: a device turned off mid-pass got further
  attempts.** The pass's later pairs were recorded from its earlier read.
  Fixed by the re-check above; `TestDeliverOwedStopsMidPassWhenTurnedOff`
  turns devices off during the pass's first publish.
- **Confirmed and fixed: a revoked device listed its stored switch.**
  Turning a device on, then revoking it, listed it on. Fixed in the
  daemon's list and the mock; both now have a test.
- **Confirmed and fixed: the app's switch flickered back after a
  change.** It showed the requested value until the reload after the
  change finished, though the row already held the answer.
- **Disproved: a caller can act on a device it shouldn't.** The caller
  comes from the credential and the target from the path, as in
  `revokeDevice`, and any active device may set any active device's
  switch by design.
- **Disproved: a revoked caller or target slips through.** The caller's
  active gate runs again inside the writing transaction (403), and an
  unknown or revoked target is 404. Both are tested.
- **Disproved: the body decoding can be bypassed.** The strict decoder
  refuses unknown fields, trailing data, duplicate keys, and mistyped
  values; null, `{}`, and an empty body leave `enabled` unset, which is
  400. Authentication runs first. A duplicate-key case joined the tests.
- **Disproved: a replay moves the switch-on instant.** The replay aborts
  the write, so neither the revision nor `updated_at` moves. Tested.
- **Disproved: the switch grants authority.** Only the device list, the
  sender, and the setter read the preference. `gateActiveDevice`, the
  request authorizer, and `domain.Device` are unchanged.
- **Declined: a re-arm test.** Turning a device off and on again moves
  `updated_at`, and an item created before it is owed nothing. That is
  the created-before-switch case the `owedDeliveries` table already
  covers, so a separate test would repeat it.
