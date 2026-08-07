# Align Engine Digest Validation With the Shared Parser (#552)

Work unit: #552. Scope: `daemon/internal/engine`, `devlog/`. This changes a
persisted-payload decode boundary, so the high-assurance trust-boundary rule
requires this note.

## Decision

Chose to keep the engine's `domain.Digest`-typed `validSHA256Digest` wrapper
but delegate its string-shape decision to `contentaddr.Valid`, over retaining
the local `hex.DecodeString` implementation. This follows the earlier
`devlog/2026-07-20-1153-centralize-content-address-parsing.md` decision that
callers keep their local types and error policy while the neutral leaf owns the
canonical `sha256:<64 lowercase hex>` form. The local decoder accepted
uppercase hex, so keeping it would preserve a second, divergent parser on a
durable reconstruction path.

The change deliberately does not introduce formatting helpers or convert
producer sites. Existing producers use `hex.EncodeToString`, which emits
lowercase hex, so the stricter reader does not exclude a form they persist.

## Verification Findings

The refute-first input enumeration confirmed one decision difference from the
old engine validator: uppercase or mixed-case hex is now rejected. Lowercase
64-hex remains accepted; empty, missing-prefix, wrong-length, non-hex, and
whitespace-bearing inputs remain rejected. Both consumers were exercised
separately: task reconstruction rejects an uppercase handoff digest, and
invocation-owner decoding rejects an uppercase binding digest.

Confirmed and fixed: the engine reader was more permissive than the shared
canonical parser. Rejected by verification: persisted fixtures do not depend
on uppercase digests, and the full daemon suite accepts the tightened reader.
Accepted by decision: no non-canonical compatibility form is retained because
all in-tree producers already emit lowercase.

Revisit when: Freeside deliberately adopts a non-canonical content-address
form. That is a shared `contentaddr` contract change, not an engine-only
exception.
