# A Profile May Publish Workflow Edits; a Merge Reviews Them

Issue #3124, plan revision 118. Safety policy and a material plan change,
so a mandatory note. Docs only; nothing here is built yet.

Chose to let a trust profile publish candidate automation-control edits as
advisory findings, and under such a profile to treat the owner's merge as
the review of the workflow change it carried, over keeping the block or
adding a pause, because Freeside now develops Freeside and workflow and
check-script edits are ordinary work there.

## Decision

- **`candidate_automation_changes` gains `advisory`.** Under it, an
  automation-control match publishes, surfaced on the ready card and in
  the PR-body advisory section, as a reviewer-instruction edit has since
  revision 42. No block, no waiver. Detection stays a widen-only mandatory
  minimum; the value changes only what a match does. `block` stays the
  default, and only profile review sets the value.
- **The advisory lists jobs the limits can't see.** The profile's `allow_*`
  limits describe pull-request jobs only, so a workflow change's advisory
  also lists each added or altered job on another trigger, with its token
  permissions, secret reads, and identity-token request.
- **A merge accepts the workflow change it carried.** Under `advisory`, the
  daemon accepts the default branch's workflow files with no profile review
  only when every difference is a change a Freeside-published pull request
  carried, its advisory showed, and merged unchanged; nothing else the audit
  records changed; the fresh audit is inside every limit; and no
  workflow's token could have made the merge. Anything else is
  still drift and fails closed. Acceptance leaves the profile's digest
  alone, so runs in flight keep their binding. Until #3140 builds it, every
  merged workflow change still needs a profile review.
- **Only the publisher holds `workflows: write`, and only under
  `advisory`.** GitHub refuses a push that changes a workflow file without
  it. An existing registration gains it only when its owner accepts the
  permission change on GitHub.
- **Repository-specific protection is a profile-review choice, and
  Freeside's profile protects none.** A path the profile protects still
  blocks with no per-run waiver.

Builders: #3125 (the option, the importer severity, the token), #3123
(Freeside's protected-path flags), #3140 (the acceptance and the job list).

## Direction

- **Owner, 2026-11-11 (tracker #3132, #3123):** every kind of development
  done by hand today goes through Freeside with no pause for approval
  beyond the pull request itself, and this revision also covers §11's
  operator-authored sentence.
- **Owner, 2026-11-12, in chat (#3124's body):** the first plan kept a
  profile review after every merged workflow edit. The owner asked for the
  better design whatever it costs to build, had it checked by an
  independent review, and replanned to accept merged changes.

## Rejected

- **Keep the block and edit workflows by hand.** It keeps a hand-run lane
  beside Freeside, which the 2026-11-11 direction removes.
- **A hold that waits for approval before publishing.** A pause beyond the
  pull request, and it would need building first. #3125's non-goals leave
  it to a later issue.
- **A profile review after every merged workflow edit.** Each merge would
  stop publication for every task until the owner re-reviewed, another
  pause beyond the pull request.
- **Accepting any new digest without checking it.** That also accepts a
  direct push or a change no reviewed pull request carried.
- **Pinning repository settings instead of workflow content.** An
  independent review refuted it: a merged job on `push`, `schedule`, or
  `workflow_run` can take a writable token or an identity token and cross
  no limit (`prTriggered` in `daemon/internal/publish/workflow_analysis.go`
  gates every axis but artifact consumers), and the audit reads no
  repository or organization secret. So the content pin stays, and the
  daemon moves it only for a change the owner's merge already reviewed.

## Revises

- Revision 7 item 1: automation-control paths are publish-blocking. Now the
  default, not universal.
- Revision 42: "every other control-plane category stays blocking and
  non-waivable." Automation-control follows the profile; the rest block
  on the paths the profile protects.
- Revisions 92 and 113 ("control-plane paths stay operator-authored",
  [note](2026-11-08-1223-whole-repository-self-management.md)): overturned.
- §5.5's "drift fails closed": gains the one accepted case.

## The Assumption That Changed

Workflow and check-script edits were assumed rare enough to do by hand.
With Freeside developing Freeside they are ordinary work, and a refused
import loses the writer's run.

## What the Owner Accepts

Before merge, under `advisory`:

- **The candidate's workflow runs before anyone reviews it.** It gets
  whatever GitHub gives a same-repository pull-request job that the edited
  workflow asks for: a writable token, any secret not behind a protected
  environment, an identity token. The audit of the base's workflows doesn't
  bound it.
- **Where the branch rules let a token merge, that job can merge its own
  pull request or another.** See Freeside's exposure below.
- **That job can rewrite the pull request's body**, advisory section
  included, if it asks for `pull-requests: write`. So the acceptance
  counts the advisory on the ready card, which no candidate can edit.

After merge:

- **Whoever may merge to the default branch can accept a workflow change**
  that only profile review could accept before.
- **Accepted changes add up** without a fresh review of the whole surface,
  unless the owner asks for one.
- **Neither the audit nor the acceptance covers** a script a workflow runs
  or a remote action named by a mutable ref.

On Freeside, with no repository-specific protection:

- **A candidate that edits a check script is graded by its own edit**, and
  the plan's materiality gate rests on the owner's review of the pull
  request.

## Freeside's Own Exposure

Re-read from the repository's settings on 2026-11-12:

- No repository or organization Actions secret, no variable, no
  environment, no self-hosted runner. Default workflow token read-only, and
  Actions can't approve pull requests. OIDC uses the default subject.
- `main` requires the `required` check, an up-to-date branch, and a pull
  request, with admins included. It requires **zero** approving reviews,
  and there are no rulesets.
- Non-pull-request triggers today: `push` to `main` and one `schedule`
  (`daemon-vuln.yml`); no `issue_comment` workflow.

So an edited workflow on Freeside can gain a writable token and an OIDC
token, and nothing else. The planning assumption that branch protection
requires a review the Actions token can't give is false: with zero
required approvals, a candidate job holding `contents: write` and
`pull-requests: write` can merge its own pull request once `required`
passes, and the candidate's edit decides what `required` runs. It could
merge another open pull request the same way. #3140's fourth condition
keeps a token-made merge from accepting a workflow change, but the merge
itself still lands. The owner should close that before #3125
enables `advisory` on Freeside, for example with a branch rule a
pull-request job's token can't satisfy; that choice is the owner's.

A candidate job can also add the `approved` label, but the lander ignores
it: the label counts only when its actor holds write permission, and
`github-actions[bot]` reads as `none` through the collaborator-permission
API (checked 2026-11-12).

## Revisit When

- A repository secret, an environment, or a self-hosted runner is added to
  a repository whose profile is `advisory`.
- A candidate workflow edit does something in CI the owner would have
  refused.
- The owner merges a workflow change and later finds the advisory didn't
  show what it did. Then a profile limit for privileged jobs on
  non-pull-request triggers is the likely next step.
