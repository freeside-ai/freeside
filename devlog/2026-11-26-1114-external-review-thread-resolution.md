# Resolve the Thread After a Declined or Deferred External Reply

Issue #2358. Tier A: `daemon/internal/publish/` trusts GitHub's answers to
decide which review thread gets resolved and whether it counts as resolved.

## Decision

After a declined or deferred reply posts in an external reviewer's inline
thread, the replier finds the one thread on the reply's pull request whose
first comment is the finding's root comment and resolves it, once per posted
reply. A failure records the result and raises one advisory
`external_review_thread_unresolved` notice; only a response in which GitHub
refused nothing (a proven rate limit, a transport failure, a 5xx) is tried
again, under the reply's bound of three attempts per process a minute apart.

## Choices and What They Rejected

- **Resolve a thread only when every other finding in it is settled,** over
  deciding per finding as the contract's acceptance 1 reads. Ingestion makes
  every inline comment from every identity a finding whose thread is its
  root (`daemoncompose/external_review.go`), so one thread can hold a fixed
  finding, one still waiting on a person, or a human's unanswered root
  comment beside a declined one. Resolving for the declined one would
  collapse the others, and the contract keeps a fixed thread open. A thread
  is due only once each other finding in it has a posted declined or
  deferred reply on its latest disposition; until then nothing is recorded
  and a later pass checks again. Findings the App wrote (its own replies,
  which ingestion also reads back) are skipped, or no thread it answered
  would ever resolve. The fresh-context review before the push found this.
  The cost, which a later review round confirmed and this note accepts: a
  comment from someone Freeside doesn't answer (the PR author's "+1", an
  unadmitted bot) or a sibling reply that never posts keeps the thread open
  for good, with no notice, and the gate re-reads the run's findings each
  pass while the run is open. Leaving a thread open is the behavior before
  this unit, so the failure costs a hand resolve, never a collapsed
  conversation. Resolution is final per reply, so a finding added in the
  thread later replies into a resolved thread; Freeside doesn't reopen it.
- **Match the thread by its first comment's `fullDatabaseId`,** over
  `databaseId`. GitHub deprecated `databaseId` on review comments because the
  IDs no longer fit 32 bits. `fullDatabaseId` is a string; it is parsed as a
  canonical positive integer, as the stored thread ID is.
- **Fail the whole lookup on any malformed thread,** over skipping it. A
  thread with no `id`, no `isResolved`, or an unparseable first-comment ID
  fails the lookup even when the target thread is present on the page. The
  response is untrusted, and a page that can't be read whole can't prove
  there is exactly one match.
- **Stop paging at the page that holds the match,** over reading every page
  to prove the match is unique. Ingestion derives the root from GitHub's
  `in_reply_to_id` (`daemoncompose/external_review.go`), REST comment IDs are
  unique, and a thread's first comment is its root, so a second thread with
  the same first comment can't exist. Two matches on one page still fail.
- **No "not applicable" record,** over writing one for every reply that will
  never resolve (a fixed reply, a review-body finding, a closed run). Each
  such reply costs a few store reads per pass instead; the due check orders
  its reads so finished work costs the fewest.
- **Resolve earlier replies on upgrade.** Declined or deferred replies posted
  before this change on runs still open have no resolution record, so their
  threads get resolved on the first pass after deploy. The contract intends
  it.

## Refute-First Findings

One independent reviewer tried to prove the response handling and the due
check wrong, with scratch tests run against the change.

- **Confirmed, fixed: a dropped body on a 200 failed permanently.** A
  connection closed mid-body read as a malformed response and raised the
  notice. It is a transport failure, so it now retries; a body that reads
  whole but doesn't decode still fails.
- **Allowed by decision: admission isn't rechecked after the token mint.**
  The due check reads admission in one transaction, then the forge mints the
  token and sends. A reviewer revoked in that window still gets the thread
  resolved. The reply already posted, and resolving grants nothing and
  publishes no text, so the window isn't worth a second read. `send` keeps
  its recheck because a reply is the irreversible step.
- **Allowed by decision: a duplicate on a later page is never seen.** See
  the paging choice above.
- **Allowed by decision: one bad unrelated thread fails the lookup.** See the
  malformed-thread choice above.
- **Disproved:** resolving the wrong thread (the mutation's ID comes from a
  page whose repository and pull number were checked, and the response must
  echo it); counting an unresolved thread as resolved (`isResolved` must be
  present and true); an endless page loop (an empty or repeated cursor fails,
  and paging stops at 100 pages); retrying a definite refusal; recording
  twice or on a closed run; a token or response body reaching an error or
  notice; and a regression for outcome rows written before this change.

## Declined Review Findings

- **A renamed repository raises the notice.** The lookup requires
  `nameWithOwner` to equal the name recorded at publication, as `setPRDraft`
  does. A rename after the reply fails closed into a notice that asks for a
  hand resolve; it can't resolve the wrong thread.
- **Rate limits are read from headers alone,** as the reply path reads them
  (`forbiddenByRateLimit`). A refusal that spent the last point is retried at
  most three times, and a secondary limit with neither header raises the
  notice. Both are bounded and match the reply's classification. A 200 whose
  `errors` say GitHub failed internally, with no rate-limit header, raises
  the notice too: the contract retries only a proven rate limit, a transport
  failure, or a 5xx.
- **A run that closes during the call still gets its thread resolved,** with
  no record, since the write rechecks closure and writes nothing. Like the
  admission window above, resolving grants nothing and publishes no text.

## Revisit When

- **GitHub refuses the App the mutation.** The publish token's
  `pull_requests: write` hasn't been tried against `resolveReviewThread`. If
  it's refused, every declined or deferred inline reply raises the notice,
  and acceptance 5's fallback (README note, no code) applies instead.
- **A spent GraphQL rate limit stops answering 200 with `errors` and
  `x-ratelimit-remaining: 0`.** That shape comes from GitHub's documentation,
  not a test; if it changes, that case becomes a notice instead of a retry.
