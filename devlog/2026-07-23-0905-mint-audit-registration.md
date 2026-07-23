# Mint Audit Registration Identity

Issue: #256. Dependency of #247.

## Decision

Chose GitHub's stable numeric App ID as the mint audit's registration identity,
matching the multi-account principal decision in
`2026-07-22-2124-multi-account-agent-identity.md`. Registration owner login,
App slug, and App name remain display metadata and cannot substitute for it.

Chose an explicit `0` legacy-unknown sentinel for rows written before
multi-registration minting over backfilling a guessed App ID. The old rows bind
only an installation ID and repository; local keystore state can change, so no
durable fact proves which registration produced them. Migration preserves those
rows with `0`, while `RecordMintAudit` rejects non-positive registration IDs for
every new write.

Rejected adding the field only to `publish.MintRecord`: `StoreRecorder` would
then erase it at the production persistence boundary while tests over a fake
recorder appeared to satisfy #247. The shared store shape and forward migration
therefore land first as this contract unit.

## Verification

The migration-path test constructs the schema through migration 0009, inserts a
legacy mint, applies the current migration set, and proves the row remains
readable with registration ID `0`. Store round-trip and rejection tests prove
new positive IDs persist and zero or negative IDs fail before SQLite.

Codex review confirmed the first contract draft would have broken the existing
singleton minter: the new store guard rejected zero, but `MintRecord` and
`StoreRecorder` did not yet carry the already-known App ID. The contract unit
now threads that identity from the selected credentials through both adapters,
and its production-recorder test proves a mint still succeeds and persists the
App ID before #247 replaces singleton selection with owner resolution.

Revisit when historical mint rows need external reporting: the unknown sentinel
must remain visible and must never be rewritten from present-day keystore state.
