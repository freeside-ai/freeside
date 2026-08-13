# Agent-Claims Record Migration and Store Accessors

Work unit: #732 (wave-5 contract chain, after #720; tracking #651;
blocks #381's driver work). Split from #381's implementation plan per
its recorded default. Mandatory note: `kind:contract` change and a
returned-object trust boundary (a decoded `agent_claims` row trusting
the claim set it carries). Scope: `daemon/` (`migrations`,
`internal/store`). No `api/`/`app/`/domain change (non-goal).

## What This Slice Is (and Is Not)

The persistence substrate only: an `agent_claims` table keyed by
invocation plus `WriteTx.PutAgentClaims` / `ReadTx.GetAgentClaims` with
`putImmutable` write-once semantics and fail-closed readback. No caller
writes the record yet; #381 binds the driver's `RecordClaims` to it
next. The driver, `ports.go` semantics, the shared driver-contract
tests, and any sync/API exposure stay out (all #381 or later).

## Decisions

- **Store-private `agentClaimsRecord` wrapper, not a domain type.** The
  record is `{InvocationID, []domain.AgentClaim}`. It exists because the
  kernel's `encode`/`decode[T]` demand a `Validate`, and the readback
  needs an embedded key to cross-check against the row's primary key. A
  package-private persistence format keeps the no-domain-change non-goal
  intact and is outside the golden convention (daemon conventions), so
  no golden fixture. Claims marshal through `domain.AgentClaim`'s
  existing wire shape, which embeds `Provenance` and carries no
  `publish_eligible` field, so no decoded trust bit exists to launder.
- **FK `invocation_id REFERENCES agent_invocations (id)`** (plan's
  recommended default, taken). Chose the FK over a bare PK because the
  invocation row is persisted at run creation, before any stage driver
  records claims, so the parent always exists; the FK closes the
  direct-SQL orphan gap the same way `0037_finding_dispositions.sql`
  did. Cost: store tests seed an invocation row first
  (`TestAgentClaimsForeignKeyRequiresInvocation` pins the refusal).
- **House shape keeps `entity_version`/`as_of_revision`.** The issue's
  Affected-interfaces line mandates the `agent_invocations` house
  pattern, so `PutAgentClaims` is modeled line-for-line on
  `PutAgentInvocation` (`VALUES (?, 1, ?, ?)`, `as_of_revision` stamped
  by the `WriteTx`). Not the `0043_intake_occurrences.sql` shape (which
  drops those columns): that record is daemon-internal by an explicit
  #720 decision, whereas here the issue fixes the column set.
- **Empty claim set is invalid at the record boundary.** The driver
  early-returns on `len == 0` (#381), so a persisted empty record has no
  writer and no consumer; `Validate` rejects it, keeping the tamper
  surface closed.
- **Order-sensitive identity stands** (#381's recorded assumption): the
  canonical encoding's byte equality is the immutability contract, order
  included. Every differing-set axis (label, digest, membership, text,
  order) is therefore a distinct `ErrImmutableConflict`, not a silent
  converge.

## Returned-Object Trust Boundary (Refute-First Pass)

`GetAgentClaims` returns fields of a decoded row. Two gates re-run on
read, both failing closed:

- **`decode` re-validation:** `agentClaimsRecord.Validate` requires a
  non-empty invocation key and a non-empty claim set, and re-runs
  `AgentClaim.Validate` at every position. That pins the agent producer
  class (a laundered `producer_class` is `ErrNonAgentClaim`) and the
  text/digest binding (an unbound text claim is
  `ErrClaimTextDigestMismatch`).
- **Key cross-check:** the decoded `InvocationID` must equal the queried
  key, else `errRowInconsistent`, mirroring `GetAgentInvocation`.

`TestGetAgentClaimsRejectsTamperedRow` writes each tampered shape past
the Put boundary as raw SQL (laundered producer class, body key
disagreeing with the row key, digest-unbound text claim, empty set) and
asserts the matching sentinel.

Findings — refute-first pass (fresh-context reviewer, refute-only). No
CONFIRMED defect; two PLAUSIBLE design-intent questions, both declined:

- **Confirmed and fixed:** none. The reviewer verified the putImmutable
  identity (map-free struct graph, `*ClaimText` dereferenced on marshal,
  every conflict axis reaching the byte compare and the replay
  re-encoding identically), the four fail-closed tamper sentinels, the
  FK direction/column set/STRICT typing against the house shape, the
  `as_of_revision`/`entity_version` stamping, both exclusion-list joins,
  and all eight head-version bumps.
- **Declined — couple each claim's `producer_invocation_id` to the
  record key.** A tampered row could carry a claim whose
  `provenance.producer_invocation_id` names a different invocation than
  the PK, since `AgentClaim.Validate` checks a claim in isolation.
  Declined: `producer_invocation_id` is provenance about the
  *referenced artifact's* origin, semantically distinct from the
  owning-invocation key; no privilege rides on it (agent claims are
  never publish-eligible), and the domain asserts no coupling (nor does
  `AttentionItem.Validate`). Enforcing equality would impose an
  unverified invariant that could fail closed on a legitimate
  cross-invocation claim — the read-re-gate over-reach the #720 note
  records. Whether the writer guarantees it is a #381 question (it owns
  the driver); surfaced there, not enforced blind here.
- **Declined — enforce set-level claim coherence (one artifact id → one
  digest, dedup).** The reviewer's "artifact-identity uniqueness" framing
  is inexact: `AttentionItem.Validate` does not dedup claims (it allows
  same id + same digest under different labels, attention_item.go:849);
  it forbids only one id mapping to two digests and claim/evidence id
  reuse (the latter has no analog here). Declined: that coherence is the
  consuming aggregate's invariant, applied when claims render on an item
  (which #381 does); a tampered set is caught there with no trust
  escalation at `GetAgentClaims` (each claim individually passes
  `AgentClaim.Validate`). Re-implementing the aggregate's rendering
  invariants in the substrate couples the layers and exceeds the
  contract's readback scope (`AgentClaim.Validate` + row-key
  consistency). The store persists the driver's set faithfully.

## Rejected Alternatives

- **Bare PK without the FK:** would leave the direct-SQL orphan gap the
  `0037` precedent closes; the FK costs only a seeded parent in tests.
- **A domain-level claim-set type:** out of scope (non-goal); the
  store-private wrapper carries the persistence-only concern (the
  embedded key) without touching the domain.
- **Dropping `entity_version`/`as_of_revision`** (the `0043` shape):
  contradicts the issue's mandated house pattern.

## Revisit When

- **#381 binds `RecordClaims` to these accessors:** confirm the driver's
  claim-set ordering matches the order-sensitive identity here, and that
  its `len == 0` early-return still holds (the empty-set rejection
  assumes it). Decide there whether a claim's
  `provenance.producer_invocation_id` must equal the record key (the
  declined refute-first finding): if the writer guarantees it and a
  consumer relies on it, that coupling belongs in the domain invariant,
  not bolted onto this store boundary.
- **A consumer needs the record sync-carried** (run timeline, labeled-
  claim rendering): that promotes it to a wire contract (domain + api +
  app), a separate `kind:contract` unit, not a daemon-only change.
