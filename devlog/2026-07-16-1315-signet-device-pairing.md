# Signet device pairing, credential verification, and revocation

**Work unit:** #67 (§5.14 tests 13–16). Consumes the #64 contract shapes
and #106's `GetDeviceSnapshot`.

Decisions:

- **Key and code policy live in the service, injected.**
  `NewService(st, opts...)` gains `WithPairingKey`/`WithClock`/`WithRand`;
  a nil key fails pairing closed (mint errors loudly, redemption is the
  same undifferentiated 403 as an unknown code, so misconfiguration
  cannot be probed). Rejected alternative: a load-or-generate key file
  helper inside signet; key material management is daemon-composition
  territory and would smuggle filesystem I/O into the service.
- **Code format (owner decision this session): 8 chars, Crockford
  base32, 10-minute TTL.** ~40 bits against an unauthenticated endpoint
  whose only oracle is one undifferentiated 403; 32 divides 256, so
  byte-modulo sampling is unbiased. Rejected: the spec example's 6
  digits (10^6 online space) and a 12+-char copy-paste token (the code
  must be hand-typed off a display-less host, §5.14). Typed input is
  folded onto the minted alphabet before hashing per Crockford's
  decoding rules (case, O→0, I/L→1, grouping separators), so
  transcription variants of a valid code redeem it (Codex review
  finding on #116).
- **Credential = sha256 of the whole token string**, per the
  deviceCredential scheme; the authorizer compares digests with
  `subtle.ConstantTimeCompare` after gating on `CredentialHash` kind,
  and re-reads the device row so a revoked device fails at the
  credential (401) before any handler runs.
- **Active-device gate sits inside Submit's write transaction, on the
  new-command branch only.** After the command-id replay lookup (the
  documented command-id-first contract) and before any item read, so a
  revoked device triggers no item-carrying rejection (StaleVersion /
  ClosedItem leak item state) and test 16 holds structurally: the
  replay branch never writes (`errReplay` rollback), so a retry returns
  the recorded result with no new side effect whether or not the device
  is still active. The in-tx read also closes the authorizer-to-commit
  TOCTOU window. Rejected: gating in the HTTP layer only (the service
  is a boundary other composition will call) and gating before the
  replay lookup (would break test 16's "may return its recorded
  result").
- **Revoke is read-first with a rollback sentinel.**
  `ValidateDeviceTransition` pins `RevokedAt` as immutable, so an
  idempotent re-revoke cannot re-write the row with a fresh timestamp;
  the replay path captures the recorded snapshot and abandons the Write
  (`errRevokeReplay`, the Submit `errReplay` pattern), so re-revocation
  bumps no revision.
- **Pair is one Write with the device row written before the code
  consumption**: `pairing_codes.device_id` has a foreign key to
  `devices(id)`, so consumption must follow creation; a lost
  single-winner race still rolls the whole transaction back
  (verification: the FK failed loudly in the first test run when the
  order was reversed).

Refute-first pass (three independent read-only lenses: token-parsing /
authorizer, secret-leakage / pairing, revocation semantics). No blocker
or real-severity finding survived; core claims (whole-token digest
binding defeats confused-deputy and non-canonical-base64 attacks;
single-use under replay and concurrency; no plaintext in any error,
row, or response beyond the grant; undifferentiated 403 across
unknown/expired/consumed/no-key; replay branch structurally write-free
for test 16) were attacked and held.

- **Confirmed, fixed:** case-sensitive `Bearer` matching rejected
  RFC 7235-conformant lowercase/multi-space schemes (fail-closed
  interop bug) — replaced with case-insensitive scheme parsing plus
  1*SP handling, tests added.
- **Confirmed, fixed:** a backwards clock let Revoke persist a
  quietly domain-invalid `revoked_at < paired_at` row (PutDevice's
  transition gate does not re-run full Validate) — Revoke now clamps
  `revoked_at` to `paired_at`, so revocation (a security action) never
  blocks on skew and the row stays valid; test added. Codex's #116
  review caught the same class unswept in Pair (a rewind past the mint
  made `consumed_at < created_at` fail validation as a 500); Pair now
  clamps the redemption instant to the code's `created_at`, and all
  three `s.now()` sites were re-enumerated (mint is internally
  consistent, needing no clamp).
- **Confirmed, fixed (Codex #116 round 3):** the revoke handler
  discarded the authenticated caller, so a device revoked after the
  middleware authorized its request could still commit a revocation of
  the owner's remaining device (the same authorize-to-commit window
  Submit's gate closes). `Revoke(ctx, caller, target)` now re-gates
  the caller inside the revoking transaction; self-revocation still
  passes (the caller is active when it commits), and a revoked
  caller's attempt is 403 with the target untouched.
- **Accepted by decision (timing):** the authorizer's not-found early
  return and the pairing unknown-vs-existing path lengths leak
  existence, never secret bits; device IDs are 128-bit random and the
  secret-bearing digest compare is constant-time over equal-length
  operands, so neither oracle is material.
- **Accepted by decision:** a 128-bit device-ID collision surfaces as
  a differentiated 500 with full rollback (not a consumed code);
  `WithPairingKey` imposes no strength floor (key generation is the
  composition unit's, and an empty key already fails closed); a
  backwards daemon clock can lengthen a code's window (daemon-
  controlled, fails toward availability); a revoked device retrying
  its own command_id with a changed body sees 400 ErrImmutableConflict
  before the gate (it already owns that id; no write occurs).
- **Rejected by verification:** hypothesized cross-device
  confused-deputy, padded/std-alphabet/non-canonical token variants,
  pubkey-kind rows matched by digest tokens, display_name 400-vs-403
  code probing, modulo bias, and any pre-gate item-state leak to a
  revoked device were each tried and disproven (do not re-raise
  without new evidence).

Revisit when: a device list/sync projection lands (revoked-device read
policy is currently "401 at the credential, everywhere"; a policy that
lets a revoked device fetch its own recorded results would need the
enforcement-policy paragraph in the deviceCredential scheme re-read),
or when saddle needs the pairing code TTL/length surfaced as
configuration.
