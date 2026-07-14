---
run: manual
stage: authenticate-policy-digest
date: 2026-07-14
branch: feat/authenticate-policy-digest
---

# Authenticate the resolved-policy digest (issue #33)

Spine-role session. #33 is the Track A (`kind:contract`) chain head of the
Wave 0 exit-fixes batch (#4), self-selected: predecessor n/a (chain head,
Dependencies none), no active claim (PR #42 is Track B, `exec/fake`, no path
overlap), and contract serialization holds because the only other scheduled
open contract units, #32 and #37, have Dependencies chains that include #33
(exempt per the PR #12 amendment). Deferrals #22/#28 are unmilestoned/dormant.

The finding: `ResolvedPolicy.Validate` required only a non-empty `Digest` and
never derived it from the keys, and `PutResolvedPolicy` compared that
caller-controlled digest to the run's caller-controlled `policy_digest` column
without authenticating the policy body. A caller could persist arbitrary values
and provenance under an expected digest; write-once immutability then locked in
a false attribution. Declared paths: `daemon/internal/domain`,
`daemon/internal/store`, `devlog/`. PR #43.

## Decisions

- **Keys-only, order-independent content address (confirmed with the user).**
  `ComputeDigest` = sha256 over the JSON of the keys (key + value + per-key
  provenance) sorted by key name; `run_id` and the digest field itself are
  excluded. Identical resolved content yields the same digest regardless of run
  or key order, matching how every other digest in the repo is a content
  address. Rejected **keys+run_id** (would make the same policy under two runs
  two digests, departing from content-addressing); the run↔policy binding is
  enforced separately at the store, so run identity does not need to be in the
  digest.
- **Authentication lives in the domain, enforced for free at both store
  boundaries.** `Validate` recomputes the digest and rejects a mismatch. The
  store's `encode`→`Validate` (write) and `decode`→`Validate` (read) then reject
  a forged digest on both paths with no store production-code change; the only
  store edit is a comment noting the existing run-binding check now carries
  authenticated meaning (acceptance 3). Mirrors the #21 precedent (rules live
  with the vocabulary). A `NewResolvedPolicy` constructor makes the digest
  authentic by construction so callers never hand-set it.
- **Canonical body: one representation the digest addresses (class fix across
  the refute pass and the Codex review).** The stored body must be byte-for-byte
  the form `ComputeDigest` hashes, or the write-once store can hold two bodies
  for one digest and idempotent retries collide with `ErrImmutableConflict`.
  Two members of this class surfaced and were closed at the root, not
  per-instance:
  1. **Key order** (refute pass). The first cut sorted keys only inside
     `ComputeDigest`, leaving the persisted body in caller order, so a **reorder**
     of the same content produced the same digest but a different body. Fix:
     `NewResolvedPolicy` stores keys in canonical (sorted) order and `Validate`
     **requires** it (`ErrKeysNotCanonical`).
  2. **Empty vs nil** (Codex P2). An empty-but-non-nil `Keys` marshals to `"[]"`
     in the body, but the digest's key copy (`append([]PolicyKey(nil), …)`)
     collapses empty to nil (`"null"`), so a zero-key policy's body differed from
     the bytes its digest addressed, and a nil-vs-`[]` retry would collide. Fix:
     **reject zero-key policies** (`len(p.Keys) == 0` → `ErrEmptyField`) — a
     resolved policy that resolves no keys is degenerate (plan §5.12 is per-key
     provenance), and rejecting it removes the only representation ambiguity
     canonical order does not.
  With the class enumerated (order, nil/empty are the only axes where the sorted
  copy can marshal differently from a non-empty in-order `p.Keys`), the body of
  any valid policy now equals the digested bytes. Rejected a custom `MarshalJSON`
  that sorts/normalizes (too much serialization magic and blast radius for every
  marshal); the explicit Validate invariants match the package's
  every-invariant-is-a-check style.
- **No migration.** The `resolved_policies` columns (`digest`, `body`) are
  unchanged; only the digest's *value* is now constrained. `migrations/` stayed
  out of declared scope.

## Refute-first pass (returned-object-trust / data-integrity boundary)

One fresh-context reviewer, prompted to refute over seven attack angles (diff +
stated intent only). Ledger:

- **Confirmed sound (no defect):** forged digest on write (A); determinism —
  no maps, total sort over distinct keys, slice copied so no post-hoc aliasing
  (B); coverage of value + full provenance (C); run_id exclusion is not a replay
  hole, the write-path run binding is reachable and correct (D); `Validate`
  cannot return nil on a mismatch — the digest check is last and every earlier
  branch errors (F); tests assert the specific sentinel and split the
  content-forge vs run-binding paths (G).
- **Confirmed defect, fixed:** angle E — non-canonical stored body (key order).
  Root cause addressed by canonicalizing the body + enforcing canonical order;
  regression test asserts `ErrKeysNotCanonical`, and the `TestPutImmutableConflict`
  rationale is now literally true. Codex review then found a **second member of
  the same class** (empty vs nil, P2); per the escalation rule the class was
  widened and closed at the root (reject zero-key policies), not patched at the
  cited line. See the canonical-body decision above.
- **Accepted by decision:** the read path re-verifies content authenticity but
  not run binding — irrelevant, since a raw-DB attacker who forges
  `resolved_policies` also controls `runs.policy_digest`, so a read-side
  re-check adds no protection.

Added `TestGetResolvedPolicyRejectsForgedDigest` (store-internal, raw insert
past the `Put` boundary) so acceptance 2's "stored digest" half is nailed at the
persistence boundary, not only the domain unit.

## Verification

- `go build ./...`, `go test -race ./...`, `go vet ./...`,
  `golangci-lint run` (0 issues): all green.
- Acceptance 1: `ComputeDigest` documents the canonical form; `Validate`
  enforces canonical order. 2: forged digest rejected on write
  (`TestResolvedPolicyDigestMatchesRun`) and read
  (`TestGetResolvedPolicyRejectsForgedDigest`). 3: store binding test rejects an
  authentic policy whose content differs from the run's pin. 4:
  value/provenance-change-changes-digest and can't-store-under-old-digest
  (`TestResolvedPolicyDigest`).
- Golden fixtures for `resolved_policy` and `run` regenerated to the computed
  digest (`dc1af0…`), consistent across the domain and store copies.
