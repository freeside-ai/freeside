# Notifications Default Off, the Inbox Marks the Unviewed

Owner decisions of 2026-11-09, reached while sorting out which kinds of
notification Freeside needs. Three kinds were on the table: device
notifications (ntfy today, native push later), in-app pop-ups, and a visual
mark on inbox items the operator hasn't looked at. The finding that shaped
every choice below: runs generate far more attention items than deserve a
push, so a notification per open item per device is noise, and the owner
finds notifications less important than the plan's Phase 1 framing assumed.

## Decisions

- **Device notifications are off by default, per device.** Chose a
  per-device boolean, off by default, over today's rule (every active device
  is owed one notification for every open item,
  `daemon/internal/signet/delivery_dispatch.go`) and over a richer filter by
  posture or item kind. One boolean is the smallest change that stops the
  flood and leaves room for a filter once the flood is measured. Existing
  paired devices are not backfilled on; the owner turns a device on in
  Settings. A backfill rule is more mechanism than the install base
  justifies.
- **The preference is its own record, not a field on `domain.Device`.**
  `daemon/internal/domain/device.go` is on the Tier A client-authority list.
  A notification preference grants no authority, so it lives in a record
  keyed by device id and leaves the authority type alone.
- **Notification setup leaves pairing.** The sheet offered once after an
  iPhone pairs (`PhoneNotificationSetupSheet` in
  `app/Sources/FreesideCore/FreesideRootView.swift`) is no longer offered.
  The steps stay in Settings' Notifications pane, shown once the device's
  notifications are on. This supersedes the placement in
  `devlog/2026-10-11-0911-phone-notification-setup-steps.md`; the step
  content there stands.
- **No in-app pop-ups.** Not built. If ever added, they are a client
  preference with no daemon change, and the OS toggle already covers native
  alerts.
- **The inbox marks unviewed items, as default behavior, never a setting.**
  With notifications off, the inbox is how the operator finds what needs
  them. Plan §4 already makes the inbox canonical and notifications hints,
  so turning notifications off costs nothing in correctness.
- **Viewed state is daemon-held and shared across devices, not
  client-local.** Chose deriving it from the `card_opened` comprehension
  event any device emits when a decision card appears
  (`app/Sources/FreesideModel/DecisionModel.swift`, `emitCardOpened`) over a
  per-device local flag. The inbox syncs across Mac and iPhone, and the mark
  must agree on both. Rejected delivery `opened_at` receipts as the signal:
  they fire only on a notification tap, not when the inbox is opened
  directly. The client still clears the mark locally the moment a card opens,
  so a slow or failed telemetry write never makes the inbox feel laggy.
- **Viewed resets when the decision surface changes.** `card_opened` is bound
  to the item's decision-surface digest, so a revision marks the item
  unviewed again. Chose this over binding to the item id, which would show an
  item once and never again after a material change.
- **Dot plus bold title, not either alone.** The Mail and Messages
  convention, and a redundant encoding. Bold alone is hard to scan in a list
  of varying row lengths and reads backwards while most items are new; a dot
  alone is one small mark against the selection highlight. The gutter stays
  reserved so rows don't shift, and the dot's color does not encode posture.

## Deferred

- **A posture or kind filter, or coalescing (one notification per run):**
  until the flood is measured by item kind. The spike unit filed from this
  note owns the measurement and the proposal.
- **A sidebar count and Dock badge:** after living with the mark. A badge
  would need the same opt-out as a notification.
- **Moving APNs later in the roadmap:** plan §11 sequences APNs into Phase 2
  and gates the Developer Program on it. If the demotion holds, that can
  move; it is a material plan change and its own PR.

## Revisit When

- The spike shows one item kind dominates the volume: a kind filter may then
  be a better default than the bare boolean.
- Native macOS push arrives: the OS toggle and the device switch must agree,
  and the Mac's switch gets a visible effect.
- Anything starts reading viewed state as authority: it is a Tier C hint
  derived from telemetry and must stay one.

Follow-up: #2930, #2931, #2932, tracked on #2933.
