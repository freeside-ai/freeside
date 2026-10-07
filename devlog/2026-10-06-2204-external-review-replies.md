# Reply to External Reviews Through the Outbox

Issue #1636 uses the existing outbox for reply intents and outcomes. A
separate ledger would require a shared contract and migration for a lower
cost duplicate than follow-up issue filing. The poster treats an uncertain
send as irreversible: it adopts one new comment by the recorded App account
in the original thread, or records ambiguity and asks the operator to inspect
the pull request. It never retries an uncertain send.

## Public Text and Authority

The reason is screened with the existing `github-issue/1` ruleset and rendered
as escaped code. A new ruleset would require a domain contract change. The
active trust profile is checked before sending, even though the disposition
already proves admission under its cycle's profile. Fixed replies wait for a
reviewed head to be published and name that published commit, with an explicit
statement that Freeside has not proven the finding fixed.

Inline comments receive real replies. Review bodies receive conversation
comments with a root-relative link to the review on the same forge web host.
The API base does not reliably identify that host. The Claude publish chain
owns construction; fake publication creates no poster. Backup registers both
outbox kinds, which contain no artifact references.

The owner-assigned plan accepts comment-triggered workflows without a new
workflow-audit gate. The existing audit does not inspect these triggers.
Screening limits the agent-controlled reason; it cannot prevent a workflow
from reacting to any App comment.

## Refute-First Evidence

- Malformed create identities must not prove success. Tests reject missing
  IDs, authors, and timestamps and treat malformed 201 responses as unknown.
- A malformed listing must not reduce the recovery candidate count. The
  conversation-comment reader rejects the entire listing.
- Independent review confirmed that a decoded head and its decoded digest
  could agree without proving publication. Pending sends now reload the
  pinned ready-item binding and recheck the reviewed head against it.
- Independent review confirmed that pagination could follow another PR's
  comments on the same API host. Conversation creates and listings now
  validate `issue_url`; a wrong-resource next page fails the whole read.
- Cancellation at each identity, token, and HTTP boundary, followed by closing
  and reopening the store, produces at most one create. Only proven transient
  rejection releases a dispatch claim. Recovery starts a fresh ten-minute
  settle interval after restart because intent age does not prove send age.
- A validated create response proves that response's new App-authored comment
  without comparing GitHub's clock to the daemon's. Unknown-send recovery
  still requires the recorded timestamp criterion, so skew there remains
  ambiguous rather than risking adoption of an older comment. A dispatching
  intent also settles after reviewer revocation; revocation prevents new sends,
  not recovery of an irreversible request already attempted. That recovery
  still compares the intent coordinates to the historical finding and ready
  binding, so a revoked profile cannot authorize a mismatched decoded intent.
- Extracting the create status classifier must preserve filing decisions.
  Its comparison test covers every HTTP status from 100 through 599, both
  decoded-body states, transport failure, and rate-limit header forms.

## Revisit When

Reply volume makes retained outbox history costly, another writer uses the
same App account concurrently, comment workflows need stronger admission,
or the owner needs an action to settle ambiguous replies in the client.
