# External Review Response

Issue #524 makes review activity on a published pull request drive something.
#1623 defined the allowlist, the external finding, and the `external_review`
authority (`2026-10-02-1249-external-review-admission.md`). #502 built the
re-entry cycle (`2026-10-03-1508-readiness-reentry-cycle.md`). This unit reads
the activity, stores it, starts a cycle for an admitted finding, and ends the
cycle on a person. It changes `daemon/internal/publish`,
`daemon/cmd/freesided`, `daemon/internal/engine`, and `daemon/internal/store`,
and no shared package.

## Decisions Stated at Planning (2026-10-05, on #524)

Planning stated these on the issue for the owner to veto. None was vetoed
before implementation started.

1. **Review activity means submitted reviews and inline review comments.** A
   review body is a finding unless the review approves or the body is empty.
   Every inline comment is a finding. Rejected: top-level conversation
   comments. The forge records no commit for them, and plan §5.19 requires a
   head binding that is read, never assumed.
2. **A finding belongs to the commit the reviewer commented on.** For an
   inline comment that is the forge's original commit, not the commit the
   forge later re-anchors the comment to. Rejected: the current commit, which
   would turn one old comment into a new finding on every later head.
3. **The thread ID has a fixed form:** `review/<review id>` for a review body
   and `review_comment/<id of the thread's first comment>` for an inline
   comment or a reply. #1636 replies by this ID.
4. **An unlisted identity's activity is stored and raises no item.** The
   objective allows "at most" a low-priority item. Rejected: a new attention
   type, which is a `daemon/internal/domain` change this unit can't make.
5. **One cycle handles every admitted finding on the published head.** The
   earliest admitted finding no cycle has answered starts it, because #1623's
   authority names one finding.
6. **Admitted findings may be adjudicated and remediated only when the
   cycle's base is the base the daemon admits at.** This narrows #502's owner
   decision 3 ("findings in a re-entered cycle go to a person") for this one
   origin. That decision rested on the base: after a base advance no agent
   run can be admitted on the new base. An external review cycle keeps the
   base its last review used.
7. **Freeside doesn't claim an external finding is proven fixed.** The
   existing proof looks for a finding's fingerprint in the next review, and an
   external finding's source never appears in a Freeside review, so the proof
   would pass without showing anything.

Decisions 6 and 7 govern adjudication and remediation, which this change
does not implement (see below). #502's decision 3 therefore still holds in
the code: no re-entered cycle starts remediation.

## Decisions Made in Implementation

1. **A dismissed review is not a finding.** Decision 1 exempts only
   `APPROVED`. The forge replaces a dismissed review's state with `DISMISSED`
   and no longer says whether it approved, so an approval would become a
   finding the moment a maintainer dismissed it. Chose to skip `DISMISSED`
   over storing it. The cost: a changes-requested review that is later
   dismissed, and was never read while it stood, is not stored. This narrows
   the literal text of decision 1 and is the owner's to veto.
2. **A whitespace-only inline comment is skipped,** as an empty review body
   is. It says nothing a person could act on.
3. **The location is the comment's original range.** The plan named the
   current line. The original range matches the original commit the finding
   is bound to (decision 2); the current line belongs to a later commit. A
   range that starts on the other side of the diff carries a start line from
   the other file version, which can be past the end line, so the finding
   then names the comment's own line alone.
4. **Intake commits in its own transaction, after a read that filters out
   stored findings.** `store.Write` advances the client-visible revision once
   per transaction even when it writes nothing. Without the pre-read, every
   pass over a pull request with any review activity would move the revision.
   A failed intake is collected and never blocks the pull and issue facts.
5. **Reviews and review comments are read unconditionally once their list
   spans more than one page.** The forge's validator covers page one only, so
   a first-page "not modified" hid activity on later pages. Reactions keep the
   validator: nothing here reads them. Rejected: leaving it, which the plan
   accepted as existing behavior. For native review it was a missed
   observation; here it is a finding that never arrives.
6. **A pass that fetches review activity, fails to store it, and is retried
   in the same process evicts the review cache.** The fetch has already
   advanced the validators, so the retry would be answered "not modified"
   and the activity would never be stored. Two exits do this: a discarded
   observation and a failed intake, both collected failures the loop retries.
   A failed pull commit and a refused health-item resolution evict nothing.
   They return the pass error, which ends the reconciler loop under a durable
   stop. The validators live in process memory, so the next pass runs in a
   fresh process and fetches unconditionally.
7. **The trigger takes the item's ID and has a read-only probe.** The plan
   passed the item and opened a write transaction on every pass. The probe
   (`ExternalReviewReentryDue`) and the writer
   (`StartExternalReviewReentry`) run one planning function, so the writer
   decides again from its own transaction's reads and never acts on a
   decision made in another. A pass with nothing to start opens no write
   transaction, for the reason in 4.
8. **The trigger skips from reads and fails on writes,** as #502's does. Every
   "start nothing" condition is decided before the first write and leaves the
   ready item open. A failure after the first write fails the transaction, so
   readiness is never withdrawn without a cycle behind it. That includes the
   store's refusal of a head Freeside did not push: the trigger reads
   `ReadyHeadIsForeign` first, so the refusal is a skip and not a failure
   repeated on every pass.
9. **The trigger needs the pull request open.** A merged pull request whose
   bound issue has not closed still has an open ready item. Without this
   check a comment on the merged head would start a cycle.
10. **The cycle always ends on a person, and every ending names the
    findings.** Under the proposed split this is part 2: the round's review
    record is followed by a `review_dispute` item whatever the review found,
    because an admitted finding is still open and nothing automatic answers
    it yet. That item, and every other item the cycle can end on (a review
    that stops on a quota failure, a round past the hard limit, a stopped
    cycle), names each admitted finding on the head: the reviewer's login,
    the thread, and the text, quoted and cut to 256 bytes, five at most with
    a count of the rest. Rejected: naming them only after a review record.
    The cycle has already withdrawn readiness for the finding and its
    authority has spent it, so any other ending would leave no sign that a
    reviewer objected.
11. **The item reads the findings through the authority's named profile.**
    `ExternalReviewCycleFindings` admits by the profile the authority sealed,
    not the active one, so the item names the same findings after the owner
    edits the allowlist mid-cycle.
12. **After a base advance the cycle reviews nothing, and says so.** A
    base-advance re-entry reviews the head's merge into the new base, so the
    last review's base is one the head does not descend from. The external
    review authority admits no prospective merge
    (`PublicationSuccessor.AllowsProspectiveMerge`), and changing that is a
    domain change. The cycle stops on an item that names the findings and
    says no review ran. Rejected: starting nothing, which leaves a listed
    reviewer's finding with no item at all. The base moves whenever anything
    merges, so on an active repository this is the common case, not the
    rare one. Follow-up: #1751.
13. **The probe drops unlisted reviewers in memory before the gated read.**
    Anyone can comment on a pull request, and the gated admission read costs
    a chain walk and a profile read per finding. The gated read still
    decides.
14. **A cycle past the round limit ends on an item of its own identity.**
    The first cycle writes its exhaustion item under the round-limit round's
    review identity, which that round may already have used for an item a
    person resolved on the way to ready. Written there, the external cycle's
    exhaustion was dropped (a resolved dispute) or refused on every pass (an
    adjudication item, or another head), with readiness already withdrawn.
    The cycle's item is `production-external-review-exhaustion-<run>-<round>`.
    Rejected: starting nothing at the limit, which the issue's acceptance
    rules out. A readiness re-entry past the limit has the same collision and
    is not changed here. Follow-up: #1755.
15. **Tests record the profile through the store, not through onboarding.**
    The plan asked for a profile onboarded with `-external-reviewer`.
    `TestOnboardReRunAdmitsExternalReviewers` already proves onboarding
    activates a profile that admits the reviewer, and its stubs live in
    another package's tests. These tests start from the same stored state.

## Trust Boundary: Review Activity From the Forge

The intake binds fields the forge returns for text anyone can write. The
refute-first pass on the identity mapping found:

Confirmed and fixed:

- Activity on a later page was hidden behind a first-page "not modified"
  (decision 5).
- A dismissed approval became a finding (decision 1 here).
- A multi-line comment that starts on the other side of the diff was dropped
  as an invalid range (decision 3).
- Fetched activity was stranded when the observation was discarded after the
  fetch (decision 6).
- A whitespace-only inline comment was stored (decision 2).
- Coverage gaps: the bound-open-pull gate, the dismissed state, a write-phase
  failure followed by a retry, and review order independence now have tests.

Accepted limit:

- **A person with write access can edit a listed reviewer's comment, and the
  forge's REST API keeps the original author.** The edited text is stored
  under the listed identity. REST exposes no editor; GraphQL does. Today the
  text reaches only a quoted attention item.

Disproved by a check: a null or zero account being admitted, two threads
sharing an ID, a finding bound to the moving commit, divergence after a
reorder, a repost, or a restart, a failed intake blocking the pull fact,
native review regressions, and a second pass in one process after a pass
error (`Run` is the only caller of `Reconcile`, returns on that error, and
is started once per process). A bot's login is stored with its `[bot]`
suffix as the forge returns it, so the allowlist must list the suffixed
login; native review strips it.

Not verified: what the forge returns for `in_reply_to_id` after a thread's
first comment is deleted. If it changes, a reply would be stored again under
another thread ID. A list past ten pages fails closed.

Declined: a reconciler-level test for text that is not valid UTF-8. The
builder's test covers the same path.

## Trust Boundary: The Trigger

The trigger turns stored text from the forge into withdrawn readiness and a
sealed authority. The refute-first pass found:

Confirmed and fixed:

- A cycle after a base advance stopped on "does not descend from the
  reviewed base" with no review and no mention of the finding (decision 12).
- A cycle whose review stopped on a quota failure, or that passed the round
  limit, ended on an item that did not name the finding (decision 10).
- The probe took about a second per pass with 351 unlisted findings on the
  head (decision 13).

Disproved by a check: admission through an unlisted identity, a renamed or
reused login, a removed reviewer, another repository's profile, another
head, or another run; a finding on one head sealing an authority for
another; a store refusal the trigger's reads miss; a withdrawn item with no
cycle behind it; a second cycle from a second pass; a ready item written
once the cycle's review record exists, including after a push mid-cycle; a
second item after a restart; reviewer text escaping its quotes.

Not pinned by a test, and kept because `StartReadinessReentry` carries the
same checks: a review failure later than the latest record, a review record
with no base, and a task the lane could not decode. No state the store
accepts reaches them. The "already answered" skip is also unreachable today:
no ready item reopens on a head after an external review cycle.

Not verified: a hold (revoked recipe, review configuration) inside an
external cycle, a review configuration that changes between the record and
the escalation, the Discuss and Stop decisions on the cycle's item, and a
pull request that merges mid-cycle.

## Pre-Push Independent Review

A fresh reviewer was given the diff and the intended outcome, and asked to
refute it.

Confirmed and fixed:

- The exhaustion item collided with the round-limit identity (decision 14).
  Run with a resolved dispute seeded there: the cycle ended with no open
  item. The adjudication-item and other-head refusals were read, not run.

Added from this review, then removed:

- An eviction of the review cache after a refused health-item resolution.
  The automated review round traced that exit: it ends the reconciler loop
  for the life of the process, so no later pass in that process is answered
  "not modified" (decision 6).

Declined:

- **A parked or held cycle's item does not name the findings.** A review
  configuration the profile no longer approves, a revoked recipe, and a
  transport failure park the cycle on the existing hold items. They are not
  endings: the cycle resumes when the hold clears, and the item it then ends
  on names the findings. Until then the run is in the state #1623's decision
  5 already accepts, readiness withdrawn with no item saying why. A person
  who answers a hold with Stop ends the run without Freeside having shown
  the finding; it stays on the pull request. Appending to the hold writers
  would touch three item shapes whose reasons other code compares.
- **Typographic quotation marks pass through the quoting.** The delimiter is
  an ASCII quote, which is escaped. Escaping every non-ASCII rune would make
  a comment in any other script unreadable, the reviewer is one the owner
  listed, and the findings list is the last thing in the reason, so nothing
  Freeside says follows it.

Disproved by a check: the in-memory allowlist filter dropping a finding the
gated read admits; a retry writing a different reason and tripping an
idempotence check; the findings read wedging a cycle or changing another
task's item; an external cycle re-earning readiness; a revision advance on
an unchanged pass.

## Adjudication and Remediation Are Not in This Change

The plan's part 3 (steps 10 to 13) said to stop and file a contract issue if
recording a remediated external finding needed a shared-package change. It
does, and two more facts make part 3 its own planning question:

- **No disposition can be recorded for an external finding.** The
  `finding_disposition_requires_round_finding` trigger
  (`daemon/migrations/0037_finding_dispositions.sql`) refuses a row whose
  finding is not linked to the round's review record, and `PutFinding`
  refuses to link an external finding to one (plan §5.19's quarantine). So a
  declined, deferred, or fixed external finding has no durable record, and
  #1636's replies have no disposition to name. Lifting the trigger is a
  migration. Follow-up: #1749.
- **The reviewer's words would reach an agent as instructions.**
  `remediationInput.Findings` carries each finding's message and raw text
  into the remediation input unquoted, and the remediator's prompt
  (`prompts/phase-1a/remediator.md`) calls that input daemon-authenticated.
  The adjudicator receives the finding whole as well. Both framings live
  outside this unit's paths, and the comment-edit limit above means the text
  is not always the listed reviewer's.
- **Remediation is admitted at one base for the whole daemon.**
  `AllowsRemediation` requires the cycle's base, and every agent run is
  admitted at the daemon's configured base. Decision 6 on the issue already
  sends the mismatch to a person; how often the two agree in practice was
  not measured.

Rejected: adjudicating external findings with no disposition rows. A round
would then close on the adjudication artifact alone, the remediation review
path would have to skip its `fixed` row, and whether the ready-item and
publication gates accept such a round was not traced.

What part 3 can reuse unchanged, from reading and not from a run: the domain
refuses nothing about an external finding in an adjudication entry or a
remediation intent, and `AllowsRemediation` already treats an
`external_review` cycle as Freeside's own head.

## Consequences Worth Knowing

- **Listing a reviewer acts on old comments.** The trigger reads stored
  findings. The first pass after the owner lists a reviewer starts a cycle for
  every ready pull request whose published head already carries a comment
  from them.
- **Any counted comment withdraws readiness.** "Thanks, looks fine" as an
  inline comment from a listed reviewer supersedes the ready card, spends a
  review round, and ends on a person.
- **A cycle at the round limit still withdraws readiness.** It ends on the
  existing "Review exhausted" item without a review.
- **A stopped external cycle borrows the readiness wording.** A missing head
  or failed verification reads "Readiness re-entry stopped ...", followed by
  the findings.
- **A comment can miss its head.** A finding on a commit that is no longer
  the published head is stored and drives nothing, and no item says so.
- **Nothing is read while a cycle runs.** The reconciler reads activity for
  open ready items only.
- **A withdrawn ready card does not say why** until the cycle's item appears
  (#1623, decision 5).
- **Every pass lists the run's findings.** The `findings` table has no index
  on `run_id`, and an index is a migration. The read is one scan per open
  ready item per pass.

## Revisit When

- Text from an external finding is shown to an agent. The comment-edit limit
  above then matters: read the editor through GraphQL, or skip edited
  comments.
- Listed reviewers leave conversational inline comments often enough that
  cycles are wasted. Decision 1 on the issue is the rule to narrow.
- A cycle can re-earn readiness on the same head, as adjudication would
  allow. Decision 5 then needs a durable record of every finding a cycle
  handled: today only the one its authority names is recorded, and each
  remaining finding would start another cycle.
- The findings scan shows up in a pass's time. The fix is an index, which is
  a contract change.
- The forge's `in_reply_to_id` behavior for a deleted thread root is observed.
