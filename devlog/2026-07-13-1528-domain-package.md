---
run: manual
stage: domain-package
date: 2026-07-13
branch: feat/domain-package
---

# Domain package (Wave 0 unit 2)

Spine-role session: Wave 0 unit 2 (#7, `kind:contract`), the shared
`daemon/internal/domain` vocabulary the four later Wave 0 units and every
Wave 1 lane depend on. Types and validation only, no I/O. Selection was
mechanical per tracking issue #4: #7 is the first unchecked box, its
dependency #6 is merged, no open claim. Declared paths held:
`daemon/internal/domain`, `devlog/`. PR #19.

## Decisions

- **Invariants enforced at the validation boundary, not by hiding
  fields.** Chose exported json-tagged fields + `Validate()` +
  `New…`/input-struct constructors over unexported-fields-with-getters.
  The package is a contract other lanes (store, api, signet) serialize
  and round-trip; unexported fields would force a hand-written
  `MarshalJSON`/`UnmarshalJSON` on every type, doubling the surface and
  minting an expensive pattern for every lane to copy. Golden coverage
  (crit 9) then comes free from `encoding/json`.
- **Trust-gated fields split input from output.** `publish_eligible`
  (§5.15) and message `sequence` (§5.14) and item `timing` (§4) must
  never come from a caller. Rather than trust a "don't set this" comment,
  the *input* struct omits the field entirely (`ArtifactInput` has no
  `PublishEligible`, `NewMessage` has no sequence param,
  `AttentionItemInput` has no timing), so the illegal input is
  unrepresentable; the field is exported only on the output type, set by
  trusted computation (`computePublishEligible`), the appender
  (`Conversation.Append`), or the derivation (`WithTiming`). This is what
  makes crit 2/5/6 structural facts rather than tested conventions.
- **Enums are named strings, not iota.** The JSON/golden token is the
  human-readable string, stable on the wire for store/api; the zero value
  `""` is invalid-by-design and rejected by every `Validate`. Rejected
  iota (zero is a real member; wire values become opaque ints).

## To promote

Conventions this unit introduces, flagged for spine review at Wave 0
exit. All are documented at point-of-use (`doc.go`, the switch comments
in `artifact.go`) and internal to `daemon/`, so none needs AGENTS.md
promotion now; elevate to a cross-cutting section only if spine wants
them binding beyond this package.

- **Enum convention**: named string + `valid()` predicate + `AllX` slice
  (the single registration point, drives the tables).
- **Switch convention**: a validity `valid()` switch uses `default`
  (it is a predicate); a switch that *dispatches behaviour* on an enum
  omits `default` so `exhaustive` forces a new member to be handled, with
  a trailing fallback return for the invalid zero value
  (`computePublishEligible`, `EligibleForEvidenceSnapshot`).
- **Golden convention**: `json.MarshalIndent` of a fixed, valid fixture
  (UTC-fixed times, pointer-for-optional rendering explicit null, no map
  fields in any serialized shape); fixtures double as
  validation-positive cases.

## Deferred (provisional, for a later kind:contract change)

The plan names these fields but enumerates no members; I chose minimal
provisional member sets from surrounding plan text rather than leave them
untyped `string`, so they are validated now and a contract PR can widen
without churn. Not escalated to issues: they drain naturally when the
consuming lane (signet) needs them, well within Wave 1.

- **`Priority`** {low, normal, high, urgent}, **`ItemStatus`** {open,
  resolved, superseded, dismissed, expired}, **`SensitivityClass`**
  {normal, sensitive, high_sensitivity}, **`Author`** {user, agent,
  daemon}: provisional vocabularies.
- **`Action`** (the §4 union) is modeled here; the *per-type allowed
  action set* is signet policy, deferred to that lane, not domain
  vocabulary.
- **Timing lives on the item** as a `WithTiming`-only field rather than
  compute-on-demand-with-no-field. If spine prefers the latter it is a
  one-type change; the derivation function (`TimingAggregates`) is the
  same either way.

Pre-existing queue swept: the only open promote/deferred item across the
devlog is the Phase 4 license ADR-candidate (`-> Refs #18`), untouched by
this unit; nothing to drain here, no new escalations.

## Verification

- Golden fixtures double as validation-positive cases: a fixture that
  fails `Validate` fails the golden test, so the two can't drift.
- `time.Duration` serializes as int64 nanoseconds (`submit_to_first_open:
  300000000000`); acceptable and deterministic, noted so a reviewer
  isn't surprised the golden isn't `"5m0s"`.
- Every commit verified green in isolation (`git rebase --exec` build +
  test over the four); bisect-safe. Full run record in the PR body.

## Review rounds (Codex)

**Round 1 — two P2, both accepted, folded into the attention-model
commit.** Both were real gaps against plan invariants, not style:

- **Provenance dropped head binding.** `Provenance.Validate` accepted an
  empty `source_head_sha`, so a verifier/daemon artifact could enter
  evidence unbindable to a head. §5.15 rule 2 lists `source_head_sha` as
  a non-optional provenance field (only `verification_recipe_digest?` is
  optional) and requires head-binding before publication, so an empty
  value is now rejected in `Provenance.Validate`. The plan's
  "explicitly head-independent" evidence is a publisher (1B) concern, not
  modeled in the domain type now (no premature abstraction).
- **Delivery status could outrun its receipt.** A `DeliveryOpened` row
  with `opened_at == nil` (or `channel_accepted` without
  `channel_accepted_at`) validated, letting telemetry claim a stronger
  lifecycle state than the timestamps prove: exactly the dishonesty the
  §4/decision-11 status vocabulary exists to prevent. `Validate` now
  requires the matching receipt timestamp per status (an exhaustive,
  default-less switch, per the switch convention above). Minimal: each
  status requires only its own receipt, not all prior ones.

No class siblings: `source_head_sha` was the one unchecked required
provenance field, and the status/timestamp coupling is the delivery
type's only such pairing.

**Round 2 — three P2, one class, all accepted, folded.** All three were
the **returned-object-trust-boundary** class (AGENTS.md finish-line risk
list): caller-owned mutable data escaping a validation boundary. Fixed as
a class, not line-by-line:

- **Constructors aliased caller-owned references.** `NewArtifact` stored
  the caller's `*Digest` recipe pointer and `NewAttentionItem` stored the
  caller's slices, so post-validation mutation could change a validated
  value (swap an agent artifact into gated evidence, retarget a recipe
  after eligibility was computed). Both constructors now detach every
  caller-owned reference: `clonePtr` for pointers (recipe, run-id,
  conversation, expiry), `slices.Clone` for the value-element slices, and
  a deep `cloneArtifacts`/`Artifact.clone`/`Provenance.clone` for evidence
  (whose elements themselves hold a pointer, so a shallow clone would
  leak). The two constructors are the only validated-return boundaries in
  the package; the value-only `Validate`-on-a-value types (policy, run,
  invocation) have no such boundary.
- **`Validate` wasn't a deserialization backstop.** An `Artifact`
  reconstructed from the store or built as a literal bypasses `NewArtifact`,
  so an agent artifact with `publish_eligible: true` validated. `Validate`
  now rejects a `publish_eligible` inconsistent with provenance as far as
  is checkable without policy (agent never eligible; eligibility needs a
  recipe digest). The approved-recipe half stays policy-gated in
  `EligibleForEvidenceSnapshot`; `AttentionItem.Validate`'s doc now states
  that a store-reconstructed item must re-run the gate, not rely on
  `Validate` (refute-pass finding, below).

**Refute-first pass** (AGENTS.md mandate for trust-boundary changes): one
read-only fresh-context lens, given only the diff + intent, tasked to
disprove the fixes. It could construct no mutation that slips past the
copies and found no nil/empty golden regression. It surfaced two items,
both addressed: the `AttentionItem.Validate` reconstruction-gap note
above (rejected-by-design as code, accepted as a contract doc), and a
near-vacuous test assertion (now checks the item's element is actually
unchanged, not just that the item still validates). Rejected-by-verification,
not re-raised: `AgentInvocation.InputIDs`, `ResolvedPolicy.Keys`, and
`NewMessage` are not trust boundaries (value-only, or `Append` already
clones).

**Round 3 — three P2, one class, a recurrence of my own round-1 miss.**
All three were "a validator does not enforce a required structural
field": `Run` accepted an empty `project_id`/`policy_digest`, `Stage`
and `Attempt` accepted empty parent join keys (`run_id`, `stage_id`),
`AttentionDelivery` accepted an empty `channel` / non-positive
`attempt`. This is the same class as round 1's `source_head_sha` gap;
I patched that one field and did not sweep the siblings, so the class
recurred (my miss, not non-convergence, per AGENTS.md). Widened the
boundary and swept **every** `Validate`, enforcing one principled
category: **structural identity, scope, and parent-join keys**
(`AttentionItem.project_id`, delivery `channel`/`attempt`, run
`project_id`/`policy_digest`, the `run_id`/`stage_id` join keys, plus
parent-key cross-checks so a `Stage` must name its `Run` and an
`Attempt` its `Stage`), and validating the previously-unchecked
`AgentClaim`. Deliberately **left optional**: free-text descriptive
fields (`reason`, message body, finding message, policy value) the plan
does not mark required, drawing the enforce/allow line at
structural-vs-descriptive rather than field-by-field, so the boundary
is defensible against the next round rather than reactive to it. No
refute subagent this round: additive presence checks are not a
destructive/credential/trust-boundary change (the earlier refute pass
covered the aliasing design); self-reviewed instead.

**Round 4 — three P2, same validation-completeness class, closed by
enumeration.** Findings: delivery timestamp consistency was only
one-directional (a `submitted` row with an `opened_at` validated,
the reverse of round 3); `AgentClaim` accepted an empty `digest` (its
content address); `EligibleForEvidenceSnapshot` skipped `Artifact.Validate`
so a malformed artifact could enter evidence. Two were again my own
half-fixes from round 3. AGENTS.md's rule for validation code is
explicit: stop pattern-widening per round and run **one adversarial
enumeration** of the input space instead (it cites eight rounds wasted
on one class before enumeration closed it). So this round I fixed the
three, swept every sibling I could see (delivery bidirectional
consistency, `item_version`≥1, each `artifact_digests`/`input_ids`
element, message/finding anchor `created_at`, the gate calling
`Validate`), **then ran a read-only enumeration lens over every
validator field-by-field** to find what I still missed. It found exactly
one: a non-nil `VerificationRecipeDigest` pointing at `""` (a
present-but-empty content address hidden behind a pointer the earlier
non-empty sweeps didn't reach) — now rejected. It confirmed every other
validator complete and flagged no over-constraint, and caught a cosmetic
sentinel mismatch (`Classification.Version` used `ErrEmptyField`, now
`ErrNonPositive`). The enforce/allow boundary held: descriptive free-text
(reason, body, finding source/message, policy value, classification note)
stays optional by decision, not omission.

**Round 5 + a maintainer review pass — two more dimensions closed.** Codex
round 5 raised one P2 (delivery timestamps could be temporally
out-of-order, e.g. opened before submitted, a dimension I had *explicitly
deferred* in round 4 as skew-sensitive). The maintainer independently
reviewed the same head and handed six P2s (one overlapping Codex's).
Reversed the round-4 deferral and accepted temporal ordering: a receipt
before submission is a causal impossibility that yields a negative
open-to-submit duration in the product metric, and skew at lifecycle
magnitudes (steps seconds apart) is negligible; enforced the full
monotonic chain, contained to `AttentionDelivery` (the only type with
multiple orderable timestamps). The remaining five, by class:

- **Collection integrity** (maintainer findings 3-5, swept together as
  instructed, plus the `EvidenceSnapshot` sibling): every collection whose
  elements carry an identity/ordinal now rejects duplicates, resolved
  policy keys, run stage ids, attempt id/number/invocation-id within a
  stage, conversation message ids, evidence artifact ids. Bare-value lists
  (artifact_digests, input_ids, requested_decision) allow redundancy by
  decision (no identity to be ambiguous).
- **Trusted-timing boundary**: `WithTiming` derived card timing from
  unvalidated, possibly foreign deliveries. It now validates each delivery
  and requires `delivery.ItemID == item.ID`, returning an error (signature
  changed to `(AttentionItem, error)`; the golden and timing tests adapt).
- **Subject/run-id consistency**: a project- or system-scoped subject
  could carry a `run_id`. Now rejected (allowed only for run and
  proposal-batch subjects). Note the *opposite* direction, requiring
  `run_id` when the subject *is* a run, stays deliberately unconstrained
  (the enumeration confirmed the fixture proves it optional).

Folded across three commits (attention model, run records, golden) via a
three-stage rebase. This is the sixth review round on validation
completeness; the class is now covered across presence, level-consistency,
temporal order, collection integrity, and cross-field scope. Genuinely
new dimensions each round, not the same miss re-surfacing.

**Round 6 — three P2, each a follow-on to a prior fix.** (1) Evidence
artifacts kept the caller's `publish_eligible` bit: a verifier artifact
under an approved recipe supplied `false` stayed `false`, contradicting
"computed by trusted policy." `NewAttentionItem` now *recomputes* the bit
per evidence artifact via `computePublishEligible` (it has the recipe set
in scope), so the snapshot never carries a caller-set eligibility. (2)
`invocation_id` uniqueness was stage-local, but it is the run-wide
reconciliation key (§5.3 at-most-one-accepted-result), so `Run.Validate`
now tracks it across all stages. (3) The pointer-emptiness class (first
seen round 4 as an empty recipe digest behind a non-nil pointer)
recurred at `Subject.RunID`: swept it across every optional pointer, so a
non-nil pointer to an empty id / zero time is rejected (`Subject.RunID`,
`ConversationID`, `ExpiresWhen`); delivery timestamp pointers were already
covered by the round-5 ordering check (a zero time is "before submitted").

Convergence note: six rounds, findings steady at ~3/round, each a
genuinely distinct invariant family (never the same miss), each swept as
a class. The validation surface of a rich domain contract is simply
large; the value has stayed real, so continuing was correct, but this is
the point to checkpoint with the maintainer on how far to drive review vs
hand off, rather than spin indefinitely. Maintainer chose to keep the
loop going.

**Round 7 — one P2, the gate-level complement of round 6's recompute.**
Round 6 recomputed `publish_eligible` in `NewAttentionItem`, but the
documented store-reconstruction path calls `EligibleForEvidenceSnapshot`
directly, and that gate only checked recipe membership, so a decoded
artifact with a stale bit (approved recipe but `publish_eligible: false`)
passed and stayed wrongly non-publishable. Made the gate the single
publish-eligibility authority: it now rejects a bit that disagrees with
policy in *either* direction (`Validate` catches true-when-wrong without
the recipe set; the gate catches false-when-should-be-true with it).
Reordered `NewAttentionItem` to recompute-then-gate so the normalized bit
satisfies the stricter gate. Contained to `artifact.go` + the constructor.

Process gotcha (recorded for the watch protocol): the round-7 review
watcher missed this because its baseline was anchored to *watch-start*
(21:48) rather than the *push event* (~21:46), and Codex had already
posted at 21:46:52; the maintainer surfaced it. Anchor a review-watch
baseline to the push it should trigger a review of, not to the moment the
watch is armed, or a fast reviewer's pass lands pre-baseline and is
silently banked as already-seen.

**Round 8 — two P2, a new dimension: timing trustworthiness.** (1) `Validate`
never inspected the `Timing` field, so a store-reconstructed item with an
impossible `TimingSummary` (negative count, mis-ordered first instants, a
submit-to-open gap that disagrees with its endpoints) validated. Added
`TimingSummary.Validate` (accepts exactly what `TimingAggregates` can
produce) and call it from `AttentionItem.Validate`. (2) `WithTiming`
double-counted a duplicated delivery attempt (same device/channel/attempt),
inflating `delivery_count` from store/outbox retries; it now rejects
duplicates on that key (the collection-integrity class applied to the
delivery set). Contained to the attention model.

**Round 9 — two P2 on the round-8 timing validator; one was my
regression.** (1) My `TimingSummary.Validate` asserted
`first_opened >= first_accepted`, but those are independent minima over
*different* deliveries (an opened-only delivery plus a later
channel-accepted one legitimately gives opened < accepted), so
`WithTiming` could produce an item that then failed `Validate` — a
false-positive I introduced in round 8. Dropped that cross-receipt check;
the only cross-aggregate invariant that holds is that each receipt is
>= `first_submitted` (the per-delivery ordering from round 5 is the real
guarantee). Added an end-to-end regression: `WithTiming` output over such
deliveries must pass `Validate`. (2) The count/endpoint agreement was
unchecked (a reconstructed `count:0` with a receipt, or `count>0` with no
`first_submitted`), now required. Lesson: when adding a backstop validator
for a computed shape, derive its predicate from what the producer can
actually emit (independent minima are not monotonic), not from an assumed
"tidy" ordering.

**Round 10 (maintainer, five) + Codex round 10 (one) — cross-record and
contract invariants.** All six real and plan-grounded:

- **Evidence head-binding** (§5.15 rule 2): `AttentionItem.Validate` now
  requires each evidence artifact's `source_head_sha` to equal the item's
  `pr_head_sha` when present (prior-head evidence in a new-head item;
  head-independent evidence is unmodeled). Golden fixture aligned.
- **Artifact identity across channels**: an `ArtifactID` maps to one
  digest and does not span evidence and claims (a claim may not reuse an
  evidence id, nor give one id two digests; shared id+digest across labels
  is allowed).
- **Agent recipe provenance**: `Provenance.Validate` rejects a recipe
  digest on an agent artifact (machine-checkable falsehood, §5.15).
- **Invocation inputs**: `AgentInvocation` requires >=1 input (the
  reproducibility binding, §5.14).
- **Attempt ordinals**: `Stage.Validate` requires attempt `Number` to run
  1,2,3,... in slice order (retry ordinal + serialized history), replacing
  the weaker uniqueness check.
- **Zero timing endpoints** (Codex): the pointer-emptiness class reaches
  `TimingSummary` too, a non-nil endpoint pointer to a zero time is
  rejected (TimingAggregates never emits one).

Ten rounds now; findings still real and distinct, alternating maintainer
manual passes and Codex. Continuing per the maintainer's call.
