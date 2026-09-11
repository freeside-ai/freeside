# Contract Digest for Client/Daemon Skew (#1265)

Diagnose a client/daemon API-contract mismatch instead of letting it surface
as a silent sync failure. This is the lasting rationale for a `kind:contract`
change (High-assurance profile: contract changes require a note).

## Decisions

- **Chose a spec-derived digest on `GET /health` over reusing `version`.**
  The contract identity is `sha256:` plus the SHA-256 of `api/openapi.yaml`'s
  exact bytes, served on the unauthenticated health route and compiled into a
  daemon constant and a Swift client constant. `version` is only a build
  label: two builds of the same spec share a contract but differ in version,
  and one build can carry either spec, so version can neither confirm nor deny
  a contract match. The digest identifies the spec itself. `version` is left
  as the build label alone.

- **Chose exact-byte hashing over a semantic/normalized spec hash.** Any edit
  to the spec flips the digest, including formatting-only edits. That is the
  point: the constant is regenerated from the spec by one script
  (`scripts/api-contract-digest.sh`), gated three ways (the `api` `digest`
  check, the daemon drift test, and the app `generate` check), so neither
  compiled constant can drift from the bytes clients and daemon actually
  built against. A semantic hash would add a normalizer to trust and could
  call two byte-different specs "the same" when the generated code differs.

- **Chose a reactive probe over proactive skew polling.** The client probes
  `/health` only after a sync read already failed as reachable-but-failing,
  then reports `.contractMismatch` (carrying the daemon's digest) instead of
  `.syncFailing`. Steady-state sync adds no request. Proactive diagnosis while
  reads still succeed was rejected as a non-goal (#1265 scope): there is no
  user-visible problem to diagnose until a read fails.

- **Chose whole-contract detection over per-feature gating.** A digest
  mismatch means "update the daemon or the app," not "this one field is
  missing." Per-feature gating on the digest is deferred to #1266;
  tolerating mismatched versions or attempting partial sync is a non-goal.

## Rejected Options

- Reuse `version` for skew detection (cannot distinguish spec from build).
- Semantic/normalized spec hash (adds a normalizer to trust; can mask
  generated-code differences).
- Proactive skew polling (steady-state cost with no problem to diagnose).
- Per-feature capability negotiation (deferred to #1266).

## Revisit When

- Per-feature gating (#1266) needs a finer signal than one whole-contract
  digest.
- Persisted-state upgrade compatibility (#854) needs the digest to reason
  about cross-version cache validity.
