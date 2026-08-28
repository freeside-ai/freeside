# 0002: Publish Reviewer-Instruction Edits as Advisory Findings

- Status: Accepted
- Date: 2026-08-28
- Decider: Ben Nelson-Weiss
- Supersedes: revision 4, "Reviewer-instruction poisoning closed"
  (`docs/history/decisions.md`); recorded as revision 42 in `docs/plan.md`
  §13
- Source note: `devlog/2026-08-28-1130-advisory-instruction-paths.md`

## Context

Revision 4 made every candidate change to a reviewer-instruction path
(`AGENTS.md` at any depth, `AGENTS.override.md`, `.codex/**`, and peers)
publish-blocking and non-waivable, because an automatic review is not
independent when its PR edits the instructions governing it. The wave-6 exit
run's first real work item needed an `AGENTS.md` edit and could not publish
through any configuration or human decision.

Since then the mechanism the block guarded has been built: the implementing
agent and the Freeside-invoked reviewer compose their instruction bundles from
the exact trusted base (plan §5.8, §7; #709, #713). A candidate's edited copy
is diff content and never launch authority.

## Decision

Keep the mandatory, widen-only detection. Lift a reviewer-instruction finding
with a third disposition, `advisory`: it never blocks publication, never
carries a waiver, and the publisher surfaces it in a PR-body section that
candidate prose cannot forge. The human merge gate, reading that section, is
the judgment point for an edit that will govern later runs. Every other
control-plane category stays blocking and non-waivable; the domain predicate
`ControlPlaneCategory.Advisory` names the advisory set so a new category must
choose.

## Consequences

- Routine instruction maintenance (invariants, agent guidance, skills) can
  flow through the loop instead of an operator-authored PR.
- The independence guarantee now rests on base-pinned instruction
  composition, not on a path block. If composition ever reads from the
  candidate head, the block must return.
- A human waiver flow remains unbuilt; the rejected options and revisit
  conditions live in the source note.
