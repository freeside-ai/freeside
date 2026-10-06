# Mint Audit Issues Scope

Issue #1768 lets the mint audit record an `issues` permission scope. It is a
contract unit: a `publish_mint_audits` column pair (migration 0092) and the
two `store.MintAudit` fields that read and write it. #1634's filing token
requests `issues: write`, and without the pair that mint would have been
recorded as `metadata: read` alone.

The issue left three choices to the spine. Planning resolved them on
2026-10-06 (the issue's Planning Resolution); this note keeps the reasons.

## A Column Pair, Not a Scope Map

Chose `requested_issues` and `granted_issues` over one map or JSON column
holding every scope. The six existing scopes are per-scope columns (0006,
0009), so a seventh pair keeps the table in one shape and leaves every
existing row and reader alone.

Rejected: a scope map. It would change how every scope is stored and read to
save one `ALTER TABLE` per new scope, and plan §8 keeps audit rows typed and
relational, with no map fields.

Both columns are `NOT NULL DEFAULT ''`, as in 0009. An empty scope means "not
requested", so nothing is backfilled: a row written before the migration
never requested the scope.

## The Installation Audit Table Stays as It Is

Chose to leave `publish_installation_mint_audits` (0033) and
`store.InstallationMintAudit` without the pair. No `issues` value can reach
that table:

- The janitor's grant-read request is fixed at `metadata: read`
  (`grantReadPermissionScopes` in `daemon/internal/publish/janitor.go`).
- `classifyGrantReadMint` rejects a returned grant that differs from the
  request in any key or value (`maps.Equal`). Every outcome records either
  the zero `Permissions` or the fixed request, never the returned map.

A pair there would hold `''` on every row. The issue raised the table
because its `Granted` side records what GitHub returned; that side is only
ever the fixed request, written once the returned grant has been proved
identical to it.

Revisit when the grant-read request names `issues`, or the janitor starts
accepting a grant wider than its request.

## The Publish Mapping Lands With the Field

Chose to leave `daemon/internal/publish/audit.go` unchanged. `RecordMint`
copies `publish.Permissions` into the audit row, and `Permissions` has no
`Issues` field yet. Adding it is #1634's work (and #1437's), so the mapping
has nothing to read until then.

Rejected: adding the field here so the mapping could ship with the columns.
The field is the start of the filing permission set, which this unit's
contract excludes, and a field no mint requests would be untested surface.

The accepted consequence: until #1634 merges, both columns are `''` on every
row the daemon writes. The store round-trip tests are the only writers that
set them.
