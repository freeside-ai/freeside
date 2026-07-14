---
run: manual
stage: authoritative-approval-bindings
date: 2026-07-14
branch: feat/authoritative-approval-bindings
---

# Make approval bindings authoritative and durable (issue #32)

Spine-role session. #32 is the second Track A (`kind:contract`) unit of the Wave 0
exit-fixes batch (#4), self-selected: predecessor #33 merged (PR #43), no active
claim (the only open PR was #44, Track B `exec/fake`, no path overlap), and
contract serialization holds because the one other scheduled open contract unit,
#37, has #32 in its Dependencies chain (`#33→#32→#37`, exempt per the PR #12
amendment). Deferrals #22/#28 unmilestoned/dormant. Declared paths:
`daemon/internal/domain`, `daemon/internal/store`, `daemon/migrations`, `api`.
PR #45. Cross-component: api and both consumers moved as one unit.

The finding: an item carried a caller-supplied `artifact_digests` the validator
only checked for non-empty entries, so it could display digest A while binding B;
and the schema kept no immutable record of what a decision accepted. Both are the
non-waivable stale-approval class (plan §3.1, §4).

## Decisions

- **Binding set is derived, not supplied.** `artifact_digests` = the canonical
  (sorted, deduplicated) union of the evidence and claim digests rendered on the
  item. `NewAttentionItem` derives it; `Validate` re-derives and requires equality
  (`ErrBindingMismatch`), so the store-decode path is held to it too. The field is
  removed from `AttentionItemInput`, making a divergent item unrepresentable
  (mirrors `Timing`/`PublishEligible`, computed not input). Chose derive-and-enforce
  over cross-validate-caller-input because the field is fully redundant with the
  rendered inputs; the #33 `NewResolvedPolicy` precedent (constructor canonicalizes,
  Validate requires) is the same shape.
- **Binding set includes claim digests, not just evidence.** The user is shown both
  evidence and labeled agent claims when deciding, so both are bound; a claim digest
  changing after render invalidates the approval. Rejected evidence-only binding as
  under-covering the rendered surface.
- **Durable record is write-once, keyed by `command_id`.** New `domain.Command`
  (command_id, device, item, accepted item_version, pr_head_sha, canonical
  artifact_digests, action) modeled on Finding/ResolvedPolicy: `Validate` only, no
  transition validator, no `entity_version`. `command_id` is the client idempotency
  key / PK, not a content digest, so no `ComputeDigest`. The committed result is the
  row's `as_of_revision` (the client-visible revision it applied at), not a body
  field, so a retry returns the original revision for free.
- **Store checks idempotency BEFORE binding authority.** `PutCommand` short-circuits
  on an existing `command_id` (identical body converges, different body
  `ErrImmutableConflict`) *before* cross-checking bindings against the live item.
  This is load-bearing: a retry of an already-committed command after the item
  advanced must return the original (§5.14 test 4), not be re-judged stale. Only a
  genuinely new command runs the binding cross-check; a mismatch returns
  `*StaleCommandError` carrying the current item as the replacement (§5.14 test 2).
- **`pr_head_sha` is NOT pinned in `ValidateAttentionItemTransition`.** A remediation
  head legitimately changes the head on a new item_version (plan §5.15). Head
  authority comes from the command record + stale-rejection, not from freezing the
  item field. Rejected pinning it as it would forbid legitimate remediation.
- **API: type the decision command; lightweight verification (user choice).** Typed
  `DecisionPayload` (item_id, action, bound tuple) replaces the free-form payload;
  `CommandRecord` mirrors `domain.Command` and `CommandResult` becomes
  {record, revision}. No Go round-trip harness / new dependency: acceptance 4's
  displayed-vs-bound and post-render-change cases are carried by the domain + store
  Go tests, and api examples are lifted from the regenerated domain goldens so
  `oas3-valid-schema-example` proves them (the repo's provisional-API convention).

## Verification

- Full daemon suite green (`go build/test -race/vet`, `golangci-lint` 0 issues);
  api `vacuum` 100/100. `TestMigrateFreshAndIdempotent` tracks 0004 via the glob, no
  test edit needed.
- Regenerating goldens surfaced the visible ripple: the item's `artifact_digests`
  went `["sha256:log"]` → `["sha256:img","sha256:log"]` (the claim digest is now
  bound), in the domain and store goldens and the api example — the concrete proof
  the claim channel is inside the binding set.

## Refute-first pass (returned-object-trust / data-integrity boundary)

One fresh-context reviewer, diff + stated intent only, prompted to disprove each
contract point. Ledger:

- **Confirmed sound (no defect):** binding equality — `bindingDigests` runs after
  the evidence/claim loops that reject empty digests, so the derived set is never
  empty-entry and the removed per-entry check is subsumed; dup/cross-channel/order
  all collapse via `Sort`+`Compact` on both sides of `slices.Equal`; a forged stored
  `artifact_digests` is caught by `GetAttentionItem`→`decode`→`Validate`. Nil/empty
  stability (both `bindingDigests` and `NewCommand` force nil; `slices.Equal(nil,nil)`
  holds). Canonicalization symmetry (`Digest` is `string`, same byte order
  everywhere; `encode`'s `Validate` rejects a non-canonical command body before
  insert, so no false idempotency conflict). Idempotency-before-binding order in
  `PutCommand`. `StaleCommandError` `Is`/`As` through the `%w` wrap, replacement
  carries the advanced version. `GetCommand` cross-checks all six columns. Migration
  0004 (STRICT, contiguous, FK to a PK, no BEGIN/COMMIT, `pr_head_sha` empty≠NULL).
  Single-writer tx: the intra-tx `GetAttentionItem` reads committed prior state.
- **Accepted and acted on (not a defect, a coverage gap):** the crux of §5.14 test 4
  — a retry of an already-committed command *after the item advanced* converging
  rather than being re-judged stale — was correct by the check ordering but untested
  (the replay test replayed before advancing). Added the missing store subtest
  (`committed command still converges after the item advanced`).
- **Rejected by verification:** none — no confirmed defect to reject.

## Codex review (round 1)

Two findings, both accepted and fixed (folded into the daemon commit):

- **P1 (correctness): unoffered action durably accepted.** `PutCommand` bound the
  live item's version/head/digests but never checked the action was one the item
  offered, so a client could record e.g. `stop` against an item offering only
  `open_pr`/`return_to_agent`/`dismiss`. Added `AttentionItem.Offers` and a store
  check (`ErrActionNotOffered`) after the stale check. In scope: it is the same
  binding-authority remit as the version/head/digest checks. (Status-gating a
  command against a non-open item is a separate lifecycle concern — a resolution
  bumps the version, so a decision on a superseded item is already caught as stale;
  not added here.)
- **P2 (wire mismatch): empty binding set serialized as `null`.** `bindingDigests`
  and `NewCommand` returned nil for the no-artifact case (nil → `null`), but the
  OpenAPI declares `artifact_digests` a required non-null array. Made both always
  array-shaped (`[]`). Safe for idempotency: the command record is keyed by
  `command_id`, not a content digest, and `slices.Equal` treats nil/empty as equal
  so a legacy-`null` decode still validates. This reverses the initial nil choice;
  the #33 nil-preserving rationale was content-digest body-stability, which does not
  bind here.

## To promote

- None this session. (The store now has three write-once-with-in-tx-cross-check Puts
  — ResolvedPolicy, and now Command's binding check — but the shape isn't yet
  repeated enough to abstract; leave concrete.)
