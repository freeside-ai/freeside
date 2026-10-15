# Agent-Setup Sync: The Landing Check Repeats CI When the Base Moves

The owner requested a sync of the managed AGENTS.md blocks and
`docs/agent-workflow.md` to the agent-setup skill's current canonical text
(freeasinbird/free-skills `fd81184`). Only the `pull-requests` block and its
§handing-off step changed. The sync changes one integration gate, so it
carries a note under the 2026-10-02 sync note's finding
(`devlog/2026-10-02-1016-agent-setup-sync.md`).

## Owner Decision: Adopt the Canonical Text Verbatim

The "repeat integration checks when the base moves" rule now says the landing
check reruns CI when the base moves:

- **CI is no longer repeated by hand.** With a required check that must pass
  on a branch updated to the tip, a refresh push reruns CI. This relaxes a
  gate: the old text had the agent rerun CI, final diff review, and readiness
  for every base commit.
- **Review still follows each refresh push.** A refresh restarts the review
  watch, and Codex's pass or a self-review reads the refreshed diff. A clean
  rebase can still change the diff.
- **A conflicted rebase gets a `git range-diff`.** The resolution is recorded
  in the PR's Verification section. This tightens a gate.

Without a landing check, the canonical text keeps the manual repetition.

## Finding: The Landing Check Was Not Enforced

The update audit found branch protection on `main` with `strict: true` but no
required check. #2017 built the `required` check and left naming it in
protection as an owner-run step, and the step had not been run. GitHub's
up-to-date rule has no effect until a check is named. So the new text, and
AGENTS.md's Build, Test, Run claim that protection requires `required`, were
both untrue. The owner chose to apply the setting during this sync:
`required_status_checks.checks` is now `required`, bound to the
`github-actions` app, with `strict: true`.

## Owner Decision: Add the Forge Record

The 2026-10-02 note recorded the owner declining a forge record. The owner
now accepts it. Nothing technical changed: `origin` still uses the SSH alias
`bnw.github.com`, and `gh` still resolves `freeside-ai/freeside`. The record
states the host and slug for every clone and every skill that reads it,
instead of each one re-deriving them from the alias.

## Rejected Options

- **Keep the manual-repeat text by diverging from the skill.** That means
  permanent drift in a managed block, or opting the block out.
- **Sync the text and leave protection unset.** The synced text would then
  describe a landing check this repository doesn't enforce, and agents would
  skip the manual repeat that still applies.

## Revisit When

A PR merges red or behind `main`, or `required` stops reporting on some PRs
(for example after a workflow rename), which leaves the required context
unsatisfiable and blocks every merge.
