# Normalize Stored Timestamps to UTC and Enforce It

Issue #553, `kind:contract`, `lane:spine`. Fiat-assigned after the
verification-pass revision reduced the unit to hygiene plus one narrow
validation gap. The implementation plan lives in the issue's third comment;
this note records the decisions the plan left to the implementer, the
refute-first findings mandated for the returned-object-trust boundary the
decode-path canonicalization touches, and the base the work was validated
against.

## Scope Resolved at Implementation Time

Two consumer-facing surfaces were checked against the plan's "no `api/`/`app/`
regen expected" claim and confirmed: the contract surface is the new
`domain.AttentionItem.CanonicalizeStoredRow` method plus the `Validate`
tightening; the wire shape of every synchronized type is unchanged (`ExpiresWhen`
was already `*time.Time` in the schema, and UTC normalization does not change
its JSON spelling for values producers already wrote in UTC). No cross-component
growth was needed, so the unit stayed within `daemon/internal/domain` and
`daemon/internal/store`.

## Decisions

**Canonicalize `ExpiresWhen` on the read path before the write tightening.**
Commit order is load-bearing: `decode` re-runs `Validate` on every stored row,
so tightening `Validate` to reject non-UTC before adding read tolerance would
make an existing offset row (a dev-CLI producer stamps `ExpiresWhen` from the
host clock through `NewAttentionItem`) unreadable. `CanonicalizeStoredRow`
rewrites a present `ExpiresWhen` to `.UTC()` (same instant, canonical spelling)
and `decode` calls it through a narrow optional interface after `json.Unmarshal`
and before `Validate`, covering both stored-item decode sites with no call-site
churn. The rewrite is lossless, so put-idempotence's canonical re-encode still
converges a legacy row without an `entity_version` advance, exactly the
`commit_plan_notice` (#222) precedent.

**Normalize in the constructor, reject in `Validate`.** `NewAttentionItem`
normalizes `ExpiresWhen` with `.UTC()` (the producer path stores one canonical
spelling, fixing the dev-CLI producer with no CLI edit); `Validate` rejects a
non-UTC `ExpiresWhen` as the backstop for a constructor-bypassing value. This
mirrors `DecidedAt`/`WithDecidedAt` and is asymmetric with them only in that
`DecidedAt`'s single writer rejects rather than normalizes; that is fine because
the constructor normalizes before its own `Validate` runs.

**Own RFC3339Nano parsing in `parseTime`, and ratchet the class shut.** The
write side already shared `formatTime`; the read side open-coded
`time.Parse(time.RFC3339Nano, ...)` at ~20 sites, each re-normalizing (or not)
on its own. `parseTime` is the parse mirror: RFC3339Nano to a UTC-normalized
instant with one uniform, value-free error prefix over the standard library's
(which already quotes the offending value). Every hand-rolled read and the 20
`.Format` write sites now route through the two helpers, and an AST-based ratchet
test fails if a `time.RFC3339Nano` selector appears in any non-test store source
outside them (with an explicit allowed list). This fixes the class mechanically
rather than by widening a grep pattern: a new un-normalized call site cannot
silently regrow.

## Refute-First Findings (Returned-Object-Trust Boundary)

The decode-path canonicalization runs before the trust gate, and the format
sweep changed comparison sites in credential-adjacent code, so both got a
refute-first pass.

- **Confirmed safe: canonicalize-before-`Validate` cannot smuggle a forged
  value.** `.UTC()` preserves the instant exactly (only `Location` changes), no
  trust bit is derived from `ExpiresWhen`'s spelling, and the write boundary
  still refuses a fresh non-UTC `ExpiresWhen` (`encode` runs `Validate`). The
  decode-path `Validate` UTC check for `ExpiresWhen` is intentionally dead
  (always canonicalized first); the producer boundary is where non-UTC is
  refused. Narrowing the read backstop to normalize rather than refuse a
  hand-edited offset row is the plan's accepted, instant-preserving trade.
- **Rejected by verification: the `review.go` comparison sites are not a
  read-regression.** Concern: `formatTime(failure.ObservedAt) != observedAt`
  (and the `ReviewRecord.CompletedAt` sibling) adds `.UTC()`; a pre-existing row
  with a non-UTC column would newly fail the consistency check. Refuted:
  `ReviewRecord`, `ReviewFailure`, and `ReviewRetry` all reject a non-UTC
  timestamp in `Validate` (`review.go:60/126/166`), every write runs `Validate`
  via `encode`, and `decode` re-validates before the comparison, so no non-UTC
  row of these types can exist and the in-memory value is guaranteed UTC. The
  added `.UTC()` is a proven no-op on both sides.
- **Rejected by verification: `auth_identity` lease logic is untouched.** The 10
  swept `.Format` lines there are all error-message strings (`LeaseHeldError`,
  `ErrStaleWrite`, `ErrLeaseWindowRegresses`); the gating comparisons use
  `.Before`/`.Equal` on `time.Time` instants, which are location-independent.
  `formatTime` only changes error-text spelling.

## Rejected Alternatives

- **Reuse `CanonicalizeStoredRow` for the constructor's normalization.** It does
  the same operation today, but coupling the producer path to the stored-row
  canonicalizer would silently drag any future legacy-only quirk into fresh
  construction. Kept the constructor's `.UTC()` inline.
- **Widen the `.Format`/`.Parse` grep pattern instead of a source-scan test.**
  Pattern-widening is what let a sibling class survive many review rounds
  elsewhere; the AST ratchet enumerates the real call sites and fails closed.

## Verification

Validated against base `origin/main`
f0b70f10cdf259271cafd7e629bdb5dd781272eb (the branch merge-base); the routine
green command suite lives in the PR, not here. Durable findings only:

- `cmd/freesided TestDaemonRecoversAcrossSIGKILL` fails solely under the known
  git-worktree `-buildvcs` stamping artifact (its internal `go build` omits the
  flag); it passes with `GOFLAGS=-buildvcs=false` and in a normal CI checkout.
- No migration, so migration-exclusion lists are untouched; domain goldens do
  not churn (the fixture's `ExpiresWhen` is already UTC).

## Revisit When

A second entity needs stored-field canonicalization: the narrow interface is
already the extension point, but confirm the new field's rewrite is
instant/value preserving before adding it, and add its own convergence test
against the `commit_plan_notice` precedent.
