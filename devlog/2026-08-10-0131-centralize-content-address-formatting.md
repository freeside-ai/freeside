# Centralize Content-Address Formatting (#564)

Contract refactor and trust-boundary sweep. Scope: `daemon/`, `devlog/`.
Mandatory note because the unit extends the shared `contentaddr` contract and
changes digest handling at returned-object, persisted-row, and filesystem
boundaries.

## Decision: One Canonical Writer and Inverse

Chose `Format`, `Sum`, `Hex`, and `FromHex` in the standard-library-only
`internal/contentaddr` leaf over retaining open-coded writers and prefix
handling in each producer. `Format` panics unless its input is exactly one
SHA-256 sum because accepting another length would let the canonical writer
produce a value that `Parse` rejects. `Sum` owns the one-shot hash-and-format
case. `Hex` and `FromHex` are the fail-closed inverse pair for callers that
must cross the address/payload boundary.

The implementation-base audit found 95 non-test sites across 50 files, one
formatter more than the August 9 issue audit: 84 writer, join, or bare-hex
reattachment sites and 11 prefix-removal sites. Chose a package test that scans
all non-test daemon Go source over a `scripts/` check so the contract remains a
single-component unit and ordinary `go test` permanently rejects regrowth.
Test fixtures remain excluded so expectations stay independent of the helper
under test.

## Decision: Preserve Gated Inputs and Fail Closed at Ungated Inputs

Chose `Hex` for values already validated or locally produced: decoded export
and evidence manifests, OCI descriptors after `validDigest`, locally written
export blobs, publication identities after their contract validation, and
persisted ready-resource bindings after reconstruction validation. This is
byte-identical for their admitted input set. The ready-resource path also
checks the `Hex` result before truncating it, so invalid caller input returns
the existing inconsistency error instead of reaching a slice panic.

Chose checked `FromHex` at ungated boundaries: importer directory entry names,
vendor-instruction proofs, Codex workspace observations, persisted review
bindings, and observer proof output. Invalid bare hex is rejected before it can
become a content address. The prepared instruction bundle is the sole
unchecked inverse: its payload was just produced by `hex.EncodeToString` over
a SHA-256 sum in the same trusted preparation path, and the call site records
that invariant.

## Verification Findings

The permanent differential fuzz harness reconstructs the former open-coded
writer and prefix-removal formulas independently. A focused fuzz run exercised
83,355 generated inputs with zero differences for canonical inputs; the
separate arbitrary-hex harness confirmed `FromHex` accepts exactly 64 lowercase
hex characters. The intended divergence is limited to non-canonical input:
blind `TrimPrefix` passed garbage through, while `Hex` returns empty and the
ungated callers now use checked `FromHex`.

Automated review found that the implementation had left one of the 11 audited
prefix-removal sites in instruction composition. Its multiline `TrimPrefix`
call also bypassed the ratchet's same-line regular expression. The admitted
operator-host digest now uses `Hex`, and the inverse ratchet parses Go syntax
and resolves ordinary, aliased, and dot-imported `strings` calls so formatting
cannot hide the same class again.

Refute-first ledger: confirmed and fixed the open-coded producer/inverse class
and the importer/proof acceptance opportunity it created. Rejected by
verification: no canonical digest bytes, goldens, or admitted caller behavior
changed. Accepted by decision: non-canonical bare hex now fails closed at the
explicit ungated boundaries rather than being reattached and checked through a
second spelling of the rule.

Revisit when: Freeside deliberately adopts another digest algorithm or a
non-canonical address encoding. That is a new shared-contract unit, not a
reason to weaken these SHA-256 helpers or bypass the ratchet.
