# Freeside's Own PRs Have No Automated Reviewer

The owner decided (2026-10-21) to turn off automatic Codex review of
Freeside's pull requests. This note records what the project's records
change to and why, so the decision doesn't live only in chat.

Chose to keep an `## Automated Reviewer` section that says **None** over
deleting the section. `docs/agent-workflow.md` §reviewer-record tells an
agent that observes a reviewer with no record to add one. Old Codex summary
comments stay on existing PRs, and an on-demand `@codex review` can still
post, so an empty slot would invite an agent to restore the record. The
explicit **None** says only the owner re-enables it.

Chose `Reviewer: none` in the landing record over leaving the Codex login.
`land-approved-prs` holds back any PR whose current head has no completed
summary from the recorded reviewer, so a stale login would keep every
approved PR from landing. Deleting the bullet was rejected too: the lander
treats a missing Reviewer as `bad-record` and stops the batch. `none` is
the value it accepts for a project with no reviewer.

Chose a project-level override that skips the handoff review watch over
leaving it to `await-pr-review`. The handoff sequence says to run that skill
whenever it is installed, and the skill, finding no record, scans the last
20 PRs for a bot reviewer. Those PRs are full of Codex reviews, so it would
identify Codex, wait out its cap on every PR, and try to record it again.
The handoff text sits in a managed block, so the override lives in the
unmanaged Automated Reviewer section instead of an edit to managed text.

No other rule changes. The existing fallbacks already cover a project with
no bot reviewer:

- **Before push,** §pre-push-review asks for fresh eyes on any work in a
  repository without an external bot reviewer, so every PR now gets a
  fresh-context review or relies on the owner.
- **After a refresh,** §handing-off says to self-review the refreshed diff
  when no recorded reviewer reads it.
- **In a landing batch,** rework gets a fresh-context review of its
  range-diff, or is handed back to the owner when delegation is unavailable.

Not changed: Codex as a reviewer *inside* Freeside runs. The daemon's
native-review login, the exit-run skill's Codex reviewer identity, and the
fixtures that name `chatgpt-codex-connector` describe product behaviour on
target repositories, not review of this repository.

Cost shifts from a free bot pass to a model-backed review per PR and more
owner reading per PR.

Revisit when automatic review is turned back on for this repository (restore
a full reviewer record, the review watch, and the landing Reviewer and
completion marker), when `await-pr-review` can read an explicit "no
reviewer" record and stop on it, or
when owner review time per merged PR rises enough to make another
automated reviewer worth recording.
