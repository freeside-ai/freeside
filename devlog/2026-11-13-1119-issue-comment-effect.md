# A Duplicate Finding Proposes a Comment on the Issue It Matches

Issue #3147. Tier A (the change posts untrusted finding text to GitHub,
gates the post on the issue-automation audit, and rebuilds a table), so a
mandatory note.

Chose a separate comment ledger that mirrors the follow-up filing ledger
over one generalized ledger for both, because the filing ledger is
Tier A, has run in production since 0089, and its tables, triggers, and
reconstruction would all change shape to carry a second effect. The
mirror costs about 2,000 lines of near-copy across the domain ledger, its
store, and the dispatcher; a third posting effect is when a shared ledger
earns its keep.

## Decision

- **The target comes only from the search record.** A comment's issue
  number is the duplicate-search record's `DuplicateIssue()`, and its source
  carries that record's digest. Admission and approval re-read the record by
  the adjudication digest and finding, and refuse a digest mismatch, a
  record that answered "new", or an issue that is not the record's answer.
  Re-listing open issues at approval was rejected: it would let the target
  move after the operator read the card.
- **The workflow set is checked twice.** The engine proposes a comment only
  when the repository's latest trust profile reads, names the project's
  repository ID, and accepts its `issue_comment` workflows. The dispatcher
  reruns `RecheckIssueCommentWorkflows` immediately before every post. An
  App-authored comment starts those workflows, so the check that matters is
  the one at post time; the proposal-time check keeps cards from appearing
  that could never post.
- **The recheck runs at the default branch, not the run's base ref.** Issue
  comment workflows run from the default branch, so that is the tree whose
  workflows a comment starts. The issue plan said the base ref; the contract
  said the default branch, and the contract wins. The dispatcher reads the
  live default branch from the repository on each post.
- **No cached audit.** A full audit is about 11 fixed GitHub requests plus
  one per workflow and local action file (about 23 for this repository),
  read from `workflow_auditor.go`, not from a live run. Comments are
  human-approved and capped per run, so the cost is accepted rather than
  cached.
- **Text is screened under `github-issue/1`.** The comment body is the
  filing body's ruleset, which rejects mentions and command shapes. A body
  the screen refuses takes fixed text that names the finding by the ID the
  daemon derived for it and holds nothing from the agent, a reviewer, or
  the repository. An ID the screen would refuse is left out.
- **One cap, counted together.** The filing per-run cap counts filings and
  comments together, so a run cannot exceed its follow-up budget by
  splitting it across comments. The filer and the dispatcher each recount
  under the writer lock in the transaction that starts the attempt, so the
  two cannot both pass a read taken before the other's start. A comment
  creates no issue, so the per-day and depth caps don't apply.
- **A refused profile raises one run notice.** Without an accepted set the
  engine writes no comment and raises `issue_comment_refused` on the run, the
  filer's notice pattern. Plan §5.13 says the refusal is recorded beside the
  finding's deferred disposition; the run notice is how it is recorded today.
- **Ambiguity is per issue.** The pre-dispatch set is the App's comments on
  the target issue, and one outstanding intent is allowed per issue, so an
  ambiguous post blocks adoption on that issue only, not on the repository.
- **The dispatcher shares the filer's supervision kind.** It posts on the
  duplicate branch of follow-up filing, and a separate component kind would
  add a health surface for the same feature.

## Consequences

- Migration 0139 rebuilds `effect_proposal_instances` to admit the new kind
  in its CHECK, which makes it a Tier A migration. The issue plan had
  classified it Tier B as additive.
- A revised adjudication searches again under its own digest, so an open
  comment card from the earlier adjudication keeps pointing at the earlier
  record, and its approval re-gate still passes. Accepted: the earlier
  search is what the operator read and approved.
- The `issue_comment` gate key still has no effect: every comment takes a
  card until #3150 dispatches without one.

## Refute-First Findings

A fresh-context reviewer read the diff against the contract. Outcomes:

- **Fixed: the per-run cap could be overrun.** The filer and the dispatcher
  read the count before their forge call and wrote the start after it, so a
  filing and a comment could each pass at the cap's last slot. Both now
  recount in the start transaction; a test lands a filing while the
  dispatcher lists comments and expects the comment capped.
- **Fixed: the recheck did not bind the profile's repository.** The
  dispatcher read the latest profile by name and did not compare its
  repository ID with the approved comment's. It now refuses a mismatch.
- **Declined: a search for another repository aborts the proposal.** The
  arm returns an error when the search's repository differs from the
  target's. Unreachable: a project's repository ID never changes
  (`RegisterProject` refuses a rebind), and the search and the target both
  read it.
- **Declined: a stuck intent blocks its issue.** An issue with more than
  1,000 comments, or an audit that always fails, leaves the intent
  outstanding. The filer has the same shape per repository, and the stall
  notice surfaces it to the operator.
- **Declined: zero candidates at settle record ambiguity.** A missing
  comment after a send that may have landed is not proof it failed, so the
  ledger refuses to retry, as the filer does.
- **Declined: a stale source reopens the write on each reconcile.** A
  duplicate whose source went stale before its comment was proposed keeps
  the separate-work pass unsettled, so the pass opens a write that inserts
  nothing. The filing arm has the same shape.

## Revisit When

- A third posting effect needs an intent ledger; then generalize the two.
- The per-post audit shows up as rate-limit pressure or slow approvals.
