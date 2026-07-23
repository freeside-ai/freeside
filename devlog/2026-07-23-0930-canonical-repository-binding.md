# Canonical Repository Trust Binding

Issue: #259. Dependency of #247.

## Decision

Bind every automation trust profile to GitHub's positive, immutable numeric
repository ID in addition to its human-readable `owner/name`. The ID is
owner-approved profile content, participates in the profile digest, and is the
only repository selector suitable for installation-token minting. The name
remains the local lookup and display key, but cannot authorize a repository
that later reuses it after a rename, transfer, or deletion.

Advance the canonical profile encoding to v5. Profiles recorded under v4 or
earlier contain no approved repository ID, so they fail closed and require
owner re-approval. Rejected silently discovering or backfilling IDs from
current GitHub state: that would convert external state observed after the
decision into trusted content the owner never approved. Also rejected a
separate mint-only mapping, because it would not be covered by the existing
profile digest or the persistence-boundary re-gate.

No store schema migration is needed. Trust profile bodies are self-contained
JSON addressed by their digest; the existing store decodes and validates every
body on write and read. The new required field therefore ratchets the boundary
without a parallel source of identity.

## Verification

Domain tests prove zero and negative IDs are rejected, changing the ID under an
approved digest is detected, and the v4 digest no longer validates under v5.
The persistence refute-first test inserts an authentic v4 body that lacks the
field and proves both direct and list reads fail with the non-positive-value
sentinel rather than returning a defaulted profile or `ErrNotFound`.

Revisit when onboarding records trust profiles: repository discovery may
populate the input, but the owner-approval surface must display and approve the
resolved immutable ID rather than treating discovery as approval.
