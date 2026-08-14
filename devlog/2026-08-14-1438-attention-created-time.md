# Attention Item Creation Time

## Decision

Chose a required, nullable `created_at` wire key over an optional key or a
backfill because every current client should receive one stable contract shape,
while persisted items created before issue #774 have no trustworthy creation
instant to reconstruct. The daemon stamps new items in UTC at their first
persistence path and transition validation makes the value immutable, including
forbidding a later `nil`-to-value backfill.

Chose explicit creation-time handling at every `AttentionItemInput` site over a
constructor default because several builders are also replay and trust-boundary
reconstruction helpers. A hidden wall-clock default would make deterministic
reconstruction disagree with durable state. An AST test requires every daemon
literal to declare its choice; replay builders then copy the durable stamp at
their persistence or comparison boundary, while the legacy migration declares
`nil`.

Chose to exclude `created_at` from the fake-publication terminal digest because
that digest binds derived publication identity, while creation time is a
lifecycle fact assigned when the item first persists. Recovery compares the
rebuilt terminal shape using the stored creation stamp, so a restart cannot
change the stamp or invalidate an otherwise identical terminal binding.

The clients render the absolute creation time with the system locale and time
zone, with an explicit “not recorded” label for legacy `null`, because the
detail view is an operator-local chronology surface rather than a canonical
wire representation.

## Rejected Alternatives

- Making the key optional would multiply generated-client states without
  preserving any additional historical truth.
- Backfilling from delivery, decision, or server-revision times would present a
  later lifecycle event as creation and corrupt the chronology.
- Stamping inside `NewAttentionItem` would make replayed deterministic items
  non-deterministic and could turn recovery into immutable conflicts.

## Verification Finding

A refute-first recovery test reconstructs one terminal item with a different
creation instant after the durable item has advanced. The terminal digest stays
bound to the publication facts, the replay is accepted, and the original
durable `created_at` remains unchanged. A control assertion confirms that
changing a publication fact still changes the digest.

Revisit when the persistence format records an authoritative creation event for
all legacy items, or when fake-publication terminal binding moves to a versioned
shape that explicitly includes lifecycle metadata.
