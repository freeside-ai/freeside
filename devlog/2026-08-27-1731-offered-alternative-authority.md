# Offered Adjudication Alternatives Bound to Durable Authority (#893)

## Decision

Give the digest-bound `FindingAdjudicationEntry` a typed
`OfferedAlternatives []OfferedAlternative` field, bump the encoding version
1 to 2, and derive the set deterministically from the entry's `(goal, route)`
row at construction. Only the two-route `contradictory` row offers an
alternative: the route the recommendation did not take, with a fixed
consequence. The engine projects this set into the sync-carried proposal
instead of synthesizing it, and the store `gateFindingAdjudicationItem`
authenticates it element-wise against the artifact.

The authority is the content address. The item creator no longer owns the
offered routes and consequences; a route or consequence introduced or rewritten
only in the item payload fails closed against the artifact on both the write and
every snapshot reconstruction, so `choose_alternative_route` and `PutCommand` —
which load the item through that same re-gating snapshot read — authenticate
transitively without their own comparison.

## Rejected Alternatives

- **Re-derive the offered set at the re-gate instead of storing it.** Rejected
  because plan §7 makes viable-alternatives-with-consequences part of the model
  residue the artifact captures. The set is a deterministic function of the row
  *today*, but a future model-authored alternative cannot be re-derived, so the
  digest-bound artifact is the forward-compatible home. Re-derivation would also
  couple the persistence gate to a synthesis rule that must then never drift.
- **Take the offered set as a constructor parameter (the plan's shape).**
  Rejected in favor of deriving it inside the constructors. No caller or model
  output supplies a typed offered set today, so a parameter would be unusable
  boilerplate threaded through every construction site and test. Deriving keeps
  the field digest-bound and forward-compatible at the representation level; a
  future authored set changes only the construction path, not the contract.
- **Preserve version-1 artifacts across the bump (graceful decode/migration).**
  Rejected as a clean break. The finding-adjudication artifact type is
  pre-release (introduced days earlier, no tagged release), so no persisted v1
  rows exist, and the new field changes every artifact's canonical bytes
  regardless, making the version bump the honest signal. This matches the hard
  version-mismatch reject in `Validate` used by every other encoding-versioned
  domain type, none of which carry a migration path. (An automated-review P1
  raised the migration concern; declined on this basis.)
- **Add a dedicated `sameOfferedAlternatives` store helper.** Rejected because
  `OfferedAlternative` is a comparable struct, so the existing generic
  `sameSlice` already compares element-wise on route and consequence,
  order-sensitive, with the nil-versus-empty parity the sibling list fields use.
- **Widen the OpenAPI schema and app client.** Not needed. `OfferedAlternative`
  (route plus non-empty consequence) already rode the synced proposal from #837
  and the derivation is byte-identical to the prior projection, so the wire
  contract is unchanged; `api/` and `app/` stay consistent without edits and the
  work unit is daemon-only.

## Refute-First Verification

The returned-object trust boundary is the decoded item's offered set measured
against the digest-bound artifact entry.

- **Confirmed and fixed:** the store write gate and every snapshot
  reconstruction (`GetAttentionItemSnapshot`, the open/all list reads) reject a
  removed, rewritten-consequence, or empty-versus-nil offered set with
  `ErrParentKeyMismatch`, including after a raw-row body rewrite — so restart and
  direct database tampering fail closed. The immutable-history record path stays
  intentionally un-gated.
- **Confirmed and fixed:** a raw-row-forged item that makes a non-offered route
  appear offered cannot authorize a choice; `Submit` rejects it at the re-gating
  snapshot read before the offered check runs.
- **Confirmed and fixed:** the domain locks the derivation (contradictory yields
  the alternate route; single-route rows yield none) and the structural
  validation (invalid route for the row, repeats-recommendation, duplicate, and
  empty consequence all reject), now owned by the entry with the binding
  delegating to it.
- **Rejected by verification:** no schema migration was required — the artifact
  persists as a JSON body keyed by digest; goldens regenerated for the field and
  version only.
- **Accepted by decision:** deriving the offered set rather than accepting it
  keeps invalid states unrepresentable today while leaving the digest-bound
  field as the home for a future authored set. The revisit condition guards that
  choice. Verified end-to-end against the real daemon through the convergence
  harness, including the `finding_adjudication` type, with `api/` and `app/`
  unchanged.

## Revisit When

A model authors the offered alternatives or their consequences, so the set is no
longer a deterministic function of the row. At that point add an authored-offered
-set source to the entry constructors under its own contract unit; the digest-
bound representation and the persistence re-gate already hold, so only
construction and the derivation's authority change.
