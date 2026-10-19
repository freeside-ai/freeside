# Approved PRs Land By Label, With No Base-Advance Checks

The owner asked (2026-10-19) to set Freeside up for the `land-approved-prs`
skill. The skill reads a landing record from `AGENTS.md` at `main`'s tip,
and until this change Freeside had none, so a batch would stop at
`no-landing-record`. The `approved` label was created on GitHub the same
day, by the owner; `needs-human` already existed.

Chose the label mechanism, the only one the skill lands. A review approval
or armed auto-merge would each need its own contract in the skill first.

Chose `bennelsonweiss` as the sole approver. It is the only human login on
the project. The record is what makes an agent's `approved` label a
violation, since any agent running under that login can add the label.

Chose no base-advance checks. The obvious candidates are already covered
or don't fit a fixed command:

- **`scripts/check.sh` per component** was rejected. Branch protection's
  up-to-date rule already reruns the `required` check on the branch
  updated to the tip, and `land.sh` waits for CI on that exact commit. A
  local rerun in the primary checkout would repeat it, slower, against a
  tree that isn't the PR's.
- **The merge-result audit** was rejected as a record command. It needs the
  PR's head branch and its declared scope as allowed paths, and the scope
  lives in the PR body, not in anything a command can read reliably. The
  lander still runs it per PR when it selects that PR for integration, as
  Integration Ordering and Merge-Result Audit requires.
- **Contract regeneration** is covered by CI's `generate` job on the same
  updated commit.

Kept the skill's default stale lock age of 240 minutes. No batch has run
yet to suggest a different value.

Revisit when the required check stops running on the updated branch (the
up-to-date rule is dropped or a merge queue replaces it), when a second
person can approve, or when the merge-result audit gains a way to read a
PR's scope without a human, which would make it a fit for this record.
