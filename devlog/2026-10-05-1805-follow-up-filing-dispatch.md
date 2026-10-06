# Follow-Up Filing Dispatch

Issue #1634 adds the dispatcher that files an approved follow-up issue on
GitHub exactly once and recovers a filing a crash interrupted (plan §5.17).
It drives the ledger #1626 added and changes no shared package. This note
records the decisions the issue and its plan left open, and the two places
where the plan met code it had not accounted for.

## What Is Not In This Unit Yet

The filer is built and tested against a token source, but nothing mints its
token or starts it. Plan steps 1 and 8 (the `issues: write` permission set,
the filing token source, and the composition in `cmd/freesided`) wait on
#1768.

**Chose to stop and file a contract issue over minting with an incomplete
audit row.** Every mint writes a `publish_mint_audits` row with one column
pair per permission scope, and there is no `issues` pair. A filing token
would be recorded as granting less than it does. The audit is the durable
record of what authority the daemon held, so a row that understates it is a
false record, not a cosmetic gap. Adding the column is a migration and a
`store.MintAudit` change, both outside this unit and both contract work.
Rejected: recording the scope under an existing column (false in a
different way), and skipping the audit for filing mints (no record at all).

## Owner Decisions Carried From Planning

The plan put five assumptions to the owner. The owner started implementation
by fiat without changing any. Reading that as acceptance is the agent's
interpretation, not a recorded statement.

1. **A filing sends at most three create requests.** Only a transient
   rejection permits another.
2. **A settle listing may fail three times** before the filing ends
   ambiguous.
3. **Every terminal outcome that files nothing raises an item,** not only
   the ambiguous one §5.17 names. A refusal the operator never hears about
   looks the same as a filing that is still pending.
4. **Four policy keys,** read in `publish` and never added to `domain`:
   `follow_up_filing.settle_interval` (10m), `.max_per_day` (10),
   `.max_depth` (2), `.max_per_run` (5).
5. **What each cap counts.** The daily cap is per repository over a trailing
   24 hours and holds the filing without opening an intent. The depth cap is
   the new issue's depth in Freeside-filed ancestry and refuses. The per-run
   cap is per origin run and refuses. All three count filings that may have
   created an issue: ledgered, ambiguous, or dispatched and unresolved.

## The Response List

The contract fixes the list; the reasoning is recorded here because the
list is an allowlist and the next status someone wants to add needs the
test it has to pass.

A response is a **definite rejection** only when its status means the
request was refused before anything was created: 400, 401, 404, 410, 422,
and 403 without a rate-limit header. It is **transient** only when it is a rate
limit: 429, or 403 with `x-ratelimit-remaining: 0` or `retry-after`.
Everything else a dispatched request can observe is **unproven**: any 5xx,
any other status, a 201 whose body does not decode to an issue, a transport
error, a timeout.

**Chose an allowlist over classifying by status class.** A 5xx or a dropped
connection says nothing about whether the create committed, and treating
either as a rejection is how a retry files a duplicate. An unlisted status
costs one settle interval; a wrongly listed one costs a duplicate issue.

## Decisions Made In Implementation

**Discovery walks approve commands, not the item listing.** The owner
accepted this departure from the plan on 2026-10-05. The plan read
approved filings from `ListAttentionItems`. That listing re-gates every
item's evidence and fails as a whole when one proposal instance no longer
reads, so one tampered instance would have stopped every filing, against
the acceptance criterion that the pass continues. The filer instead lists
accepted approve commands and reads each item through the record tier
(`GetAttentionItemRecord`), which authenticates the item's history without
the evidence gate. Nothing is trusted from that read: it yields the project
and subject a notice needs, and the instance read behind it is the gate.
The same tampered instance still breaks the item listing the app reads;
that is the store's existing fail-closed behavior and not changed here.

**An intent whose instance stops reading cannot be refused.** The plan had
every precondition failure end in `RefuseFollowUpFiling(precondition_failed)`
so the repository is never left blocked. Every ledger write re-runs the
instance gate, so the store rejects that refusal for exactly the instances
that need it. The filer raises a `follow_up_filing_unreadable` item and
leaves the intent outstanding, which blocks later filings in that
repository until the rows are repaired. Rejected: a write path that skips
the gate, which is a store change and would let a forged row reach a
terminal outcome. A tampered instance that never opened an intent blocks
nothing. One that holds any intent, a ledgered one included, blocks its
repository: the store rebuilds every intent in a repository before it
opens another, and that rebuild fails on the unreadable one. The filings
held behind it get the stall notice below.

**A 201 is ledgered only if its issue would pass as a candidate.** The
response body is a returned object. An issue that is authored by another
account, predates the intent, sits in the pre-dispatch set, or is a pull
request is not ledgered from the response; the attempt is recorded
unproven and the settled listing decides.

**The request is built before the dispatch marker.** Encoding the body and
acquiring the token can fail, and a failure after the marker is durable
would have to be read as unproven. With the request already built, the only
step after the marker is the send.

**The candidate window opens five minutes before the intent, not at it.**
This departed from the contract's literal test, `created_at` at or after
the intent's `opened_at`. The agent proposed it and the owner accepted it
on 2026-10-05; #1634's criterion now states the window this way. The two
timestamps come from different clocks: `created_at` is
GitHub's, to the second, and `opened_at` is the daemon's, sub-second. Read
literally, a daemon clock one second ahead of GitHub rejects the filing's
own issue, from the 201 and from the settled listing alike, and the filing
ends ambiguous with the issue filed and unledgered. The allowance costs no
safety because the pre-dispatch set is listed over the same widened
window: an App-authored issue that existed before the create was sent is
in the set whatever its timestamp, so the window never admits one.
Rejected: comparing against the response's `Date` header, which the
settled listing does not have for the original create. Beyond five minutes
of skew the filing fails closed to ambiguous, as before.

**A candidate is counted once per issue number.** A paginated listing can
return one issue on two pages when the result set shifts between requests.
Counted twice, the filing's own issue would read as two candidates and
end ambiguous.

**The terminal outcome and its item are one transaction.** A refused or
ambiguous filing never exists without the notice that explains it.

**A transient rejection waits one minute,** measured from the recorded
response time, so the wait holds across restarts and does not depend on the
sweep interval. GitHub's `retry-after` value is not stored: the ledger
records a response class, not headers.

**The listing-failure count is in memory.** A restart grants a fresh bound
of three. The count bounds a persistent failure, which is what the
criterion asks; making it durable needs a ledger column.

**Caps are counted from the pass's own discovery.** The store exposes no
listing of intents by repository or run. The filer counts over the approved
filings it discovered, reading each one's intent. A filing whose intent
does not read fails the count rather than passing the cap. Only filings in
the cap's own repository or run are read, so an unreadable intent fails
counts there and nowhere else.

**The daily window counts from when a create was sent, not from when the
intent opened.** An intent can wait open through an outage longer than the
window. Counted from its opening, it would file on recovery and hold no
slot, and the next filing in the repository would pass a cap of one in
the same pass. Found by the automated reviewer.

**A held filing and a refused filing raise different items.** The daily
cap holds a filing under `follow_up_filing_waiting`; a depth or per-run
refusal uses `follow_up_filing_capped`. An item's ID is derived from its
code, so one shared code would have let a refusal reuse the waiting item
the operator had already acknowledged.

**A filing that fails for fifteen minutes raises a stall notice.** A
precondition read that keeps failing without a definite answer leaves the
intent open and is retried, which is right, but it also holds every later
filing in the repository with nothing in the queue to say so. The filer
tracks the first failure in memory and raises `follow_up_filing_stalled`
once the filing has failed on every attempt for fifteen minutes. The
notice is advisory and changes no outcome. Rejected: bounding the retries
and refusing, which would turn an outage at GitHub into refused filings a
human had approved.

**The filer resolves a transient notice when its condition ends.** The
waiting, stalled, and unreadable notices describe a state a filing can
leave, and an operator can acknowledge an advisory item but never resolve
it (plan §4). Left alone, a filing the daily cap held for a day would keep
an open "is waiting" item after it filed. Each pass records which of the
three conditions it saw still holding and resolves every other open one,
the pattern the engine's held-work notices use. A resolved item is final,
so a condition that returns raises a new item whose ID carries the next
occurrence number. The terminal notices (ambiguous, refused, capped)
record an outcome and stay open. Rejected: leaving the notices open, and
resolving at each place a condition ends, which is several places per
condition. Found by the automated reviewer.

## Known Gap: The Dispatching Identity Is Not Recorded

The intent records no App identity, and the proposal deliberately names no
filer, so the filer resolves the bot account fresh at each step. If a
repository's App registration changes while a create is unproven, the
settle step lists the new App's issues against a pre-dispatch set built
from the old one's, and an issue the new App authored in the window can be
adopted as this filing's. Found by the automated reviewer.

**Chose to file #1777 and gate the composition on it over a guard in the
filer.** The binding has to survive a restart, so it belongs on the intent,
which is a ledger field and a migration: contract work outside this unit.
Rejected: remembering the identity in memory, which a restart erases, and
re-registration is likely to involve one; and listing every author's
issues into the pre-dispatch set, which closes only the issues that
already existed. Nothing constructs the filer yet, so no daemon runs with
this gap, and the step that would start it must wait for #1777.

## Refute-First Findings

A fresh-context reviewer was asked to break the dispatcher, not confirm
it, against three claims: no approved filing creates two issues, no filing
is ledgered twice, and no issue the filing did not create is ledgered.

It found no path to any of the three. It tried and could not break: a kill
between any two steps of a pass (the test matrix enumerates every one), a
resend after an unproven create, adoption while an earlier intent in the
repository is unresolved, adoption in an ambiguous repository, a 201 body
naming an issue by another author or from the pre-dispatch set, two
passes racing on one filing, and a wake during a pass.

Findings, each with its outcome:

- **Confirmed, fixed: clock skew discards a real 201.** The literal window
  rejected the filing's own issue when the daemon clock ran ahead. See the
  five-minute window above.
- **Confirmed, fixed: the waiting item and the cap refusal shared an ID.**
  See the two codes above.
- **Confirmed, fixed: a filing wedged before its first attempt raised no
  item.** See the stall notice above.
- **Confirmed, fixed: a duplicated listing row counted as two candidates.**
- **Confirmed, narrowed: one unreadable intent failed cap counts in every
  repository.** Counts now read only their own repository or run.
- **Confirmed, allowed: an unreadable instance that holds an intent blocks
  its repository.** The block is the store's, by the gate on every ledger
  read, and failing closed is the intended direction. The earlier text of
  this note claimed only an unresolved intent blocked; that was wrong and
  is corrected above.
- **Allowed: `settle` returns a malformed-cap error without refusing.**
  Not reachable: a filing reaches `settle` only after `dispatch` parsed
  the same run's policy, and a run's resolved policy does not change.
- **Test gaps closed:** the skew window, a token value in any notice or
  pass error, and a refused approve waking the filer.

## Revisit When

- **The source's currency matters between approval and create.** The
  approval gate is the last currency check; a finding fixed after approval
  still files. A re-check needs a store read this unit could not add.
- **Approve commands number in the thousands.** Each pass reads every
  accepted approve command and its item record. A store listing of approved
  filings without a terminal outcome would replace the scan.
- **Clock skew above five minutes shows up as ambiguous filings.** The
  allowance is a constant. If a host's clock drifts further, fix the clock
  first; widen the constant only if the pre-dispatch listing still bounds
  it.
- **The filer is about to be started.** #1777 must be merged first; see
  the known gap above.
- **An operator has to repair rows to unblock a repository.** An
  unreadable instance that holds an intent blocks its repository with no
  action that clears it. A store path that retires such an intent would
  remove the block.
- **An operator needs to clear an ambiguous repository.** No human action
  settles `ambiguous` (a non-goal here), so the repository adopts no
  candidate from then on.
