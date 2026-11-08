# One AuthProvider Enum for Identities and Pools

Work unit: #2783. Mandatory note: a `kind:contract` change to two domain
types, Tier A by path (`internal/domain/auth_identity.go`,
`internal/store/auth_identity.go`, and the `internal/ward/` callers).

`AuthIdentity.Provider` and `UsagePool.Provider` are now one named enum,
`domain.AuthProvider`, with members `claude` and `openai`. An unknown
provider fails `Validate` with `ErrInvalidAuthProvider`, so it fails on
write and as an invalid stored row on read.

## Decision: One Shared Enum

Chose one enum for both fields over typing the identity's field alone.
The store copies an identity's provider into its usage pool when it
creates the pool, and refuses to settle an identity into a pool whose
provider differs (`settleIdentityUsagePool`). Two types would need a
conversion at each of those points and could drift apart.

## Decision: Refuse an Unknown Provider, No Legacy Alias

Chose to refuse an unknown provider at decode over accepting `codex` as
a legacy alias for `openai`. No non-test code ever wrote `codex` as a
provider (`git log -S'Provider: "codex"' -- daemon ':!*_test.go'` is
empty), so no real row holds it and no migration is needed. The member
values are the strings rows already store, so existing `claude` and
`openai` rows decode unchanged. Test fixtures that recorded a `codex`
identity now use `openai`. The pre-rename dump and the Codex
re-enrollment migration test keep their `codex` rows, which no code
decodes.

The fake review identities in `internal/exec/fake` used `fake` as their
provider. They're recorded through `RecordAuthIdentity`, so they now use
`openai`. No code branches on a fake identity's provider: the provider
checks in ward, preflight, and the deputy act only on configured
identities that carry interim facts. Since 2026-11-01 the scenario seed
has written those `fake` rows, but only into a scenario driver's store,
a development fixture normally run in the fresh root
`scripts/dev-instance.sh` creates and removes. Chose that cost over a
`fake` member or a data-rewriting migration: a scenario root kept from
before this change stops reading its fake identities and pools, and is
discarded rather than repaired.

## Decision: Drain the Usage Pool Cross-Check

The issue plan said to keep the usage pool's column-versus-body
comparison and stringify it. Retyping the field forced an edit inside
`getUsagePoolRecord`, a Tier B decode, and the ratchet in AGENTS.md
(Assurance Tiers; `2026-11-07-1119-tier-b-tamper-ratchet.md`) says a
unit that changes a Tier B row's decode removes those checks unless a
Tier A gate relies on them. Project policy wins over the plan, so the
pool row now decodes from its body with plain validation, and
`TestUsagePoolFieldsCrossChecked` is gone.

No Tier A gate relies on it. The pool's readers are pool settling,
slot capacity, `auth list`, `set-pool-limit`, the operator pool
commands, and the usage projection. The review gate in
`engine/production_review_gate.go`, a Tier A file, calls the slot
capacity check, but a pool's limit only bounds concurrency (pools and
holds are Tier B) and grants no authority or credential. No SQL outside
the decode reads the `provider`, `max_parallel_executions`, or
`recorded_at` column, so the body is the only source any reader sees
and the comparison guarded nothing they use. The body-versus-key id
check went with it; `RecordUsagePool` always writes the two equal.

The auth identity's cross-check in `getAuthIdentityRecord` stays: that
file is on the Tier A list, and admission chooses a credential through
the identity.

## Refute-First Findings

Each claim that the change is wrong, and how it was settled:

- **A caller changed behavior.** Disproved by check: mapping each
  member back to its literal and dropping the `string(...)` conversions
  reproduces every changed non-test caller byte for byte, except the
  fake review identities' intended move from `fake` to `openai`.
- **A missed site still compares a string.** Disproved by check: no
  non-test code compares or writes a provider literal. The one grep hit
  left, `seedfixture/items.go`'s `LegacyProvider`, is a review record's
  stored legacy label, not this field. The `claude` and `codex` literals
  in `preflight.go` name CLIs, not providers.
- **A real row holds another provider and stops decoding.** Disproved
  for production stores by history: no non-test writer stored anything
  but `claude` or `openai`, and migration 0112 copies identity providers
  into pools. Allowed by decision for scenario stores (above).
- **The encoding changed.** Disproved: the type is still a string
  underneath, the domain goldens are byte-identical, the store passes
  the column as a string, and the wire structs stay strings.
- **The fake identities' new provider changes a review label.**
  Disproved: `ReviewAdmission.ProviderLabel` reads the lineage group, and
  nothing selects identities by provider.
- **Draining the pool check weakens an authority gate.** Disproved by
  tracing its readers (above).

A fresh-context reviewer ran the same refutation over the diff and
found no defect.

## Revisit When

Revisit when a Phase 2 provider unit (#406) adds a provider descriptor
or registry; the enum may become its key or give way to it.
