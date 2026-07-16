# Signet attention deliveries and the ntfy channel

Work unit: #69 (signet lane, 1A.0's last unit). Scope:
`daemon/internal/signet`, `daemon/cmd/freeside-signet-dev`.

## Timing: persisted, recomputed transactionally (resolves #28's deferred question)

The timing-placement note (2026-07-14-2235-timing-placement.md) kept
`Timing` on `AttentionItem` and left persist-vs-derive-at-read to this
unit. Decision: **persist, re-derived via `WithTiming` in the same
Write transaction as the delivery row that changed it, with
`ItemVersion++`**, skipped when the summary is unchanged.

- Derive-at-read rejected: `timing` is a required field of the wire
  item, and sync invalidation and command acceptance both key on
  `entity_version`; a body drifting under an unchanged version would
  defeat cache invalidation (a client would never refetch the timing it
  displays) and blur what a command's `expected_entity_version` binds.
  One entity_version must mean one body.
- Unconditional re-put rejected: an aggregate-neutral event (a second
  device's later open) would churn item versions and staleness-
  invalidate in-flight prepared commands for nothing. The skip is
  tested (`TestTimingReputSkippedWhenUnchanged`).
- Owned consequence: an aggregate-moving delivery event can invalidate
  a prepared command; the existing 409-with-replacement path converges.
  This matches the #28 note's revisit condition — no staleness defect,
  because the aggregate can never lag its delivery rows.

## Two-transaction pipeline

`SubmitDelivery` commits the submitted row first, publishes to ntfy
outside any transaction, then advances to `channel_accepted` in a
second Write. External I/O inside a Write was rejected outright; a
crash or provider failure between the transactions leaves a
submitted-only row claiming exactly what happened, and a retry is the
next attempt number. The provider's 2xx populates `channel_accepted_at`
and nothing stronger ("delivered" does not exist; plan §4,
decision 11).

## Opened receipts are in-process only, for now

The contract has no client write path for delivery receipts (the
deliveries listing is explicitly read-only), and side-effecting the
deep-link GET would both change a contract endpoint's semantics and
conflate "client fetched item detail" with "user opened the
notification" — the dishonesty the status vocabulary exists to prevent.
`RecordDeliveryOpened` is the service boundary; the device-facing wire
path is a contract widening deferred to its own `kind:contract` unit.
Opened receipts replay idempotently, gate on the active device, and do
not require an open item: a late open of a resolved item is honest
telemetry (and §5.14 test 9's scenario).

## ntfy channel

- **Per-device topics are an HMAC of the device ID under a daemon-held
  key** (`fs-` + 32 hex chars). Hosted ntfy topics are capability URLs,
  so they must be unguessable; `Device` has no topic field, so the
  derivation is deliberately schema-free. Surfacing the topic to the
  device (through the pairing grant) is a deferred `kind:contract`
  unit. Rejected alternative: random per-delivery topics (the client
  must subscribe once, so the topic must be stable per device).
- **Payload is a generic hint** (owner decision, 2026-07-16): title
  "Attention needed", the attention type, and the Click deep link into
  canonical state. Item subject and reason text never reach the hosted
  third party; richer payloads can become configuration later. The
  accepted metadata surface, named so it is not re-litigated: the item
  ID (inside the deep link, unavoidably) and the priority (as the ntfy
  urgency level) do reach the provider.
- **Acceptance-race receipt is dropped by design**: if an opened
  receipt lands between the publish and the acceptance Write, the
  stronger recorded state wins and `channel_accepted_at` for that
  attempt is never recorded (first_accepted_at under-reports). Rejected
  alternative: a same-rank receipt-completing update, which would
  weaken the transition validator's "receipts are immutable, rank
  strictly advances" rule for one telemetry instant nobody consumes
  ahead of first_opened_at.
- Signet keeps a local redacting `Secret` for the optional ntfy token
  rather than importing publish's: same discipline, different lane
  territory. Hoisting one shared type is a contract-shaped cleanup for
  a later unit if a third copy threatens.
- Unconfigured or misconfigured channels fail closed before any write.

Revisit when: a real client subscribes (the topic-surfacing contract
unit lands), when APNs arrives in Phase 2 (the channel seam is
`notificationFor` + `publish`), if delivery volume ever makes the
recompute's full-row listing a measured cost, or if a second
delivery-row writer appears outside signet — the timing-never-lags-rows
invariant is service-layer only, and a direct store writer bypasses the
recompute.
