# Post the Approved Specification to the Bound Issue

Work unit: #1415. Plan revision: 66.

The daemon posts the approved public plan as one comment on a label-intake
task's bound issue, once a human approves the specification. Until this
revision the approved specification lived only in the daemon's store, and the
published PR body describes the implemented outcome, not the approved plan. This
unit edits `docs/plan.md` only. The plan text is in §5.11 (The
Approved-Specification Comment), with supporting edits in §1, §4, §5.5, §5.12,
§5.13, §5.17, §9, §10, §13, and §14.

This is a new GitHub write effect driven by agent-written text, so it is on the
mandatory-note list as a safety-policy change and a material plan decision.

The positions below are the issue's recommendations, refined by the implementing
session against the code at `be74208e`. The owner decides them by reviewing the
revision's PR.

## Decisions

1. **What text posts: a new specifier field, not the summary or the body.**
   Chose an optional public plan (at most 8 KiB) in the specifier's output over
   posting `Specification.Summary` or `Body`. The summary and body address the
   owner and the implementer and may quote daemon-fetched research and owner
   answers, and a body at its 64 KiB bound fills a GitHub comment by itself. All three
   texts are normal sensitivity; sensitivity grants nothing, so it did not decide
   this.
2. **The approval must bind the public plan separately.** The approved
   specification digest is the digest of the body bytes alone
   (`acceptSpecification`), so a field outside the body is not approval-bound by
   default. A decision's binding set is exactly the digests its card renders in
   `evidence_snapshot` and `agent_claims`. So for a bound task the public plan
   is its own digest-addressed artifact, carried on the `spec_approval` card as
   a labeled claim the way the summary already is; an unbound task discards the
   field. The approval binds the plan's source bytes, and the comment is the
   daemon's deterministic rendering of them. No new binding mechanism is
   needed. The card's primary layer must also say that approving posts to the
   issue and show what will post, because a claim in the collapsed detail layer
   can be approved unread. Whether that fact needs an API or client change is
   for the decomposed card unit to establish.
3. **Screen at acceptance, so the card never shows text that will not post.**
   Chose to run the screen when the daemon accepts the specification over
   screening only at post time. A refused public plan never becomes a claim, and
   the card says that only the daemon's notice will post. A credential-shaped
   public plan fails the specification, as the summary and body do today. The
   screen runs again before dispatch under the recorded ruleset version; a
   refusal there, or a withdrawn ruleset version, posts the frame alone.
4. **A refused or absent public plan posts the frame alone.** This departs from
   the issue's recommendation of no comment on a screen failure. The
   daemon-written frame (notice, task ID, digests) holds no agent text, so a
   screen failure does not make it unsafe, and the issue still gets tied to the
   task and the approved digest. It also keeps the effect uniform: every approval
   on a bound task is one comment.
5. **A revised specification appends a successor comment.** Chose a successor
   that names the digest it supersedes over editing one comment in place. The
   issue history stays append-only, the daemon never edits or deletes what it
   posted, and the forge surface shrinks to read and create.
6. **A failed post never blocks the run.** The comment is a visibility effect,
   not a gate. A transient rejection GitHub made before creating anything (a
   rate limit), or an attempt that never dispatched, retries within §5.9; a
   definite refusal (missing `issues: write`, a gone or locked
   target, an unapproved profile) ends quietly
   with a reason class; only residual ambiguity raises a `system_health` item,
   and recording that ambiguity records a terminal `ambiguous` outcome, so no
   attempt follows and there is no blind retry. Acknowledge keeps its §4
   meaning (seen, never resolved) and changes nothing about the effect; an
   acknowledge that closed the effect would have overloaded it. Each attempt
   records a dispatch-started marker before the create request. Every other
   dispatched attempt is unproven: a 5xx response, an unclassifiable or
   truncated response, a timeout, or a lost response. With no candidate after
   the settle interval, it is residual ambiguity, never a retry.
7. **The comment is a visibility aid, not a durable external record.** A record
   would need to be complete and tamper-evident. A comment is editable by
   repository admins, the daemon never reads it back, and completeness would
   push toward posting the body. So the comment carries the task ID and the two
   digests, enough to match it to the PR and the daemon's store, and no further
   lineage.
8. **The record reuses the outbox and inbox ledger.** Chose new intent and
   outcome kinds, as `publicationrecord` does, over a dedicated table. That
   avoids migration 0077 and its three migration-subset exclusion lists. The
   boundary that rebuilds the record re-derives the target from the task's
   current intake binding and never trusts a decoded target, approval bit, or
   comment ID.
9. **The effect identity is the approval occurrence, not the content.** Task ID
   plus the resolved `spec_approval` item, which is deterministic across replay.
   Chose this over the planning recommendation of task ID plus digest. With a
   content identity, approving A, then B, then A again posts nothing the third
   time, and the issue's newest comment names B while A is in force. It also
   follows §5.13's rule that content never defines occurrence identity. The
   cost: approving unchanged bytes twice posts a successor naming the same
   digests, which is accurate and rare.
10. **Fan-out is named and bounded, not ignored.** An App-authored issue comment
    starts `issue_comment` workflows, which run from the default branch with
    repository secrets and can read the comment body. `docs/plan.md` said
    nothing about them. The §5.5 workflow audit now enumerates them under
    `allow_issue_comment_workflows` (default false), and the key gates only this
    comment, never publication. Those workflows run from the default branch as
    it stands at post time, and the human accepted their contents, not their
    names. So the audited set records a digest for each listed workflow, over
    the workflow file and the local reusable workflows and composite actions it
    uses, as part of the digest-bound profile. The daemon re-enumerates and
    re-digests them at the current tip immediately before each post and refuses
    on drift, as §5.17 revalidates its profile before each creation. Clearing
    drift needs the owner's re-review. A profile reviewed before revision 66
    does not approve the operation. Named residuals: remote actions and reusable
    workflows named by a mutable ref, scripts a listed workflow runs from the
    checkout, workflows chained by `workflow_run`, webhook-driven Apps,
    including ones keyed on plain phrases, and stale-issue timers that any
    comment resets.
11. **The comment ruleset rejects mentions and command shapes outright.**
    `github/1` rejects closing directives, CI-skip directives, and trailers, but
    passes `@name` tokens and `/command` lines. Recipe v2 neutralizes mentions
    at render time, which does nothing against a webhook bot that reads the raw
    body. So `github-issue-comment/1` rejects them. The cost is a false refusal
    on a plan that names a decorator such as `@property`, or that opens a line
    with a path such as `/usr/local`; that falls back to the frame alone, which
    is cheap.
12. **No comment when the `spec_approval` gate is off.** Approval is the
    trigger. With the gate off, agent text drawn from research would reach a
    public issue before any human had seen it.
13. **The narrowed token keeps `metadata: read`, as the existing narrowed sets
    do.** `issues: write` is wider than the operation: it also labels, closes,
    locks, and creates issues and edits and deletes comments, and a label can
    start intake. The mitigation is who holds it: only the daemon's publisher,
    which reads the target issue and its comments and creates the one comment.
14. **No §11 table edit.** The decomposed units are unscheduled deferrals and
    the spine places them (expected 1B.1), following revision 64's precedent.

## Verification Findings

- **Intake independence holds at `be74208e`.** No code in `daemon/` handles
  `issue_comment` events or lists or creates issue comments, and label intake
  reads only an issue's number, state, and label names
  (`daemon/internal/publish/forge_label.go`). The one path that can see the
  comment is fetched research for a later specifier run on the same issue
  (`intakeWorkItemDocument`), which is input and never authority.
- **Revision 65 landed between planning and implementation.** It made the
  specifier a lineup role whose prompt is admitted by its own digest. That fits
  this design unchanged: the public-plan field changes the specifier prompt,
  which gets a new digest.

## Rejected Options

- **Post the full body.** Private-by-default text on a possibly public issue,
  and it barely fits.
- **Post the summary.** Already bounded at 8 KiB, but written for the owner's
  card, not for the public.
- **Edit one comment in place.** Hides the earlier plan behind GitHub's edit
  history and needs an update call.
- **A dedicated effect table.** A migration and store accessors for a record the
  ledger already models.
- **Let a failed post block the run.** Turns a visibility aid into a gate.
- **Content identity (task ID plus digests).** Fails the approve A, B, A
  sequence; see decision 9.
- **A clock-based recovery window.** Daemon and GitHub clocks differ, so the
  daemon's own comment could fall outside the window and be reposted.
  Candidates are instead the App-authored comments absent from the intent's
  pre-dispatch ID set, and no clock defines one.
- **Retry after a settled empty listing.** GitHub has no idempotency key for
  comment creation, so a dispatched attempt with no response can still commit;
  retrying can post a duplicate that re-fires accepted `issue_comment`
  workflows. That case is residual ambiguity instead.
- **Retry on any recorded error response.** A 5xx response can arrive after
  GitHub persisted the comment. Only a response that definitively rejected the
  request before creating anything shows that a create is safe, and the
  implementing unit lists those responses; a status class alone is not the test.
- **Recheck only the set of `issue_comment` workflows.** An edit to an accepted
  workflow, or to a local action it uses, would pass a set comparison and run
  automation the owner never reviewed. The recheck compares content digests.
- **Neutralize mentions and commands instead of rejecting them.** Inert
  rendering does not protect against bots that read the raw body.

## Revisit When

- A bound issue's readers need more than the public plan, such as the full body
  or the run lineage. That reopens the visibility-aid position.
- False refusals from the mention rule are common enough that frame-only
  comments are the norm.
- Another daemon feature comments on issues as the same App. Per-issue
  candidate matching then needs a stronger key than authorship and the intent
  window.
- GitHub offers a permission narrower than `issues: write` for creating
  comments.
- GitHub offers an idempotency key or a client-supplied identifier for comment
  creation. The dispatched-with-no-response case could then retry safely.
- External activity ingestion (#524) starts reading issue comments. The
  zero-authority rule for the daemon's own comment must hold there too.
