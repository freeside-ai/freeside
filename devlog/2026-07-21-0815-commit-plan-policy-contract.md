# The Commit-Plan Policy Contract Lands in the Trust Profile and Attention Item

Contract unit #222 (spine, `kind:contract`), implementing the policy
half of the settled commit-plan design
(2026-07-20-1145-gauntlet-commit-structure.md, Policy Gating and
Contract Classification; design PR #224). The two profile keys and the
encoding-version bump follow that note directly; what this unit had to
decide was the concrete shape of the plan-notice vocabulary across
domain, API, and app. #212 consumes these surfaces and owns all
emission and gating behavior.

## Decisions

**The notice rides `AttentionItem` as an optional daemon-derived
field** (`commit_plan_notice`, a pointer to a `CommitPlanNoticeReason`
enum; wire: required-but-nullable, the `decided_at` pattern). Rejected
alternatives:

- **A new `CandidateFinding` class.** The finding vocabulary is
  disposition-bearing publication-gate input (blocking/waived, waiver
  records, §3.1 waivability rules); the notice is informational
  surfacing about an import that still produced a candidate. Forcing
  it through the finding shape would either give it a fake
  disposition axis or special-case the gate.
- **A carrier on `Run`.** The run aggregate is stages and attempts;
  the notice is a fact a human should see when deciding, which is the
  §4 attention surface.
- **A wrapper struct (`PlanNotice{reason, ...}`).** The V1 fact is
  exactly the reason class; a wrapper is premature abstraction, and a
  later contract unit can widen the field to an object if #212's
  successors need detail (group ordinal, finding kind).

**Reason validity is the only validation arm.** Nil is absence; a
present reason must be one of the four classes. The notice is
deliberately transition-mutable (unlike `DecidedAt`): a remediation
import may legitimately produce a different notice, so pinning it
immutable would fight #212's own emission semantics.

**No `daemon/internal/signet` changes.** Signet serializes
`domain.AttentionItem` with `encoding/json`, so the daemon emits
`"commit_plan_notice": null` with no handler change, and the
signet-dev seed route gains no fake derivation path ahead of #212.
Real-daemon convergence therefore exercises only the null render;
the four non-null values are exercised by the domain/store round
trips and the MockServer tests. This is full parity with the daemon
that exists today: there is no derivation path to mirror until #212
lands emission.

**Wire shape: `nullable: true` + `allOf: [$ref]`** for the property,
keeping `CommitPlanNoticeReason` a clean top-level vocabulary schema.
swift-openapi-generator renders that as a single-value
`commit_plan_noticePayload` wrapper (`?.value1` at use sites); the
alternative, inlining the enum at the property, would fork the
vocabulary out of the enumerated-vocabularies block and lose reuse
for later carriers. The wrapper is generator noise, not contract
shape.

**Migration proof is two-sided.** Domain: the pinned v2 stability
digest fails `Validate` against v3 content
(`ErrProfileDigestMismatch`). Store: a literal v2-encoded row
(captured from the v2 build before the bump) fails the decode re-gate
with a hard error, not `ErrNotFound`; the first failing invariant is
the missing `commit_plan` member, with the version-string digest
mismatch behind it. Either way no stored profile is silently
defaulted; owner re-approval is the only path to a readable v3 row.

Revisit when: #212 emission needs more than the reason class on the
notice (widen the field to an object through a reviewed contract
bump); or a carrier beyond the attention item needs the vocabulary.

Follow-up: #212 (emission, gating, fixtures).
