# Actionless blocked attention items (#96)

Contract relaxation: `domain.AttentionItem` and the OpenAPI schema now
permit an empty `requested_decision`, so signet can persist the
read-only `blocked` type exactly as plan §4 defines it (no action).
Discovered by #23, which pinned the authoritative empty set in signet
policy but could not persist it past the structural contracts.

## Decision

Cardinality is policy, not structure. The structural contracts
(domain validation, `api/openapi.yaml` `minItems`) no longer impose a
minimum; signet's `validateRequestedActions` alone decides which types
must offer actions: `blocked` must offer none (its allowed set is
empty, so any offered action is rejected), each other Phase 1 type
must offer at least one from its allowed set. `domain.ErrNoActions`
survives as the sentinel signet raises; it is no longer a structural
error.

## Rejected alternatives

- **Per-type action table in domain.** Would let structure carry the
  rule, but moves signet policy into shared vocabulary; rejected by
  the #23 decision the issue restates ("do not move the per-type
  action table into `daemon/internal/domain`").
- **A `blocked`-only structural exception in domain/schema** (e.g. a
  conditional minimum keyed on type). Encodes one type's policy in
  every consumer's generated code; the general relaxation plus a
  policy gate is smaller and keeps one owner for the rule.
- **Keeping the saddle placeholder set.** The fixtures misrepresented
  plan §4 (three fake actions on a read-only type) and forced the mock
  to special-case blocked as unconditionally rejecting; both now
  mirror the daemon exactly.

## Verification findings

- The mock server's Submit-path policy mirror previously rejected
  blocked unconditionally; mirroring signet exactly changes the
  rejection class for a command against a canonical blocked item from
  item-policy to action-not-offered, matching the daemon's gate order.
  A seeded blocked row forging an offered action still fails the
  policy re-gate for that very action.
- Wire rendering needed no change: the sync projection
  (`normalizeAttentionItem`) already renders every item collection as
  a required non-null array, and the new `attention_item_blocked`
  golden pins the actionless shape (`"requested_decision": []`).

Revisit when: a second actionless attention type appears; the
per-type sets in `allowedActionsByType` are the single registration
point, and nothing else should need to change.
