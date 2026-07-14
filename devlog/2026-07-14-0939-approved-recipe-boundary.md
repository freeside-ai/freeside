---
run: manual
stage: approved-recipe-boundary
date: 2026-07-14
branch: fix/approved-recipe-boundary
---

# Approved-recipe persistence boundary (issue #31)

Spine-role session, fiat-assigned #31 (`kind:contract`, `lane:spine`, 1A), a
Wave-0 adversarial finding against plan §3.1 / §5.15 rule 2. The domain enforced
recipe approval only inside `NewArtifact` / `NewAttentionItem`; the store's
`Put*`/`Get*` for `Artifact` and `AttentionItem` called only policy-free
`Validate`, so a caller handing the store a raw exported struct could persist a
verifier artifact under an *unapproved* recipe with `publish_eligible: true`
(standalone or embedded in evidence) and read it back as valid. This closes that
returned-object-trust-boundary hole. Declared paths held:
`daemon/internal/domain`, `daemon/internal/store`, `devlog/`. No migration. PR #40.

## Decisions

- **Injected approved-recipe set on `store.Options`, threaded into `ReadTx`/
  `WriteTx` like `asOfRevision`, over a per-transaction parameter or a persisted
  approval table (user-confirmed).** The approved-recipe set has no persisted
  form and no production resolver yet (it lives only as a `map[Digest]bool` in
  the domain constructors and test fixtures); `ResolvedPolicy` does not carry it.
  A per-transaction parameter would change every `Write`/`Read` call site and
  invent plumbing ahead of the policy source that would feed it. A schema table
  would persist a set nothing yet produces. The injected set is the minimal seam
  and needs no migration. **Provisional and process-global**: to be replaced by a
  per-run/per-policy resolver when policy resolution is wired.
- **Fail closed by default.** Nil approved set = nothing approved. The store, not
  the caller, owns the approval decision, so even a legitimately-constructed
  eligible artifact is rejected by a store that approves nothing. No production
  caller persists eligible evidence yet, so nothing breaks; store tests that
  persist the fixtures open with the fixture's approved recipe.
- **Reconstruction rejects loudly, over silently recomputing (user-confirmed).**
  A decoded row whose `publish_eligible` disagrees with the current approved set
  fails the Get, mirroring the store's existing `errRowInconsistent` discipline
  (fail fast for data integrity). Silent recomputation would return a struct that
  disagrees with its own stored body and mask a corrupt row.
- **Two domain gates, by shape.** `ValidatePublishEligibility` (new, exported)
  asserts a *standalone* artifact's bit equals `computePublishEligible`: an agent
  artifact with the bit false is a legal standalone row, so the evidence gate
  (which rejects agent artifacts) can't be reused there. Embedded evidence keeps
  `EligibleForEvidenceSnapshot` (rejects agent artifacts, requires approval,
  checks the bit). The store's `gateEvidence` runs the latter per snapshot entry.
- **Gate before the replay short-circuit.** In `PutAttentionItem` the evidence
  gate runs before the byte-identical-replay early return and the transition
  guard, so an idempotent replay is gated too, not waved through.

## Verification

- `go build/test/vet ./...` and `golangci-lint run` (v2.12.2, matching CI): green.
  (A stale golangci cache referenced a deleted worktree path; `cache clean`
  cleared it, 0 issues.)
- Acceptance #1/#3: new store regression tests fail closed on all four paths —
  Put/Get × standalone artifact / embedded evidence — under an empty (and a
  wrong) approved set, plus a positive control that a legal non-evidence agent
  artifact still persists and reads. Domain `TestValidatePublishEligibility`
  covers forged-true / stale-false / legal cases. Acceptance #2: the store
  receives the approved-recipe context. Acceptance #4: both row shapes covered.
- **Refute-first pass** (returned-object-trust + artifact-integrity path): an
  independent lens was tasked to find any remaining path to persist or
  reconstruct a forged `publish_eligible` (other write/read paths, outbox,
  `WriteInternal`, gate ordering, the `old`-decode, map aliasing).
  - Confirmed (real, fixed before this record): none. The lens found no bypass.
  - Rejected-by-verification: all eight angles checked-safe with `file:line`
    reasoning, so none re-raise. Load-bearing ones: the `PutAttentionItem`
    evidence gate runs strictly before the byte-identical replay short-circuit
    and the transition guard; the `old`-decode feeds only the transition check
    and is never returned or persisted; both Get gates run on the returned value
    after the column cross-check; the standalone bit-equality check does catch a
    forged `true` under an unapproved recipe (`computePublishEligible` returns
    false → mismatch); `approvedRecipes` is populated at the only two tx
    construction sites (`Read`, `transact` serving both `Write`/`WriteInternal`),
    nil-by-omission fails closed; the outbox stores an opaque payload and never
    reconstructs these types. The store package is the entire persistence surface
    for both types today (other lanes are doc.go stubs; `exec` carries only
    `[]Digest`).
  - Accepted-by-decision: the provisional process-global set (per-run resolver
    deferred); reconstruction rejects rather than recomputes — so legitimately
    approved-at-write evidence fails closed if the global set later shrinks,
    which is the intended behavior, asserted by the Get regression tests.

## To promote

- **Invariant**: reconstruction/persistence boundaries re-run the policy gate
  against the current approved-recipe set; the store never trusts
  `publish_eligible` or recipe approval decoded from a row or handed in as an
  exported struct. Candidate for an AGENTS.md trust-boundary line if it recurs
  beyond this store. -> open

## Notes

- No new deferral queue items beyond the promotion candidate above. #31 drains
  via `Closes #31`; tick Wave 0 tracking (#4) on merge.
- The process-global approved set is a known limitation, not a gap: it is the
  minimal fail-closed seam until a per-run policy resolver exists (recorded on
  `Options.ApprovedRecipes`).
