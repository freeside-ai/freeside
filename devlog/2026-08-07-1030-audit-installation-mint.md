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

Chose to record **every token GitHub actually minted, at the point its
existence is known, before the returned grant is validated** (owner call after
the automated P1 below). `mintGrantReadToken` confirms a token exists (201 with
a non-empty token), then `classifyGrantReadMint` judges the grant and returns an
`outcome`, the granted scopes to record (the fixed grant only when validated,
else empty — the daemon does not vouch for a grant it rejected), and the
validated expiry (nil otherwise). The record is written before any
validation-driven return, so a token whose revoke also fails is never left off
the ledger. A record that fails to commit fails the mint (wrapped in
`errJanitorUnsafe`), and its error subsumes any validation error, since audit is
the barrier; the token still travels out so the caller revokes it, and on this
path it is never used.

Chose migration **0033 as a single additive `CREATE TABLE`**
(`publish_installation_mint_audits`, STRICT, insert-only, no token column). It
carries `registration_id`/`installation_id` (strictly positive — a fresh table
has no legacy-unknown sentinels), an `outcome` column (validated /
grant_rejected / expiry_rejected / undecodable), the six requested + six granted
permission scope columns, and a **nullable `expires_at`** (NULL when the expiry
was not validated, so the audit never fabricates an instant that never held).
`InstallationMintAudit` / `RecordInstallationMint` / `ListInstallationMintAudits`
mirror the worker `MintAudit` surface, with `InstallationMintOutcome` the closed
outcome vocabulary and `ExpiresAt` a `*time.Time`.

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
- **Record only the validated-clean mint** (the first-pass shape): rejected by
  the automated P1 below and the owner. It under-closes the very invariant the
  issue exists to protect — a 201 with a live token but a malformed grant is
  never used, yet if its revoke also fails the daemon holds a live unrevocable
  credential with no row. The determined shape (typed granted + valid expiry)
  structurally cannot represent that mint, which is why closing it needed the
  `outcome` column and nullable expiry above (an owner-approved expansion of the
  determined shape, resolved in-session).

## Refute-First Findings

A fresh-context reviewer (pre-push) traced the change decision-by-decision and
found no confirmed defect; the automated reviewer (Codex, post-push) then found
one **P1** that was real and reshaped the design.

- **Confirmed and fixed (P1, the design change above):** the validated-clean
  audit did not close the invariant for a token whose grant was rejected and
  whose revoke then also failed — a live, unrevocable credential with no row.
  Fixed by auditing every minted token before validation, with the `outcome`
  column and nullable expiry. Covered by
  `TestInstallationJanitorAuditsRejectedMintBeforeFailedRevoke`.
- **Confirmed and fixed (test robustness):** the store round-trip fixture set
  only `metadata:read`, leaving the other ten scope columns empty, so a
  field-to-wrong-empty-column misbinding would round-trip clean. The reviewer
  hand-verified the INSERT/SELECT/Scan correspondence (no actual bug), and the
  fixture now populates every scope column with a distinguishing value so the
  round-trip catches a misbinding, matching the worker mint-audit test pattern.
- **Rejected by verification:** `registration_id = app.AppID` must be positive
  or the store fail-closes and aborts the pass. This is established parity with
  the worker mint's `RegistrationID > 0` rule; `app.AppID` already anchors
  `reconcileRegistration`'s own error messages, so a zero here is already a
  broken registration, not a new failure mode. No regression.

No token material reaches the audit row, an error string, or a log: the record
has no token field by type, requested/granted carry the fixed scope names (not
the response), and an over-broad or proxy-tampered grant fails the scope
comparison before any record. Verified by the reviewer and by the two tests
asserting the enumeration token never appears in the error.

## Verification

- `go build ./...`, `go vet ./...`, `golangci-lint run` (0 issues), `gofmt -l`
  clean; full `go test ./...` green.
- Acceptance grep: `access_tokens` shows exactly two mint sites
  (`janitor.go` → `RecordInstallationMint`, `mint.go` → `RecordMint`).
- Four janitor mint-audit tests: clean pass writes exactly one validated row;
  failed revocation leaves the validated row; a rejected grant with a failed
  revoke leaves a `grant_rejected` row with no expiry; audit-write failure
  blocks token use yet revokes.
- Store tests mirror the worker mint-audit suite (round-trip, append-only,
  rejections, sync-invisibility, reopen).

## Revisit When

A second installation-scope mint appears (today only the janitor's grant-read
mint is installation-wide): the table's always-`metadata:read` scope columns
become genuinely multi-valued, and the "requested == granted" invariant the
record relies on would need its own validation at that new site.
