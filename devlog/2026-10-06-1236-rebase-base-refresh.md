# Rebase to Refresh a Pull-Request Branch

The owner decided on 2026-10-06 that a pull-request branch is refreshed by
rebasing it onto its base, never by merging the base into it. The decision
came in review of PR #1789, whose branch carried a `Merge remote-tracking
branch 'origin/main'` commit. AGENTS.md now records the method (Integration
Ordering and Merge-Result Audit), and `scripts/check-commit-messages.sh`
rejects a merge of the base in a pull request's commit range.

## What Changed

- **The method was unrecorded.** `docs/agent-workflow.md` §handing-off says
  to update the branch "with the project's merge or rebase method", and no
  project record named one. Sessions inferred merge from history: several
  merged branches carry a merge of `origin/main`.
- **The tooling treated a base merge as ordinary.** The range check skipped
  merge commits so that a base-freshness merge would pass, its suite pinned
  that case, and `2026-09-02-1130-agents-md-residency.md` calls the merge
  "ordinary" where it explains why the commit hook exempts a merge in
  progress. No owner decision chose merge; the exemption assumed it. This
  note replaces that assumption.

## Decisions

- **Chose rebase over a base merge (owner).** The reasons below are the
  agent's reading, not the owner's words. A base merge puts a commit that is
  not part of the work unit inside the unit's history, and it holds conflict
  resolutions outside the commit that owns the conflicting change. PR #1789
  showed the cost: folding a review fix into an earlier commit meant
  rebuilding the merge by hand and proving the resolution survived.
- **Chose a mechanical check over the prose rule alone (agent; the owner can
  veto).** The prose gap produced the same wrong choice in several sessions,
  and the range check runs on every pull request already.
- **Chose a rule in the existing range check over a new script and
  workflow.** The check already decides which commits of a pull request it
  reads, and it is the one check that sees the exact range. The rule is the
  only one that reads history and not a message; the header says so.
- **Chose to reject only a merge with a parent the base already contains,
  over rejecting every merge commit.** A merge of two lines of work the base
  lacks is not a base refresh, and the suite already pinned that such a
  merge is exempt.
- **Left the commit hook's merge exemption as it is.** The hook runs before
  the commit exists and is not told the pull request's base, so it cannot
  tell a base merge from another merge. Pull-request CI is the authority.
- **Left `docs/agent-workflow.md` generic.** Its step defers to the
  project's method, and AGENTS.md is now where that method is recorded.

## Limits

- **A base brought in through a side branch passes.** When a branch merges a
  side branch that sits on a newer base tip and carries commits of its own,
  no parent of that merge is contained in the base, so the rule does not
  fire.
- **A base SHA older than the merged tip changes what is reported.** The
  range then holds the mainline history the merge brought in. The check
  still fails, on the first mainline merge commit in that history, because
  `main` advances only by merge commits. It names that commit and not the
  branch's own merge.
- **The check runs on pull requests only.** A local base merge is caught
  when the branch is pushed to a pull request, or by running the script.

## Revisit When

- The forge or a host tool refreshes branches in a way the project cannot
  turn off and that only merges. The rule then needs an exception or the
  tool needs replacing.
- A work unit needs a base merge for a reason rebasing cannot serve, such as
  a branch several people push to. The owner decides the exception.
