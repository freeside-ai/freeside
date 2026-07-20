# Centralize strict SHA-256 content-address parsing (#200)

Contract refactor, 2026-07-20. Extracts the repeated strict
`sha256:<64 lowercase hex>` parser into a neutral leaf package
`daemon/internal/contentaddr`, replacing four decision-identical copies.
`kind:contract` (spine-owned) and a trust-boundary change: these parsers gate a
filesystem path (signet), a credential field (domain), a manifest entry
(export), and an untrusted GitHub-returned PR body (publish). Mandatory note
per the contract-change and trust-boundary triggers.

## The duplication

Four packages each carried their own copy of the same predicate — strip the
`"sha256:"` prefix, require exactly 64 characters, reject anything outside
lowercase `[0-9a-f]`:

- `export/manifest.go` `func (d Digest) valid() bool`
- `domain/device.go` `func isSHA256Digest(s string) bool`
- `signet/attachments.go` validation fused into `blobPath`
- `publish/identity.go` `func validIdentityDigest(raw string) bool`

They differed only cosmetically (bool vs `(string,error)` return, literal `64`
vs `sha256.Size*2`, `strings.CutPrefix` vs index-slice), never in the
accept/reject decision. Four is past the project's three-instance abstraction
threshold, and a divergent future edit to one copy would silently split a
security-relevant invariant.

## Decision: two functions, no named type

Chose a minimal `Parse(raw) (hex string, ok bool)` + `Valid(raw) bool` pair over
a named validated `Addr` type. Three of the four callers need only a yes/no
decision; only signet needs the hex payload (to build the `sha256-<hex>`
filename), which `Parse` returns. A named type would add `.Hex()`/`.String()`
surface that no caller consumes, and each caller already keeps its own named
type (`export.Digest`, `domain.Digest`). Merits-driven, not scope-driven:
the primitive stays the size of its single job.

## Decision: keep the wrappers, types, sentinels, and policy at each caller

Chose to swap only each validator's *body* to call `contentaddr`, leaving the
wrapper function, named type, sentinel error, error context, and interleaved
package policy untouched, over inlining `contentaddr.Valid` at the call sites.
The parser decides string shape only; everything package-specific stays put:

- **export** keeps its local `Digest` type (the helper is a standalone binary,
  and `contentaddr` is a zero-dependency leaf, so the "no domain import"
  property holds) and the `EntryRegular`-gated mode/size/target checks.
- **domain** keeps the `Kind == CredentialHash` gate, the separate
  empty→`ErrEmptyField` check, and the anti-plaintext-secret
  `ErrPlaintextCredential` semantic (distinct from a mere format error).
- **signet** keeps `ErrInvalidDigest`, its error context, and the
  `"sha256-"+raw` filename derivation; the `//nolint:gosec` in `Open` stays
  justified because the path still derives only from the strict form.
- **publish** keeps the fail-closed `ParseMarker` framing and single-identity
  uniqueness rule.

`contentaddr` imports only `crypto/sha256` and `strings` and nothing from
domain/export/signet/publish, so `domain` importing it introduces no cycle and
the leaf stays neutral.

## Verification: refute-first equivalence pass

A throwaway harness (not committed) reconstructed all four pre-refactor bodies
verbatim from base `origin/main` (2464dc2) and compared each old decision — and
signet's derived hex payload — against `contentaddr.Parse`/`Valid` over a
400,017-input corpus (adversarial seeds spanning prefix/length/case/non-hex/
whitespace/empty/boundary, plus a deterministic random sweep of hex, mixed-case,
alnum, and whitespace-laden alphabets with and without the prefix). Result:
**0 decision differences**. The permanent coverage is the committed
`contentaddr_test.go` table plus `FuzzParse` (the repo's first fuzz test; its
seed corpus runs under ordinary `go test`, ~4.5M execs under `-fuzz` found no
failures). All four callers' pre-existing tests pass unchanged, which is the
caller-level equivalence guarantee.

Refute-first ledger: no findings confirmed (equivalence held), none
rejected-by-verification, none accepted-by-decision — the accept/reject sets are
byte-for-byte unchanged, as the issue required.

Revisit when: a caller needs a non-canonical address form (uppercase, a
different algorithm, or a multihash encoding). That is a deliberate contract
change, not a silent widening of this parser; add it as its own `kind:contract`
unit rather than loosening `contentaddr`.
