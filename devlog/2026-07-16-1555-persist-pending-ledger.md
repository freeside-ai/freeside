---
run: manual
stage: persist-pending-ledger
date: 2026-07-16
branch: feat/persist-pending-ledger
---

# Persist the pending-command ledger across app restarts (issue #115)

Saddle-lane deferral from #114 (source note:
2026-07-16-1030-saddle-cache-pairing.md, the "Accepted by decision"
scope cut). Declared paths: `app/` plus this note. Mandatory note: the
unit widens the persisted cache shape with command bodies
(credential-leak surface) and restores client mutation state from disk
(reconstruction trust boundary).

## Decisions

- **The ledger joins the one cache file; cursors become optional.**
  `CachedState` gains `pendingCommands` and its `cursors` turn
  optional, because the two now have different lifetimes: an epoch
  discard kills cursors and rows while an unsettled command still
  needs its verbatim resend (#115 acceptance 4). Rejected: a second
  file for the ledger (two atomic-write paths and a torn state between
  them for one small payload); and a fabricated "empty" cursor
  sentinel, which would invent an epoch the daemon never issued.
- **Restore coerces `.inFlight` to `.unresolved`.** No task awaits a
  restored command's response, so a command persisted mid-flight has
  failed ambiguously by the time a relaunch reads it; restoring it
  in-flight would suppress the retry affordance forever (only
  `.unresolved` offers retry). The coercion lives in
  `restorePendingCommands`, not at encode time, so the persisted file
  stays a faithful snapshot of the in-memory ledger.
- **Ledger mutations persist immediately via a store observer.** The
  existing `revisionObserver` fires only on canonical reads, and a
  sync round may never come between a submission and termination, so
  the ledger mutators fire a dedicated `pendingCommandsObserver` wired
  to the coordinator's `persist()`. Writes are tiny and at most a few
  per decision; no debounce.
- **Epoch discard re-persists the ledger from the in-memory store.**
  Both discard paths (heartbeat eager evict, adopt-time backstop) run
  through `discardCache()`, which now discards and then re-persists;
  `persist()` writes a cursor-less, row-less state carrying the ledger
  alone, and deletes the file outright when there is neither a cursor
  nor a pending entry. Eviction stays first so a lost re-save degrades
  to an absent cache (honest), never to lingering dead-epoch rows.
- **A corrupt ledger section loads as absent, not the whole file.**
  `CachedState` decodes the ledger with a forgiving `try?` while
  cursors and rows stay strict: the ledger is client mutation state
  whose loss costs one retry affordance, and it must not take the
  readable cache down with it. Whole-file corruption still loads as
  absent, and the format bump (1 → 2) retires pre-ledger files at the
  cost of one bootstrap; a pre-upgrade unresolved ledger did not exist
  to lose.
- **Entries for vanished items restore anyway**, matching the
  in-process `replaceAll` behavior: commitment is not readable cache,
  and the restored resend converges either way (recorded result
  replayed, or an authoritative 404 clears the slot).

## Refute-first verification (credential and outcome surfaces)

Two independent fresh-context lenses, given only the diff and the
issue's intent, tried to disprove (a) credential custody (no token,
Authorization content, or token-derived fragment can reach the
persisted file through the ledger) and (b) outcome integrity (restore
and the persist rework cannot fabricate or destroy a command outcome
or regress the pre-change cache semantics).

**Confirmed and fixed before commit:** the byte-level scan in
`thePersistedLedgerCarriesNoCredentialMaterial` originally checked
only exact-case fragments; extended to a lowercased scan plus the
token's base64 form. Completeness of the test, not a leak: the
property holds structurally (`ClientCommand` is a fixed-shape schema
with `additionalProperties: false` and no credential-shaped field; the
token's only sink is the per-request Authorization header).

**Confirmed and fixed in review (Codex P2 on #125):** restore trusted
two decoded fields the reconstruction boundary must re-gate. A
deployment-scoped cache survives a re-pair (lost credential, same
daemon), so a restored entry minted by the old `device_id` would
occupy the new device's slot and its verbatim resend would die at the
daemon's device gate — an authoritative rejection clearing a possibly
committed outcome as "not recorded". Restore now drops entries whose
`device_id` is not the store's device or whose dictionary key does not
match the command's `payload.item_id` (the same class: a decoded
binding trusted across the boundary).

**Rejected by verification (do not re-raise):** a token path into
`ClientCommand`, `AttentionItemSnapshot`, or `SyncCursors` (none
exists; `DeviceCredential.token` never reaches the store graph);
weakened iOS at-rest protection (the file-protection envelope is
untouched); observer re-entrancy or a persist during init (observers
wire after restore, and restore never fires them); the cursor-less
persist or the empty-state file delete destroying readable state (both
require a failed load or a completed discard first); dead-epoch rows
reaching disk through a ledger persist (a cursor-less save writes no
rows); partial ledger restore from a malformed section (dictionary
decode is all-or-nothing); a format-1 ledger lost by the bump (none
ever existed).

**Accepted by decision (residual, with rationale):** two concurrently
live coordinators sharing one cache file can overwrite each other's
persisted ledger (a stale session's persist drops or resurrects an
entry across a later relaunch). Production composes one coordinator
per process, the resurrect case degrades to a harmless replayed
resend, and the same multi-writer class already existed for cursors
and rows before this unit; defending it would need cross-process
coordination the disposable cache does not warrant.

Revisit when: conversations or runs join the persisted cache — the
ledger's immediate-persist observer and the cursor-less save shape
were designed for the item table only.
