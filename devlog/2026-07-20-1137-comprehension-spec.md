---
run: manual
stage: comprehension-spec
date: 2026-07-20
branch: docs/comprehension-spec
---

# Comprehension Specified: Layering, Provenance, and Paired Metrics

Material plan change (revision 12 -> 13), issue #194, fiat-assigned.
Declared paths: `docs/` (plan.md; intro.md re-check), `devlog/` (this
note).

## Context

Owner priority from the PR #192 review: agents produce more text than
anyone reads, and unread text pushes the human out of the loop, so
decision quality and the attention metrics depend on comprehension
design, not just routing. §9 carried the theme as two lines ("Present
evidence packets first. Add altitude summaries once plans become
structured artifacts."). #194 asked for a real specification: per-type
presentation requirements, a settled summary-provenance model, and
measurable acceptance criteria.

## Decision

§9 now specifies: a four-layer card ordering (the ask and daemon facts,
a labeled summary, the evidence packet, drill-down); three required
digests (change summaries before the diff, plan altitude, digested
review feedback that never silently drops an unresolved or
low-confidence-classified finding); a per-item-type table of what each
of the ten Phase 1 cards leads with and what layers below; a
provenance split; and metrics paired against correctness. §4 gains one
cross-reference sentence; §12 is unchanged.

### Summary Provenance (the Trust Settlement)

Two content classes, two producers. Objective assertions in the
ask-and-facts layer are daemon-produced card facts (§5.13), which puts
them under §12's existing mechanical false-ready zero tolerance with no
§12 edit. Judgment summaries are `producer_class: agent`, ride
`agent_claims`, render as labeled claims, and never enter
`evidence_snapshot`, so §5.15 is untouched.

The Phase 1 summarizer is the stage agent whose work the card
concerns. Rejected alternatives:

- **Daemon-templated summaries**: summarizing plans, diffs, and
  feedback is judgment, and §5.13 places judgment with agents;
  templates would either lie by omission or degenerate into the raw
  facts the ask-and-facts layer already carries.
- **Verifier-produced summaries**: routing prose through the verifier
  would launder agent-quality text into the `evidence_snapshot` trust
  class, defeating §5.15's point.
- **An independent summarizer invocation now**: still
  `producer_class: agent` under §5.15, so independence buys no
  trust-class upgrade, only resistance to self-serving framing, at
  extra cost and latency. That risk is bounded by composition instead:
  a summary may not assert a verifiable fact except by citing the
  daemon fact or linking the artifact digest it compresses. The
  independent path is kept as the already-reserved briefer (§5.13)
  with an explicit promotion condition: recurring audited
  summary-evidence contradictions move summarization to an independent
  invocation blind to the implementer's rationale.

A summary contradicted by its cited evidence is a named comprehension
defect, found by sampled decision audits and recorded in §8 telemetry;
it is deliberately not a §12 false-ready row, because agent claims are
claims and zero tolerance stays reserved for asserted facts.

### Reinterpretation, Not Reversal

The old "present evidence packets first" is reinterpreted: the short
labeled summary now leads above the evidence packet, and the surviving
rule is that evidence precedes long-form agent text. This is the
deliberate material change the owner asked for, recorded here so a
later reader does not mistake it for drift. The altitude staging
("once plans become structured artifacts") survives as the trigger for
enforcing plan structure rather than prompt-level convention.

### Metrics

Open-to-decision per item type, reversal rate, drill-down rate
(explicitly a health signal, never a target: it is trivially gamed by
hiding detail), and comprehension-defect count. §9 states the pairing
rule: speed counts only alongside correctness, because thin cards
produce fast wrong decisions.

## Verification Findings

`docs/intro.md` re-checked against the merged wording (the
2026-07-19-1011 note's revisit condition): the intro's presentation
paragraphs are not contradicted; each sentence (leads with the ask and
an absorb-in-seconds summary, detail lower; changes summarized, plans
lead with questions, feedback digested; summaries as a trust surface
with producer, uncertainty, and evidence links) now has a normative
§9 counterpart. No intro edit shipped.

## Revisit When ...

- Audit data exists to evaluate the briefer promotion condition
  (recurring summary-evidence contradictions), then decide whether
  summarization moves to an independent invocation.
- The renderable text summary carrier lands (the claim contract today
  carries only labeled artifact references, so §9's summary layer
  cannot render without it; raised by Codex review), then implementing
  units can start. Follow-up: #217.
- Plans become structured artifacts, then plan altitude becomes
  enforced structure per §9's staging.
