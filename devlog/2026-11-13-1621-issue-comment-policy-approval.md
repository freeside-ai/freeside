# A Comment Posts Without a Card When Its Gate Is Off

Issue #3150. Tier A (the change grants a policy actor the authority to post
untrusted finding text to GitHub, and adds a store gate the dispatcher reads
before posting), so a mandatory note.

Chose a separate `issue_comment_policy_approvals` table, written in place of
the comment's card when `gates.issue_comment` resolves off in the run's
resolved policy, over reusing `effect_proposal_policy_approvals` (0080),
because the closure row binds a prospective merge and a comment binds none.
Widening the closure table would make the merge columns optional on a Tier A
row the publisher already trusts.

## Decision

- **The gate comes from the run's pinned policy.** The engine, the store
  writer, and the store reader all read `gates.issue_comment` from the run's
  `ResolvedPolicy`, never from the current Workflow layers. Absent or
  exactly `false` is off; any other value is on, so a typo, or a spelling
  `strconv.ParseBool` would accept such as `0`, keeps a person on the path.
  Turning the gate on in the pane affects only runs resolved afterwards; a
  run already in flight keeps the answer it was resolved with. That
  includes runs resolved before this change: their absent key now means
  off, as plan §3.2 says, so on upgrade their later comments
  post without a card where the profile accepts the `issue_comment` set.
- **The reader reads only write-once rows.** `ApprovedIssueComment` re-derives
  the proposal digest from the instance and the policy digest from the run,
  re-reads the gate from that pinned policy, and refuses when the instance
  has a card. All of these are immutable, so a comment the ledger posted
  stays readable after anything changes. It never reads the trust profile;
  the dispatcher already rechecks the profile's `issue_comment` set at the
  default-branch tip before each post, as it does for a card-approved
  comment.
- **One actor per comment.** An instance carries a human decision or a
  policy approval, never both. The policy writer refuses an instance with a
  card or a decision; `RecordProposalDecision` refuses any action, decline
  included, on a policy-approved instance; and the reader treats both
  present as corruption (`ErrRowInconsistent`). Letting a later human
  decline revoke a policy approval was rejected: no card is opened for one,
  so the only path to that decline is a forged command.
- **A posted comment raises one notice.** The dispatcher raises an advisory
  `issue_comment_posted` item naming the issue and the comment's URL, in the
  same transaction as the ledger write (both the direct post and the
  adoption of a lost response), so a crash cannot record the post without
  the notice. It is raised even when the run's workflow closed while the
  post was in flight: closure forbids advancing a workflow, not observing
  an effect it already made public. It uses the existing exceptional
  notice class: the operator learns what Freeside said in public without
  being asked first. A card-approved comment raises none, because its card
  already told them.
- **Filings keep the card.** Plan §5.17 lets a filing go without a card only
  when the trust profile has accepted the `issues` workflow set, and no unit
  builds that set. With `gates.follow_up_filing` off, each filing still
  takes its card and the run gets one `follow_up_filing_autonomy_refused`
  notice naming the missing acceptance. Building the set here would have
  put the unit at about two and a half times its line budget.

## Refute-First Findings

A fresh-context reviewer tried to refute the change from the diff and the
issue's intent.

- **Confirmed and fixed:** the posted notice was dropped when the run's
  workflow closed between the dispatch and the ledger write, because
  proposal notices skip a closed workflow. The posted notice now bypasses
  that skip.
- **Confirmed and fixed:** one policy approval whose run no longer read
  failed the whole discovery pass, stopping card-approved comments too.
  The listing now reports such a row on its own.
- **Confirmed and fixed:** the gate read through `strconv.ParseBool`, so
  `0` or `False` turned it off though a Workflow layer accepts only
  `true` and `false`.
- **Disproved by trace:** a post without a valid approval through
  discovery, restart, adoption, or ledger open (each re-reads
  `ApprovedIssueComment`); a policy approval beside a card or decision; a
  duplicate posted notice; the engine and store reading different policies.
- **Allowed by decision:** the upgrade turning in-flight runs autonomous
  (above), and the per-run filing notice on every run that files while
  `gates.follow_up_filing` is off, which plan §5.17 asks for.

## Revisit When

- The `issues` workflow set lands: filings then need the same policy arm,
  and `approvedFollowUpFiling` must accept it so a filed issue stays
  readable. Follow-up: #3198.
- A third effect needs a policy approval: then one shared table with a
  per-kind binding may earn its keep over a table per effect.
