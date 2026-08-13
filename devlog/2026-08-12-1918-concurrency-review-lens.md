# Concurrency/Lifecycle Review Lens: Home in the Wave-Review Prompt

Item 6 of #736 (the ranked daemon verification-tooling adoption).

## Decision

Chose the wave-review prompt (`.claude/skills/review-wave/SKILL.md`
Hunt-targets list) as the sole home for the concurrency/lifecycle
review lens, over the AGENTS.md finish-line refute-first list the issue
also names. #736 item 6 offers "the refute-first list and/or the
wave-review prompt"; the "and/or" is resolved by where the text can
durably live.

The finish-line refute-first paragraph is inside the
`agents-md:managed:finish-line` block, which is synced verbatim from
the agent-setup skill's canonical cross-project sections (diffed
2026-08-12: Freeside's block is byte-identical to
`references/canonical-sections.md`). A Freeside-specific fourth risk
class added there would be overwritten by the next managed-section sync
and would inject project-specific content into text shared across every
agent-setup project. AGENTS.md's own "Record a noticed automated
reviewer" guidance already states project-specific additions go
*outside* `agents-md:managed:*` blocks for exactly this reason.

The wave-review prompt is unmanaged, project-specific, and is the
adversarial review pass the issue's motivation calls out ("no review
pass examines it separately from ordinary code quality"). The lens sits
beside the existing risk-class lenses, applied "where the derivation
shows they apply," and is marked as carrying **no** disposition-record
requirement: unlike the trust-boundary risk classes, concurrency
findings are ordinary correctness findings that flow through the normal
findings-summary and issue-filing path.

## Rejected Alternatives

- **Add the lens to the AGENTS.md finish-line refute-first list.**
  Rejected: managed/synced block (see above). This is the alternative
  the issue text names first, so it is the likely instinct of a future
  session; the wave-review prompt is the correct home instead.
- **Add a new unmanaged AGENTS.md subsection for a per-PR concurrency
  lens too.** Rejected as scope inflation for the lowest-ranked,
  docs-only item: it would duplicate the lens text and create a second
  source that drifts. The wave-review pass satisfies the acceptance
  criterion ("enumerated where sessions will actually hit it") on its
  own.

## Revisit When

The concurrency/lifecycle lens proves it needs a per-PR (not only
per-wave) trigger, i.e. concurrency defects land between wave reviews
often enough to matter. If so, promote it to a general risk class in
the agent-setup canonical finish-line section (a cross-project change
to the skill, not a Freeside-local managed-block edit).
