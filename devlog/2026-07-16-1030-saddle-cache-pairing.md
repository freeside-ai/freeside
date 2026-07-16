---
run: manual
stage: saddle-cache-pairing
date: 2026-07-16
branch: feat/saddle-cache-pairing
---

# Saddle cache semantics, pairing, and revocation honesty (issue #72)

Saddle-lane unit after #71. Declared paths: `app/` plus this note.
Mandatory note: the unit builds the client credential surface (Keychain
custody of the device token) and reworks 401 handling on the
command-outcome path, both trust-boundary work. The real-daemon
convergence half of #72's acceptance is deferred: the daemon has no
composed listener or main binary, and #67 (signet pairing/revocation
enforcement) is unmerged, so every §5.14 client half here converges
against the extended in-process mock instead (user decision,
2026-07-16); the PR carries `Refs #72` and the issue stays open for the
convergence pass.

## Decisions

- **A SyncCoordinator above InboxStore, not a fatter store.** The
  coordinator owns the §5.14 cursor pair, the epoch, the disk cache,
  and the freshness claim; the store keeps what #71 built (snapshot
  table, version-monotonic upsert, pending-command ledger) plus two
  ingestion methods (`replaceAll`, `discardSnapshots`) and a revision
  observer. Cursors and persistence are session-scoped concerns that
  will also cover runs and conversations, so they sit above the item
  table. Rejected: growing InboxStore into the sync engine, which
  would have complected the item table with session lifecycle; and a
  coordinator-owned snapshot copy, which would have reintroduced the
  two-sources-of-truth problem #71 removed.
- **Freshness lives on the store; the negative states gate actions.**
  Views and DecisionModel already share the store, so the coordinator
  writes `freshness` there instead of every consumer holding a
  coordinator reference. Only `unreachable` and `unauthenticated`
  disable actions; `unvalidated` carries no signal (per-item
  validation decides), which keeps bare-store flows and the #71 tests
  meaningful rather than defaulting the gate open dishonestly.
- **The disposable cache is one atomic JSON file; the credential is
  Keychain-only.** Cursors plus item metadata snapshots, data-protected
  on iOS, corrupt-or-foreign loads as absent, epoch discard is file
  deletion. Rejected SwiftData/SQLite: a small Codable payload rebuilt
  wholesale by every bootstrap earns no database. The cache and the
  credential have opposite failure postures by design: cache loads are
  forgiving (a lost cache costs one bootstrap), credential operations
  fail loud (a silently lost token is an unpaired device with no
  signal). Attachment bytes never enter the cache, so the
  no-high-sensitivity-at-rest default holds by construction; the rule
  is documented at the store for when attachment rendering lands.
- **401 is not one more authoritative 4xx.** #71 cleared the pending
  slot on every undocumented 4xx. On the revocation path that would
  destroy a possibly committed outcome: a 401 on a lost-response
  resend proves nothing about the original attempt's commitment (§5.14
  test 16 lets the daemon serve a revoked retry its recorded result or
  reject it), so the resend path keeps the slot and surfaces
  `unauthenticated`; only a fresh first submission's 401 is treated as
  definitively unrecorded, because the credential gate precedes
  acceptance. The mock takes test 16's recorded-result branch so the
  client rendering path is exercisable; the daemon's branch choice
  stays with #67, and the client is correct under either.
- **One client spans pairing via a per-request token provider.** The
  bearer middleware consults its provider on every request and sends
  no header when the provider is empty, which is exactly the
  unauthenticated `POST /pairing` shape; custody stays with the
  provider's backing store. A pairing grant whose credential cannot be
  saved surfaces as "revoke and pair again", never as paired: the
  token appears exactly once, in the grant, and pretending otherwise
  would fabricate a working device.
- **Composition is launch-argument selected.** `AppSession` picks the
  wiring (`-FreesideServerURL` → Keychain + disk cache + live client;
  `-FreesidePairingDemo` → enforcing mock with a seeded code;
  default → permissive mock with a pre-paired identity, preserving the
  existing demo surface). The pairing gate derives purely from
  credential-store state, so the same session logic serves all three.
- **Live state is deployment-scoped (Codex P1 on #114).** A device
  credential is minted by one daemon, so the live composition keys the
  Keychain service — and, sweeping the class, the disk-cache
  directory — on a normalized deployment key of the server URL: a token
  paired with daemon A structurally cannot be attached to a request for
  any other `-FreesideServerURL`, and one deployment's cached rows never
  render against another. Rejected: a runtime mismatch check inside the
  token provider, which guards the same hole procedurally where the
  scoped lookup removes it.
- **Epoch eviction happens on observation, not on recovery (Codex P2
  on #114).** The heartbeat's epoch-mismatch branch discards the cache
  before attempting the re-bootstrap: §5.14 forces cache eviction on
  epoch change, and discarding only inside a successful adopt left an
  outage window rendering (and persisting, hence relaunching into)
  pre-restore rows. The adopt-time discard stays as the backstop for a
  mismatch first observed by a direct bootstrap.

## Refute-first verification (credential and outcome surfaces)

Two independent fresh-context lenses, given only the diff and the
§5.14 intent, tried to disprove (a) credential custody (token never
outside the Keychain, never logged or cached; no paired state without
stored credential) and (b) command-outcome integrity (no fabricated or
destroyed outcomes under 401/epoch/race paths; ledger survival).

**Confirmed and fixed before handoff:**

- A bootstrap response held open across an epoch rotation could land
  late and win the cache back for the dead epoch (no recency guard in
  the coordinator, unlike the store's refresh/validation generations);
  the same hole let a same-epoch stale bootstrap regress the
  full-snapshot cursor. Fixed with a sync-round generation counter
  (only the newest round adopts or writes freshness) and pinned by a
  held-response regression test. Exposure required a second concurrent
  sync driver and self-healed at the next heartbeat, but the persisted
  cache could carry the stale epoch until then.

**Rejected by verification (do not re-raise):** token reaching disk,
UserDefaults, logs, or error strings (its only sink is the
Authorization header); Authorization leaking across a daemon-
controlled redirect (tested empirically: Foundation strips it);
`.ready` without a stored credential, double-pair races, and
identity/credential mismatch; pending-slot clears without authoritative
proof, applied states without a received CommandResult; inconsistent
cache saves under failed sync; the mock's revoked-retry replay writing
state or leaking across devices; corrupt cache shapes loading as
anything but absent.

**Accepted by decision (residual, with rationale):**

- The pending-command ledger does not survive app restart (the disk
  cache persists cursors and rows only). In-process it survives
  bootstrap, epoch discard, and navigation; across a relaunch an
  unresolved command's retry affordance is lost and a record-only
  action could be re-minted as a new command. Deliberate scope cut for
  this unit; escalated as a deferral issue rather than widening the
  cache shape mid-unit.
- `KeychainCredentialStore` sets `kSecAttrAccessibleAfterFirstUnlock`
  without `kSecUseDataProtectionKeychain`, so on macOS the item lands
  in the login keychain where the class is a no-op (login-keychain
  default protection applies); iOS gets the declared class. Forcing
  the data-protection keychain requires a keychain-access-groups
  entitlement the unsigned CI/dev builds don't carry. Revisit with
  signing/packaging (plan §10).
- A transient Keychain read error is indistinguishable from an absent
  credential at session init (pairing UI) and presents as
  unauthenticated in the token provider. The recovery for a truly
  lost credential is the same either way (#64: revoke and re-pair).
- Partial reads carry no epoch on the wire, so a refetch racing a
  restore can briefly fold a new-epoch row or revision into old-epoch
  state; the next heartbeat's epoch check discards it. Client-side
  unfixable without a per-resource epoch, which #22's on-demand
  mechanism can add if it ever matters.

Revisit when: #67 lands and a daemon listener is composed — run the
real-daemon convergence pass (tests 1, 2, 8, 11, 13–16 client halves
against `freesided`) and close #72; and when attachment rendering
lands — the high-sensitivity memory-only default documented in
CacheStore.swift becomes enforceable behavior.
