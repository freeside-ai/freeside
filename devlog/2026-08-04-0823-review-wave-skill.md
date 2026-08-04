# The Wave Exit Review Gets a Prompt-Generator Skill

The §11 wave exit review has been prompted ad hoc: a hand-derived
per-wave prompt (Wave 1's is the surviving template) regenerated from
chat history each wave, with the derivation (units, merged PRs, cited
sections, hunt targets) redone by hand. Waves 5–8 remain, so the
recurring shape is real. The protocol is now a project skill:
`.claude/skills/review-wave/SKILL.md`, invoked as `/review-wave <N>`,
whose deliverable is one self-contained reviewer prompt.

## Decision

The skill produces the prompt; it never executes the review (owner
decision, 2026-08-04). Three reasons:

- **Independence is the review's load-bearing property.** Plan §11
  requires a reviewer with only the repository and its documents. The
  implementing sessions are typically Claude, so the executing
  reviewer is typically Codex for vendor diversity; a session that
  derived the prompt from spine context must not also execute it.
- **The variable part is derivation, not execution.** What changes per
  wave (tracking issue, unit→PR mapping, cited sections, hunt
  targets) is mechanical spine work; the hunting itself is the part
  deliberately handed to fresh eyes.
- **A prompt is vendor-neutral.** It pastes into any agent; harness
  skill formats are not symmetric across vendors.

This inverts the plan-wave rejection of a prompt-generator
(`devlog/2026-08-02-2007-plan-wave-skill.md`): there the planning
session acts on its own derivation, so generating a prompt duplicates
work and goes stale against plan revisions. Here the derivation and
the action are different vendors' sessions by design, and the prompt
is consumed once, immediately after generation, so staleness has no
window.

## Rejected Alternatives

- **Vendor-specific review-executing skills** (a Codex skill plus a
  Claude skill that run the review directly): duplicates the same
  instructions across asymmetric harness formats, invites drift
  between the copies, and removes the human hand-off that keeps the
  reviewer invocation independent of the implementing session.
- **Continuing ad-hoc prompts**: each wave re-derives skeleton and
  content from a prior wave's chat transcript; the Wave 4 derivation
  showed the tracker checklist alone mis-scopes the review (a unit
  recorded as deferred had in fact merged), a trap a codified
  derivation step now closes.

Revisit when: the §11 exit-review requirement changes shape; AGENTS.md
or docs/coordination.md change the finding-filing, lane, or
remediation-proposal rules the skill restates (synchronization
obligation); or the exit review moves into Freeside itself as a
runtime review pass, at which point the skill follows or retires.
