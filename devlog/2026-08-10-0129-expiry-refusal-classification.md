# Export the Expiry Refusal for Permanent Seed Classification

Work unit: #416. Scope: `daemon/internal/publish`,
`daemon/cmd/freesided`, and `devlog/`.

## Decision

Export `ErrTokenExpiry` so the seed boundary can classify a refused
installation-token expiry as permanent. This revises the package-private
sentinel decision in
[`2026-07-31-0930-bounded-token-expiry.md`](2026-07-31-0930-bounded-token-expiry.md):
that decision assumed no caller distinguished expiry refusals, but #416 adds a
caller that must distinguish them to keep forged or regressed `expires_at`
values out of the bounded retry loop. The other condition behind the earlier
decision also changed: #411 is closed, so its exported-surface freeze no longer
applies.

The sentinel remains specific to the existing expiry gate. Missing,
unparsable, lapsed, and over-long expiries all wrap it, while a response that is
both over-broad and over-long still carries only `ErrGrantMismatch`. Exporting
the sentinel therefore changes classification, not the expiry bound, grant
precedence, janitor audit outcome, or returned-object trust decision.

Rejected matching the error text or adding a package predicate: Go's existing
`errors.Is` contract expresses the distinction without a second interface.
Rejected classifying every mint failure as permanent because transport and
service failures remain legitimately retryable. Rejected reusing
`ErrGrantMismatch` because it would misroute diagnosis toward repository scope
and permissions.

## Verification

The existing rejected-expiry corpus proves that every refused shape wraps
`ErrTokenExpiry` through the real mint path. The two combined over-broad and
over-long cases prove grant mismatch retains precedence and does not also wrap
the expiry sentinel. The seed-classification table proves the exported
sentinel maps to `ErrSeedRefused` rather than `ErrSeedRetryable`.

Revisit when seed classification no longer needs to distinguish expiry
refusals, or when a broader typed refusal contract replaces individual
sentinels across `daemon/internal/publish`.
