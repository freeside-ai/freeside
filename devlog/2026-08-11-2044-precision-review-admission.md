# Precision-First Review Admission

Issue: #680

## Decision

Chose a precision-first finding admission rubric over the former instruction to
return every actionable finding and reserve an empty array for a clean
candidate. A finding now has to be introduced by the candidate, have a
demonstrable failure path, be discrete and actionable, exclude speculative,
pre-existing, and stylistic concerns, and survive a deliberate falsification
attempt. An empty array means that no finding met this bar, not that the
candidate is flawless. This restraint is safety policy while each finding can
interrupt the operator through an AttentionItem.

Chose three daemon-owned rules: re-derive trust from current trusted state,
preserve verification integrity, and contain credential material. Each rule
pairs the unsafe change with the safe implementation path so the reviewer can
distinguish a violation from the supported design. The credential rule permits
Freeside's bounded private, daemon-owned ephemeral snapshot and read-only
volume delivery path while rejecting uncontained logging, export, persistence,
or exposure.

Chose to deliver the rules through `codexProductionReviewPrompt`, the existing
daemon-owned CLI task-prompt argument, and advance its protocol to
`codex-production-review-prompt-v3`. This separates the rules from raw operator
and repository Markdown without changing `ReviewInstructionBinding` or
`codex_explicit_bundle_v1`. The operator-host snapshot therefore keeps its
original contract: exact operator bytes when present and explicit absence when
not.

This is the strongest authority separation the current harness offers, not a
mechanical non-relaxation guarantee. Codex receives the rules through the task
prompt rather than an ambient instruction file, but operator or repository
prose can still argue against them and model behavior remains probabilistic.
The prompt protocol and review-configuration digest make the compiled policy
identity explicit; the existing park/adopt path handles runs pinned to an older
protocol.

## Rejected Alternatives

- Rejected every ordering and delimiter inside one flat host document. The
  pre-push refute-first review showed that later operator text can claim to
  override earlier rules; automated PR review then showed that earlier
  operator text can syntactically or semantically capture later rules. A fence
  chosen around the input closes only the syntactic case, not the semantic one.
- Rejected a separate daemon-rules block in
  `codex_explicit_bundle_v2`. Besides changing the shared `exec` contract and
  invalidating persisted bindings, it remains one Markdown document with
  operator and repository bytes, so it does not escape the capture class.
- Rejected sourcing the Freeside rules from repository `AGENTS.md`. Repository
  instructions come only from the trusted exact base and remain a separate
  scoped source; candidate-controlled instructions must not govern their own
  review.
- Rejected changing the structured finding schema. Location shape and `P0`
  severity belong to #679 and are independent of the admission threshold.
- Rejected adding a second model verifier. The rubric requires the reviewer to
  falsify its own candidate finding without introducing another model stage.

## Verification Findings

The current live Codex review suite proves container topology and instruction
delivery, but it does not exercise model judgment. This unit therefore pins the
prompt criteria, exact base/head and evidence binding, prompt protocol, and an
unsafe/safe text pair for each daemon rule. A model-behavior claim remains
outside the available harness.

The independent refute-first review confirmed two defects before the first
push: rules preceding operator text could be contradicted, and the initial
credential wording falsely prohibited the supported ephemeral access-token
delivery path. Both were corrected. Automated review then disproved the
mirror-image ordering: raw operator Markdown before the rules could capture or
reframe everything that followed. That recurring class changed the design from
shared host-snapshot composition to the separate daemon-owned prompt channel.

The same syntactic capture class exists in the pre-existing v1 instruction
bundle, where an exact-base repository instruction can consume later blocks.
That merged contract surface is outside #680 and is tracked by follow-up #713.

The second automated pass restated the documented model-authority residual; the
owner accepted that residual rather than expanding this unit into a new
system/developer-instruction or mechanical policy channel. It also found two
precision defects. The trust rule could read as prohibiting persistence even
when reconstruction correctly re-runs the current-state gate, so the unsafe
path now names only treating the persisted bit as authoritative without that
re-gate. Adding the v3 rubric also pushed a formerly launchable range of exact
verification-evidence lists over the ward's 31-KiB prompt bound. The task prompt
now remains a distinct Codex positional argument but travels as a dedicated
`sh -c` positional rather than expanding inside the shell program; the ward can
therefore admit up to 120 KiB while retaining margin below Linux's per-argument
limit and preserving the complete evidence JSON.

Revisit when the Wave 6 automatic remediation loop changes the cost of a false
positive, or when the harness offers a mechanically enforced review-policy
channel.
