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
- Extracting the create status classifier must preserve filing decisions.
  Its comparison test covers every HTTP status from 100 through 599, both
  decoded-body states, transport failure, and rate-limit header forms.

## Revisit When

Reply volume makes retained outbox history costly, another writer uses the
same App account concurrently, comment workflows need stronger admission,
or the owner needs an action to settle ambiguous replies in the client.
