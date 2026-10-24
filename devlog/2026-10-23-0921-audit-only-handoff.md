# Hand Off Behind the Tip: Audit Before Handoff, Rebase Only When Needed

The owner observed (2026-10-23) that agents still rebase their PRs onto
`main`'s tip before handoff, and again when the tip advances during the
handoff wait, although `land-approved-prs` now lands most PRs. The owner
decided the pre-handoff rebase should go: an agent checks the PR against
the current tip with the merge-result audit and rebases only when the
audit finds a conflict or a landed change needs a fix in the PR.

## Finding: The Rules Asked for the Rebase

The agents were following the written rules, not drifting from them.
`docs/agent-workflow.md` §handing-off step 3 told every author to refresh
onto the latest tip at handoff, and AGENTS.md's Build, Test, Run section
said "Refresh a PR onto the current tip before handoff". Step 3 already
left a base advance before approval to the lander, but the AGENTS.md line
read as a precondition of handoff, so an agent whose CI wait outlasted a
landing rebased again.

## Decision: The Landing Check Decides Whether the Author Refreshes

Chose audit-only handoff over refreshing at handoff, for projects whose
landing check is a merge queue or land command, because the refresh buys
nothing there:

- **The lander refreshes anyway.** `land.sh` rebases a head that doesn't
  contain the tip, pushes it, and stops; the next `land` run waits for CI
  on that exact commit before it merges.
- **The lander's reading doesn't shrink.** `landed-since.sh` reads every
  landing since the PR's original fork point, across every head a force
  push replaced. An author's rebase moves the branch's base but not that
  fork point, so the lander reads the same set either way.
- **The refresh costs something.** Each one is another CI run, a restarted
  review watch and self-review, and another force-pushed head for
  `landed-since.sh` to trace. If the forge drops a replaced head, the
  lander stops `original-base-unknown` and hands the PR back.

What still runs before handoff is the check, not the rebase. The
merge-result audit builds the prospective merge with `git merge-tree`, so
it detects a conflict and an out-of-scope change against the fresh tip
without touching the branch. The handoff names the tip it checked.

Projects with only GitHub's up-to-date rule, or no landing check, keep the
author refresh: nobody else pushes the update there.

## Tradeoff Accepted

A PR that merges cleanly in text but breaks on the tip (a semantic
conflict) with a change that landed after its last push now surfaces at
landing, where the lander's CI run on the rebased head catches it. The
lander reworks it when the landed change dictates the fix and otherwise
hands it back with `needs-human`. The window is small: `pull-request.yml`
runs on the `pull_request` event, whose default checkout is the merge
ref, so the author's CI already tests the PR merged with the tip as of
each push. A pre-handoff rebase only moved that tip forward to handoff
time.

The audit can't show that a landed change needs a fix in the PR; it
prints the PR's own change set. In practice the author refreshes on a
conflict, and on a contract PR whose generated consumers must be
regenerated against a landed contract unit.

## Owner Decision: Canonical Text Changes Upstream First

`docs/agent-workflow.md` §handing-off is managed text, synced from
agent-setup's canonical scaffold. The owner chose to change it upstream
first (freeasinbird/free-skills#347) and sync it here, over editing the
managed copy in this PR ahead of the canonical change. The canonical
change landed upstream, and this PR carries its sync beside Freeside's
own text, so the two merge together. Neither half can merge alone:
AGENTS.md's summary says to hand off behind the tip, while the earlier
step 3 said to refresh.

## Rejected Options

- **Edit only AGENTS.md and merge it alone.** Step 3 would still tell
  agents to rebase, and its procedure overrides a one-line summary.
- **Edit the managed copy here ahead of upstream.** It drifts from
  agent-setup until upstream catches up, and an upstream rewording would
  leave Freeside on text that never became canonical.

## Revisit When

The lander stops refreshing or reading from the original fork point, the
project drops the land command for the up-to-date rule alone, or
hand-backs for semantic conflicts become common enough that the earlier
catch would save owner time.
