# Agent-Setup Sync: Review Gates Follow the Skill's Text

The owner requested a sync of the managed AGENTS.md blocks and
`docs/agent-workflow.md` to the agent-setup skill's current canonical text.
The sync is verbatim, and it changes two review rules.

## Owner Decision: Adopt the Canonical Text Verbatim

Chose the skill's current text over this repository's older copy. Two review
gates change with it:

- **Pre-push fresh-eyes review is keyed to risk.** It is now triggered by a
  destructive path, a credential-leak surface, a returned-object trust
  boundary, a contract or public-interface change, a behavior change without
  tests, or the absence of a bot reviewer. It used to be triggered by any
  non-trivial work. A large mechanical change relies on CI and Codex. This
  relaxes a gate.
- **Handoff waits for the last push's review.** Handoff now waits for the
  review the one allowed last push triggers, or its bounded timeout, and a
  blocker there reopens fix rounds. It used to hand off without waiting. This
  tightens a gate.

Why: the managed blocks and `docs/agent-workflow.md` are owned by the skill,
and the comparator treats any divergence as drift, so the project's review
policy follows the skill's. Diff size was a poor proxy for risk. The named
classes match the project's existing refute-first and risky-diff classes, and
Codex reviews every push here.

## Rejected Options

- **Keep the "non-trivial" trigger by diverging from the skill.** That means
  permanent drift in a managed block, or opting the block out of management.
- **Add a stricter Freeside-specific rule in unmanaged text.** There is no
  evidence yet that the risk-keyed trigger misses anything here.

## Owner Decision: No Forge Record

This agent-setup version offers a forge record because `origin` uses the SSH
alias `bnw.github.com`. The owner declined it. `gh` resolves
`freeside-ai/freeside` correctly today, and the offer recurs on the next
update run.

## Finding: A Sync That Changes a Gate Needs a Note

Codex flagged that a sync which changes a review gate is a safety-policy
change on the mandatory-note list, even when the text is upstream's. Treat
future syncs the same way: a sync that changes a gate carries a note, and a
wording-only sync does not.

## Revisit When

A defect reaches `main` through a large mechanical change that skipped
pre-push review, or the project loses its bot reviewer.
