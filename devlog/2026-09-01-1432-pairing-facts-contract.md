# Pairing Facts on the Pairing Surface (#923)

Contract change: `POST /pairing/preview` reports, for a live pairing code,
the four operational facts the pairing screen shows before redemption, and
`PairingGrant` gains the same `facts` object. `ConnectionMode` (`loopback`,
`tailscale`) and `DeviceScope` (`operator`) join the enum registry. The
store schema and `Device` do not change.

## Decisions

- **A preview endpoint over facts on `GET /health`.** The facts belong to a
  code (its expiry) and to a not-yet-paired requester, so they ride the
  pairing surface, keyed by the code, rather than the health surface every
  caller can read without one. Chose a separate operation over widening
  `POST /pairing` with a dry-run flag: a flag on the consuming endpoint
  invites a client bug that consumes when it meant to look.
- **The preview shares redemption's liveness rule and its 403.** One
  `pairingCodeRedeemable` predicate serves `PreviewPairing` and `Pair`, and
  both wrap `ErrPairingRejected` undifferentiated. The preview is therefore
  no finer a validity oracle than `POST /pairing` already is, which is why
  the contract records rate limiting as a non-goal rather than a gap: the
  code space (8 Crockford base32 characters, 10-minute life) is not
  guessable at interactive rates through either endpoint.
- **Host facts are process-fixed and fail closed.** `signet.WithHostFacts`
  carries the OS hostname (read once at start) and the listener-derived
  connection mode; a service composed without them refuses preview and
  pairing with a configuration error, before any code could be consumed.
  Chose this over an empty-string default because the wire contract's
  `minLength: 1` would then be satisfied only by accident, and over a
  runtime hostname read per request because the name is a display label
  whose stability across one process matters more than freshness.
- **Connection mode derived from the bound listener, not configured.**
  `listenPrivilegedWith` admits exactly two address classes, so
  `connectionModeOf` maps loopback to `loopback`, Tailscale-owned to
  `tailscale`, and refuses anything else as a composition defect. A third
  bound class (the §5.19 relay) adds a member here, not a flag.
- **Hostname as the display identity, unconfigured.** `os.Hostname()` can
  read as `Bens-MacBook-Pro.local` on macOS; acceptable for this unit. It is
  explicitly not the §5.9 host identity and carries no key material. An
  operator-configurable name is a follow-up if the default reads badly on
  real hosts.
- **One-member `DeviceScope`.** There is no device role model, so `operator`
  is the only scope and the client renders fixed copy for it. Registering the
  enum now gives the pairing screen a fact to state and a narrower scope a
  place to land without a second contract change to the pairing surface.
- **The client previews once per typing pause, not on completeness.**
  `PairingView` restarts a 400 ms task on every code change and previews
  when it elapses, clearing the facts on any rejection without saying why
  (the daemon never does either). Chose this over gating on the minted
  8-character length because the client should not encode the daemon's
  code format, and the mock and demo sessions use shorter codes.
- **`daemon/cmd/freeside-signet-dev` is in scope.** The issue's declared
  paths name `daemon/cmd/freesided` only, but fail-closed host facts make the
  dev harness unable to pair without fixed facts, so it takes deterministic
  loopback facts in the same unit. Surfaced in the PR as a scope addition.

## Revisit when

- A relay or a second real reachability mode exists (plan §5.2 keeps that
  seam as prose until then): `ConnectionMode` gains a member and the
  derivation stops being a two-way switch.
- A device role model lands: `DeviceScope` gains members and the client's
  fixed copy becomes a mapping.
- Real hosts report display names that read badly: add the `freesided`
  flag the contract deferred.
