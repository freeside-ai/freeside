# Wave Planning Becomes a Project Skill

Wave-planning sessions (Wave 2, Wave 4) repeated an invariant protocol
with hand-derived wave content, each generated as a one-off prompt. The
repeated shape is real (waves 5–8 remain, plus Phase 2 planning), so the
protocol is now a project skill: `.claude/skills/plan-wave/SKILL.md`,
invoked as `/plan-wave <N>` by the spine session itself.

## Decision

Encode the invariant protocol and the derivation method; never the wave
content. The skill instructs the session to derive each wave from the
docs/plan.md §11 coordination table, its cited sections, and the current
§13 revision at run time. Hard-coded unit lists would rot on the next
plan revision (revision 26 restructured the entire wave table), while the
protocol (preconditions, tracking issue, deferral sweep, decomposition
rules, needs-human filing, devlog) is stable across waves.

The skill is session protocol, not policy: AGENTS.md,
docs/coordination.md, and docs/plan.md stay authoritative. Review
hardening did fold in restatements of binding rules (the
scheduling-field coupling, the `needs-human` exception, lane
derivation, the contract-surface list), so the skill can drift and
carries a synchronization obligation: a change to those rules in
AGENTS.md or docs/coordination.md must be mirrored here.

## Rejected Alternatives

- **A prompt-generator skill** (produce a pasteable wave-N prompt):
  duplicates the derivation. Whoever generates the prompt reads §11 and
  the revisions to get the unit list right; the planning session must
  re-verify everything anyway because a generated prompt goes stale the
  moment the plan revises. Direct invocation does the derivation once, in
  the session that acts on it.
- **Continuing ad-hoc prompts**: each wave re-derives the protocol by
  hand from the prior wave's prompt, and drift between generations (the
  Wave 4 prompt needed the milestone rule and a scheduling gate the Wave
  2 prompt lacked) shows the skeleton wants one home.

Revisit when: the §11 coordination-table structure changes shape (not
merely content); AGENTS.md or docs/coordination.md change the
scheduling, lane, `needs-human`, or contract-surface rules the skill
restates; or wave planning moves into Freeside itself as an automated
spine role; the skill then follows or retires.
