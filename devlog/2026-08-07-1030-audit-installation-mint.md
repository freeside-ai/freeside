# Audit Every Installation-Token Mint (Janitor Grant-Read)

Issue #545, `kind:contract`. The installation janitor mints a metadata-only
installation token every pass to enumerate an installation's repository grants,
outside the publish package's own "every mint is audited" invariant. The owner
determination (on the issue, 2026-08-07) resolved the audit-vs-exempt fork:
**audited**, via an additive installation-scope table, not a rebuild of
`publish_mint_audits`. This note records the implementation shape and the
refute-first findings mandated for a credential-audit / returned-object-trust
surface.

## Decision

Chose a **new audit port on the janitor** (`InstallationMintRecorder`,
implemented by the existing store-backed `StoreRecorder`) over widening the
existing `JanitorRecorder`. The two record different events to different durable
surfaces: `JanitorRecorder` commits the destructive-action barrier to the
file journal (`InstallationAuthorityStore`), while the mint audit is an
append-only SQLite ledger. Fusing both behind one port would either force the
file-journal type to reach a store it structurally does not hold, or introduce
a composition-root adapter whose only job is to hide two backends behind one
interface. Two ports keep the concerns separate; the janitor's mint "routes
through the store-backed recorder" exactly as the acceptance requires.

This is a **conscious deviation from the issue's "Affected interfaces" note**,
which anticipated widening `publish.Recorder` / `publish.JanitorRecorder`. The
acceptance criteria (the spec) mandate only that the mint route through the
store-backed recorder's installation-scope method and land in the additive
table; a dedicated narrow port satisfies every criterion with cleaner
interface segregation. `publish.Recorder` is left untouched so the worker
Minter's port stays focused; `StoreRecorder` gains `RecordInstallationMint`
concretely.

Chose to record **inside `mintGrantReadToken`, on the fully validated path
only**, mirroring the worker mint (`mint.go`): after the scope-equality and
expiry checks pass, before the token is returned for use. The scope comparison
proves the grant identical to the fixed request, so both requested and granted
carry `metadata:read`. A record that fails to commit fails the mint (wrapped in
`errJanitorUnsafe`); the token still travels out so the caller revokes it, and
it is never used. Because the row is written at mint time, a later revoke
failure still leaves an operator-findable record of the live credential, which
is the exact gap the determination named.

Chose migration **0033 as a single additive `CREATE TABLE`**
(`publish_installation_mint_audits`, STRICT, insert-only, no token column). It
carries `registration_id`/`installation_id` (strictly positive — a fresh table
has no legacy-unknown sentinels) and the six requested + six granted permission
scope columns, mirroring `publish_mint_audits` minus the repository-scope
columns an installation-wide mint cannot fill. `InstallationMintAudit` /
`RecordInstallationMint` / `ListInstallationMintAudits` mirror the worker
`MintAudit` surface.

## Rejected Alternatives

- **Widen `JanitorRecorder`** with `RecordInstallationMint`: its sole
  implementer is the file-journal `InstallationAuthorityStore`, which cannot
  reach SQLite; routing the mint there contradicts the determination's SQLite
  table, and a composite adapter to bridge the two complects distinct audit
  concerns behind one port. See Decision.
- **Documented exemption / rebuild of `publish_mint_audits`**: both foreclosed
  by the owner determination on the issue (the exemption leaves the
  failed-revoke gap open; a rebuild cannot relax `publish_mint_audits`'
  CHECKs without a full-table copy of an insert-only ledger, and 0011's legacy
  `repository_id = 0` sentinel blocks a clean per-scope validity CHECK).

## Refute-First Findings

_(pending the fresh-context reviewer; folded before push)_

## Verification

- `go build ./...`, `go vet ./...`, `golangci-lint run` (0 issues), `gofmt -l`
  clean; full `go test ./...` green.
- Acceptance grep: `access_tokens` shows exactly two mint sites
  (`janitor.go` → `RecordInstallationMint`, `mint.go` → `RecordMint`).
- Three janitor mint-audit tests: clean pass writes exactly one row; failed
  revocation leaves the row; audit-write failure blocks token use yet revokes.
- Store tests mirror the worker mint-audit suite (round-trip, append-only,
  rejections, sync-invisibility, reopen).

## Revisit When

A second installation-scope mint appears (today only the janitor's grant-read
mint is installation-wide): the table's always-`metadata:read` scope columns
become genuinely multi-valued, and the "requested == granted" invariant the
record relies on would need its own validation at that new site.
