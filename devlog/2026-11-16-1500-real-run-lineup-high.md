# Real Runs Default to Opus 5.5 at High With a Sol 6.1 Reviewer

Owner decision of 2026-11-16, made while preparing #2382's live run on
Freeside's own repository, and applied to every real run. It replaces the
default in `devlog/2026-11-10-1639-exit-run-real-lineup.md` and the
escalated specifier's effort in
`devlog/2026-11-10-1758-escalated-specifier-default.md`. The rule lives in
docs/production-walkthrough.md, Choose The Real Run's Writers.

## Decisions

- **Writers: `claude-opus-5-5` at `high`** for the specifier,
  implementer, and remediator, over the 2026-11-10 default of `medium`.
  The three roles stay on one model and effort, so they share one agent.
- **Escalated specifier: `claude-fable-5-1` at `high`,** up from `medium`
  with the writers.
- **Judgment: `claude-opus-5-5` at `medium`,** unchanged.
- **Reviewer: `gpt-6.1-sol` at `high` on the Codex harness,** now part of
  the default instead of "review stays with onboarding", which left each
  project with whatever its onboarding happened to pick. The review model
  still binds into the review configuration digest a project's onboarding
  approves, so the default reaches a project only through a fresh
  onboarding review; a run never changes it on its own.

The owner stated the lineup without reasons. The 2026-11-10 note rejected
`high` for its spend on every run; the owner now accepts that spend for
real runs, which since #2382 include Freeside's own, mostly Tier A, code.
The fixture pass keeps its own cheaper table: it proves the workflow, not
the writers.

Revisit when a newer model ships, or when a real run shows `high` too
costly or the reviewer too weak for its target.
