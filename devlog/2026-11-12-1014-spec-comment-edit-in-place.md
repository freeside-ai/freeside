# Post the Specification Comment Early and Edit It in Place

Work unit: #3128. Plan revision: 119.

The specification comment now posts when the daemon accepts and screens a
bound task's specification, under either `gates.spec_approval` setting. Each
task has one comment, which the daemon edits in place as the task moves on,
and the comment is the unit's work claim. This reverses decisions 5 and 12 of
`devlog/2026-09-20-0903-spec-issue-comment.md` (revision 66) and refines its
decision 9. Nothing about the comment is built yet; #1433 through #1439 build
it, and #1738 teaches `trackercollect` the claim. The plan text is in §5.11
(The Specification Comment, renamed from The Approved-Specification
Comment), with supporting edits in §1, §4, §5.5, §5.12, §5.13, §5.17, §9,
§10, §13, and §14, and in `docs/coordination.md` (Claiming and Stages).

The owner gave the direction on 2026-11-11, in the assessment behind tracker
#3132 of what Freeside needs before its own development moves onto it: a bound
issue shows the current specification in one comment that is also the unit's
claim. The settled items below were the issue's recommended answers, which
the owner decides by reviewing the revision's PR.

## What Changed Since Revision 66

Revision 66 assumed the comment was a visibility aid for an approved plan,
read mostly after the fact, so an append-only history cost little and kept
the forge surface to read and create. Two assumptions changed:

- **The owner reads the issue's current plan, not its history.** With
  Freeside developing Freeside, the bound issue is where the owner and
  hand-run sessions look for what is in flight. A stack of successor
  comments makes the reader find the newest one; GitHub's edit history keeps
  the earlier renderings for anyone who wants them.
- **The comment has to be a claim.** Hand-run sessions and `trackercollect`
  need to see that Freeside holds the issue as soon as it starts work, and a
  claim needs one comment whose marker the daemon can later remove. That
  needs an edit call, which revision 66 had avoided.

Posting only on approval also left a task under `gates.spec_approval: false`
with no comment and so no claim, which the claim can't tolerate.

## Decisions

1. **Post when the specification is produced, under either gate setting.**
   Chose the acceptance of the specification as the trigger over a human
   approval. The screen already runs there (`acceptSpecification` in
   `daemon/internal/engine/specification.go` runs for both settings), so the
   text that posts is the text the card would show. The cost is that
   screened agent text reaches a public issue before a person reads it, and
   with the gate off a person may never read it first; the §14 row records
   that the screen, not a reader, is the check. Reverses decision 12.
2. **One comment per task, edited in place.** Chose edits over appended
   successors. Each later state (a revised specification, an approval, a
   release) replaces the comment's text. The edit target is the comment ID
   the daemon stored from its own create response, and before each edit the
   daemon reads that comment and proceeds only when the App authored it and
   it sits on the target issue. Reverses decision 5.
3. **The frame states the comment's state.** The frame always carries the
   task ID, the specification digest, the public plan digest or its absence,
   the claim, and the statement that the comment grants nothing. Its notice
   names one of four states: produced and waiting for approval, approved,
   started without approval because policy turns the gate off, or released
   because the task ended, naming its merged pull request when it has one.
   A released comment drops the public plan, the claim marker, and the
   `Claim:` line, and keeps the rest of the frame.
4. **A refused revision shows the frame alone.** When a revision's public
   plan fails the screen, or the dispatch-time re-screen refuses, the edit
   shows the frame alone naming the new specification digest. The issue never
   shows a refused plan as the one in force. Credential-shaped content still
   fails the specification itself. An edit that is refused or hasn't landed
   still leaves the earlier rendering; that lag is a named residual, not a
   guarantee the frame can make.
5. **The create is keyed by the task; each edit by its occurrence.** The
   create's identity is the task ID. An edit's identity is the task ID plus
   the occurrence that produced the state: the accepted specification's
   attempt, the resolved `spec_approval` item, the terminal transition, or
   reconciliation's observation that the task's pull request closed.
   Chose occurrence over content for edits for decision 9's reason: with a
   content key, approving A, then B, then A again would not edit the third
   time, and the comment would name B while A is in force. Refines decision 9.
6. **Edits are safe to repeat; the create keeps its recovery rules.** An edit
   sends the full rendering of its state, so a lost response or crash retries
   it. Calls serialize per issue, an older state's edit is superseded rather
   than sent once a newer state of the same task has an intent (another
   task's effects never supersede it), and after a timed-out call
   the next edit waits the settle interval. The create keeps revision 66's
   check-before-create, pre-dispatch ID set, and residual-ambiguity rules.
7. **One create per task.** A create that ends with no comment (refused,
   fenced by cancellation, or ambiguous) leaves the task with no comment the
   daemon can edit, and its later states post nothing. Chose this over letting
   a later state retry the create, which would need the create's evidence
   rules at every state for a visibility aid. An ambiguous create may still
   have posted a comment and its claim, which the daemon can never release;
   the `system_health` item says so, and a person deletes the comment.
8. **The `Claim:` line names the task, not a branch.** The issue's draft said
   the claim names "the branch Freeside will publish." The daemon can't know
   that branch when the specification is produced: a label-intake task's
   branch is `freeside/publish/<16 hex>` from the publication identity
   (`publish.resolveBranch`), and that identity hashes the candidate's head
   SHA and artifact digests (`daemon/internal/publish/identity.go`,
   `daemon/internal/publicationrecord/record.go`), which exist only once a
   candidate is ready to publish. So the line is
   `Claim: freeside-task-<task-id>`.
9. **A daemon claim has no lease and ends by an edit.** It has no 48-hour
   lease and no branch handoff, because a task can run for days before it
   publishes. It stands until the daemon edits the comment to remove the
   marker or a person deletes the comment. The daemon releases it when the
   task is terminal with no open pull request of its own for the issue, and
   when reconciliation later finds a terminal task's open pull request
   closed. This departs from the issue's recommendation, which kept the claim
   after a merged pull request until the issue closed: a merge that declined
   closure would then leave a finished task's claim on an open issue, and
   reopening a closed issue would bring the claim back. The daemon never
   reads claims and won't stop for one, so a daemon claim wins arbitration
   over any hand-run claim whatever their order; the hand-run session
   releases, stops, and reports the overlap to the owner.
10. **Claimant rule (#1738).** A claim, release, or reservation marker counts
    when its author is the repository's owner, a member, or a collaborator,
    or the bot account of the project's own Freeside App, identified by its
    numeric account ID. The App's marker counts only as a daemon claim (a
    `Claim:` line naming a `freeside-task-` task), so a marker in any other
    App-authored comment, such as the comments #3147 and #3151 add, claims
    nothing. That narrows the issue's recommendation, which would have
    counted any marker the App wrote. Any other author's marker is ignored
    and reported.
    GitHub reports an App's comments with an author association outside
    `OWNER`, `MEMBER`, and `COLLABORATOR`, so the App needs its own rule. This
    is the owner's direction that the answer must admit the App; #1738 now
    implements it rather than deciding it.
11. **The comment is not an implementation plan.** `Handle #N, implementation
    plan in comments` does not accept it. It is screened agent text written
    for issue readers and leaves out the research, owner answers, and
    implementation detail a hand-run plan carries. A session that handles the
    issue reads it as context.
12. **A deleted or uneditable comment is a definite refusal and is never
    re-created.** A person deleting the comment made a decision the daemon
    shouldn't undo, and the claim goes with the comment. A refused release
    (for example, workflow drift) leaves the claim standing until the issue
    closes or a person deletes the comment.
13. **Cancellation never fences the release edit.** It still fences an
    undispatched create. A cancelled task whose comment posted, including one
    create recovery adopts after the cancellation, still gets its release.
14. **The narrowed token gains one edit.** It still holds `issues: write`
    and `metadata: read`. It now also reads and edits the one comment whose ID
    the daemon stored. `issues: write` already allowed editing any comment, so
    the grant doesn't widen; only the publisher's own code keeps the edit to
    the stored comment. Each edit is another `issue_comment` event with the
    `edited` action, which the existing trigger-based audit already covers,
    and the dispatch-time workflow recheck runs before each edit as before
    each create. Revision 66's fan-out bound of one comment per approval
    becomes one comment per task plus one edit per state change.

## Rejected Options

- **Keep approval as the trigger and append successors** (revision 66). An
  issue with a revised plan shows several comments, a task under the gate-off
  policy shows none, and no comment can carry a claim that the daemon later
  releases.
- **Name a branch in the claim.** The branch doesn't exist until publication;
  see decision 8.
- **Re-create a deleted comment.** Overrides a person's decision and would
  need a second create's evidence rules.
- **Let a refused or re-screened revision leave the earlier plan.** The issue
  would show a plan no longer in force.
- **Accept the comment as a `Handle` session's plan.** It lacks what a plan
  carries, and §5.17's zero-authority rule for markers in issue content holds.

## Revisit When

- A task-scoped branch is bound at intake, or `publish.resolveBranch` or
  `publicationrecord.BranchName` changes, so a branch exists when the
  specification does. The `Claim:` line could then name it.
- Edits that don't land, or a task whose first create was refused and later
  states post nothing (decision 7), leave bound issues without a claim often
  enough that hand-run sessions collide with daemon tasks.
- GitHub notifies subscribers on comment edits, or offers a permission
  narrower than `issues: write` for creating and editing comments.
