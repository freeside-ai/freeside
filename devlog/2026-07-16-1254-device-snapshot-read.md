# Device reads carry store-stamped snapshot metadata

**Work unit:** #106 (contract; serialized after #96 in #83's chain).

The pairing 201 and revoke 200 responses render the API's
`DeviceSnapshot{as_of_revision, entity_version, device}`, and those two
fields are stamped by the store's own Puts. Deriving them in signet
would duplicate the store's private revision-stamping invariant, the
same gap-class #91 closed for commands, so the store grows
`ReadTx.GetDeviceSnapshot` and `GetDevice` delegates to it (the
`GetCommand`/`GetAttentionItem` pattern).

Decisions:

- **Mutable-entity consistency range, not write-once.** Commands pin
  `entity_version == 1` because their rows never update; devices are
  mutable (revocation bumps the version via `putDeviceSQL`'s ON
  CONFLICT increment), so the reconstruction gate is the
  `GetAttentionItemSnapshot` model: `entity_version >= 1 &&
  as_of_revision >= 1`, cross-checked with the existing ID/status
  column consistency. Rejected alternative: pinning `== 1` at read
  time and relaxing later would have made the first revocation a
  read-breaking event.
- **No new scanner helper.** Devices have a single-row read path (no
  list/sync projection yet, an explicit #106 non-goal), so the
  snapshot logic lives inline in `GetDeviceSnapshot` rather than a
  `scanDeviceSnapshot` split the attention-item path needed for its
  list reads.

Revisit when: the device sync/list projection lands (then a scanner
split mirroring `scanAttentionItemSnapshot` becomes the right shape).
