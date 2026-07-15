---
run: manual
stage: device-pairing-contract
date: 2026-07-14
branch: feat/device-pairing-contract
---

# Sync-surface contract: device pairing credentials and revision heartbeat (issue #64)

Spine-role session, fiat-assigned. #64 is the current head of the Wave 1
contract chain (#55 and #28 merged; tracking issue #83). Mandatory note:
`kind:contract` plus a credential trust-boundary surface. Declared
paths: `daemon/internal/domain`, `daemon/internal/store`,
`daemon/migrations`, `api/`. This lands the two §5.14 widenings Wave 0
recorded as consumer-deferred (2026-07-13-2118-api-schema.md,
2026-07-14-1556-wave0-adversarial-review.md); the consumers are #66,
#67, and #72. Contract only: enforcement (expiry at redemption,
revocation stopping commands) stays with the signet units, but every
shape here exists to make §5.14 tests 11 and 13–16 enforceable.

## Decisions

- **Concrete token format (owner decision, 2026-07-14):**
  `fsd1.<device_id_b64>.<secret>` (version prefix, the unpadded
  base64url encoding of the device identifier, 256-bit unpadded
  base64url secret). The daemon stores only the whole token's sha256
  digest; the embedded device_id routes verification to that device's
  stored credential, so auth is one keyed lookup plus a constant-time
  compare. The identifier segment is encoded, not raw: two Codex
  findings on #87 walked the class (a raw id with dots breaks
  dot-splitting; a raw id with whitespace or other non-token68
  characters breaks the Authorization header itself), and encoding
  makes the format total over the identifier space (dot-free segments,
  every character bearer-legal) instead of pinning per-symptom parse
  rules. Rejected: a fully opaque token keyed by its own hash — leaks
  nothing structurally but keys the credential table by hash instead
  of device and complicates revocation-by-device; constraining
  DeviceID's alphabet domain-wide — a wider contract change than the
  token needs, and the domain deliberately keeps IDs opaque. Landing
  the format now honors the api note's constraint that pairing
  endpoints and the token format move together.
- **Both credential kinds ship now (owner decision, 2026-07-14):**
  `credential_hash` and `device_public_key`, the two plan-sanctioned
  non-reusable shapes; v1 pairing issues only hashed bearer tokens.
  Rejected: hash-only with a later widening — the issue's acceptance
  wording ("hash or public key only") treats the vocabulary as the
  structural exclusion, and the unexercised member costs one enum
  token, not an abstraction.
- **Device / DeviceCredential split.** The synchronized Device body
  physically carries no credential field, and DeviceCredential has no
  API schema, so neither the sync surface nor the wire can represent
  credential material, even hashed (the same unrepresentability move
  the api unit used for agent evidence). Pairing codes persist only a
  keyed digest, HMAC-SHA256 under a daemon-held pairing key that never
  enters the store: the plaintext is displayed on the daemon host once
  and discarded, and redemption re-derives the digest from the
  presented code. Keyed rather than bare (a round-3 Codex finding on
  #87): a displayed code is short, so an unsalted fast hash would leave
  a leaked database or checkpoint offline-brute-forceable while a code
  is valid; the token's bare sha256 is fine by contrast because its
  secret is 256-bit. Key management and the exact derivation land with
  signet enforcement (#67); the stored shape (`sha256:<64 hex>`) is
  unchanged either way.
- **Store tiers follow the #38 lattice** (2026-07-14-1519): devices are
  synchronized (PutDevice on WriteTx; revocation must be observable
  through sync for tests 15–16); pairing codes and credentials are
  internal bookkeeping (no as_of_revision; minting a code must not
  invalidate client caches) with non-`Put` names on InternalTx so the
  reflection invariant stays honest. `putImmutable`/`existingBody`
  moved down to InternalTx unchanged; every exported `Put*` remains
  WriteTx-only.
- **One device per code is structural, not procedural:** UNIQUE
  device_id on pairing_codes plus a conditional consume
  (`UPDATE ... WHERE device_id IS NULL`, exactly one row) decide the
  winner; identical replays converge; the consumption pair is one-way
  by `ValidatePairingCodeTransition` (test 14, and 13's consumed half).
- **Revocation is a terminal status, never an erasure:**
  `deviceStatusSuccessors` admits no exit from revoked, revoked_at is
  immutable once recorded, and command rows are untouched, so a
  revoked device's recorded results stay retrievable (test 16's shape).
  Regaining access is a new pairing, hence a new device.
- **No retroactive FKs** from `commands.device_id` /
  `attention_deliveries.device_id` to devices: applied migrations are
  digest-pinned and immutable, SQLite FK addition means a table
  rebuild, and test 15 needs a status read at submission time, which
  an existence FK cannot provide. Recorded in the 0005 header too.
- **Pairing and revocation are endpoints, not ClientCommands.**
  `POST /pairing` is the one `security: []` operation (the requester
  has no credential; the displayed code is the authenticator);
  rejection is one undifferentiated 403 so an unauthenticated caller
  cannot probe code validity. `POST /devices/{device_id}/revoke` is
  the credential control surface: it invalidates what commands
  authenticate with and must not depend on the revoked device's
  cooperation, so folding it into the ClientCommand path (a decision
  on an attention item) was rejected; widening ClientCommand is
  exactly the non-goal #22 defers.
- **display_name is mutable; identity and paired_at are not** (renaming
  a device is not a lifecycle event). Consumption-after-expiry is not a
  structural fault in the domain type: expiry is redemption policy
  (signet), and a row recording a late consumption must stay readable
  evidence.

## Refute-first verification (credential-leak surface)

Two independent fresh-context lenses (one on domain/store, one on the
API contract) were prompted to disprove: (a) no schema/store/API path
persists or emits reusable plaintext; (b) no operation sequence
reactivates a revoked device, moves its revoked_at, or destroys
recorded command rows; (c) no mint/consume sequence yields two devices
from one code or un-consumes it.

**Confirmed and fixed before handoff:**

- Claim (a) was refuted in letter: nothing shape-validated credential
  material, so an arbitrary plaintext string persisted wearing the
  digest's field (probed: `RecordDeviceCredential` with a raw token,
  `MintPairingCode` with the plaintext code as code_hash). Fixed:
  Validate now requires `sha256:` + 64 hex for credential_hash material
  and for every code_hash (`ErrPlaintextCredential`); fixtures moved to
  real digests.
- A crafted pre-consumed mint fabricated a redemption that never went
  through the single-winner path and burned the device's one-code
  UNIQUE slot. Fixed: mint rejects any consumed input; consumption is
  recorded exclusively by ConsumePairingCode.
- A device's second code consumption surfaced as a raw SQLite UNIQUE
  failure callers cannot errors.Is. Fixed: mapped to
  ErrImmutableConflict (the UNIQUE stays the backstop).
- Spec/docs contradictions the widening introduced or exposed: the
  "no operation is unauthenticated" comment and the "single mutation
  surface" absolutes now name their exceptions; the domain/store
  "observable through sync" wording now says revision accounting lands
  here and the read surface lands with its consumer; the securityScheme
  no longer both promises retrievable results and a blanket-invalidated
  token (the revoked-retry either/or is signet's, test 16 permits both).

**Rejected by verification (do not re-raise):** revoked reactivation,
revoked_at rewrites, and record destruction (blocked by terminal
successors, immutability rules, and the absence of any delete path);
double consumption, un-consume, concurrent two-writer consume, re-mint
divergence (single winner held under probes); column/body time-format
mismatches; the putImmutable/existingBody receiver move (every Put*
remains WriteTx-only, reflection-guarded); Device oneOf/discriminator
fidelity and example-golden identity; revoke's 200-on-already-revoked
vs the terminal transition (an identical replay passes without a
write).

**Accepted by decision (residual, with rationale):**
device_public_key material has no format validation until its consumer
pins a format (v1's exercised path is hash-only and shape-enforced);
the spec defines no 401/403 auth-failure shapes anywhere, so the
revoked-submission rejection shape lands with signet enforcement;
Error.message is free-form, so the pairing 403's undifferentiation is
normative text, enforced in signet.

## Deferrals

Device list read endpoint, devices in BootstrapSnapshot, the public-key
pairing flow, and token rotation all wait for a consumer, per #22's
standing mechanism.

Revisit when: the signet device unit (#67) implements enforcement — if
tests 13–16 need a shape this contract lacks, the widening is a new
`kind:contract` unit, not an in-place edit; and when a second client
platform arrives, revisit whether the heartbeat should carry per-entity
revision hints.
