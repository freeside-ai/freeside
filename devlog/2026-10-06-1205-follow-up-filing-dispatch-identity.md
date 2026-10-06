# Follow-Up Filing Dispatch Identity

Issue #1777 holds a follow-up filing to the GitHub App that dispatched it
(plan §5.17). Before it, the filer resolved the repository's App identity
fresh at every step. A registration that changed between the pre-dispatch
listing and the settle listing let the filing list the new App's issues
against the old App's set and adopt an issue it never created. This note
records the choices the issue left to the spine and what the refute-first
pass found.

## The Record

**Chose the App bot's numeric user ID over the registration ID.** The
comparison needs to tell two Apps apart, and the bot user ID is what GitHub
stamps on an issue's author, so it is the same fact the candidate check
already compares. `publish.FollowUpFilingIdentitySource` returns
`publish.AppBotIdentity`, which carries the slug and the bot user ID and
nothing about the registration. Recording the registration ID would widen
that type, which the publisher and the preflight share. Re-registering the
same App under new credentials keeps the bot user ID and is correctly read
as unchanged: the check distinguishes Apps, not registrations. Rejected
also: the slug, which an owner can rename while the App stays the same.

**Chose the pre-dispatch set as the identity's home over a field on the
intent.** The set is the list of issues one App's bot account had authored
before the first create, so the identity is a property of the set, not a
second fact beside it. Writing both in one guarded `UPDATE` means a set
never exists without the identity it was listed under on a row written
after migration 0093, and the 0089 write-once rule covers the pair.

**Chose to compare in the filer over comparing in `Adopt`.** The domain
rule has no access to the live registration, and its doc comment already
leaves App authorship to the caller. The domain's part is narrower and
unconditional: `StartAttempt` refuses a set with no identity, so no create
is sent under an unrecorded identity whatever the caller does.

**A changed identity at settle is ambiguous at once.** It is a definite
answer, not a listing failure, so it does not wait out the listing bound.
The filing may have created an issue under the old App that the daemon can
no longer list, so `refused` would claim more than is known.

**A changed identity before a create is refused, not re-listed.** The
alternative was to discard the set and list again under the new App. The
set is write-once by trigger and by rule, and a filing whose last create
was a transient rejection has an attempt that references it. A refusal
frees the repository; the next approved filing opens a fresh intent under
the new App.

## Rows Written Before the Migration

**Chose no backfill.** The App that listed an existing row's set is not
recorded anywhere, and writing the current one would claim evidence the
daemon never had. This is the argument 0091 makes for its column.

**A row with no identity fails closed in the direction its history
allows.** With an unproven attempt it ends `ambiguous` and adopts nothing.
With no attempt, or only a transient rejection, it is refused before any
create. Such a row cannot be told from one whose column was cleared by
hand; both take the same path, and the trigger refuses the clearing on a
live store. The trigger also refuses filling the identity in afterwards on
a row whose set is recorded, so a legacy row cannot be repaired into a
claim.

**A row with no identity is decided on the ledger alone, before anything
else.** No answer from GitHub changes its outcome, so settle records
`ambiguous` without waiting the settle interval or resolving the target,
and dispatch refuses before its caps are read or the target is resolved.
The first version made the check after the target resolved. A repository or
App that could not be read then held the filing: at settle it spent the
listing bound, which is kept in memory and starts over at a restart, and at
dispatch it retried with no bound. Either way the open intent held the
repository's later filings behind it. Review found the settle case; the
dispatch case is the same class. A changed identity cannot move the same
way, because it is known only from the live read.

## Departures From the Plan

- **The dispatch check runs after the request is built,** not right after
  the target is resolved. The first finding below is why.
- **The sentinel is `ErrFollowUpFilingIdentityMissing`,** not the plan's
  `ErrFollowUpFilingDispatchIdentityMissing`. The longer name is the
  longest in the errors block and realigns every line of it.
- **The storetest seed's bot user ID is unexported.** The plan exported it
  for the publish tests, which turned out to drive the identity through
  their own harness and never name the seed.
- **`RecordPreDispatch` refuses a non-positive ID through the set's own
  validation,** not a second explicit check.

## Refute-First Findings

An independent reviewer was given the diff and the intended outcome and
asked to break three claims. Each finding is recorded so it does not
return.

- **Confirmed and fixed: the create's token is minted apart from the
  identity that was checked.** The first version compared identities when
  the target was resolved. The create request takes its own token later, so
  a registration that changed during the milestone lookup, the pre-dispatch
  listing, or the set's write sent the create as the new App. It failed
  closed (the 201's author did not match, the attempt was unproven, and
  settle ended `ambiguous`), but it left an issue nothing ledgers and broke
  the criterion that such a filing sends nothing. The dispatch check now
  reads the identity after the request holds its token and before the
  dispatch marker. A registration that changed before the mint is seen by
  that read; one that changes after it leaves a request already carrying
  the old App's token.
- **Allowed by decision: two registration changes inside one dispatch.** A
  registration that moves to another App for the token mint and back
  before the identity read is not detected, because nothing ties a token to
  an identity without the registration ID this unit chose not to record.
  The outcome still fails closed: the 201's author is not the recorded
  App, the attempt is unproven, and settle finds no candidate under the
  recorded App and ends `ambiguous`.
- **Disproved by a check: another App's issue can be ledgered.** The 201
  path admits only an author equal to the identity just compared with the
  set, and adoption lists and filters by that same identity. `Adopt` and
  `LedgerSuccess` each have one caller.
- **Disproved by a check: an unchanged identity changes an outcome.** Old
  and new `settle` make the same calls in the same order with the same
  listing-failure bookkeeping, and a target that fails to resolve still
  counts toward the bound. The existing suite, kill-at-every-step
  included, passes unchanged.
- **Disproved by a check: the recorded identity can be changed, cleared, or
  filled in.** The 0093 trigger refuses all three once the set is recorded,
  and 0089 refuses un-recording the set. Reconstruction refuses an identity
  without a set. A restore copies the column.
- **Disproved by a check: the tests pass with the check broken.** Removing
  the settle check, the dispatch check, the ID comparison, the
  `StartAttempt` guard, the trigger's NULL case, or the decode error each
  fails a named test. Reading the dispatch identity before the token mint
  fails the test that changes the registration at that step.

A second pass covered the review fix that decides a row with no identity
on the ledger alone.

- **Disproved by a check: the early check changes an outcome for a set
  that records an identity, or for a filing with no set yet.**
  `dispatchIdentityFault` returns the same reason for every input as
  before, and a filing with no set yet skips the early check in dispatch.
  Settle is never reached without a set: the intent's validation refuses an
  attempt without one.
- **Disproved by a check: the early outcomes are not legal transitions.**
  Settle is reached only with an attempt, which `ambiguous` requires, and
  no rule ties it to the settle interval. Dispatch is reached only with no
  attempt or a last transient rejection, which `precondition_failed`
  accepts.
- **Disproved by a check: the new tests pass with the fix removed.**
  Against the earlier filer the unproven case records nothing in the pass,
  and the other two reach the token source and the forge, which the tests
  forbid.

## Revisit When

- The audit trail needs to join a filing to `registration_id`, or the
  double registration change above has to be closed by comparing the
  token's registration with the identity's. Either is a contract unit: it
  widens `AppBotIdentity` or the identity source.
- Plan §5.17's "share App authorship" should say per intent. The sentence
  is a clarification, held back here because editing the unit's scope
  would have withdrawn its independence records.
- The filer is composed with an identity source that does not re-read the
  registration on every call. The check is only as fresh as `Resolve`.
  `GitHubAppBotIdentityResolver` revalidates the registration binding each
  time; nothing composes the filer yet (#1768), so which source it gets is
  not settled here.
