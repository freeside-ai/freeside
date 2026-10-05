# Follow-Up Filing Ledger

Issue #1626 adds the daemon's record of the follow-up issues it files and of
every attempt to file one (plan §5.17, follow-up filing recovery). It is a
contract unit: `daemon/internal/domain`, migration 0089, and the store move
together. Nothing calls the ledger yet. #1634 files issues through it and
#1635 reads it at intake.

## Owner Decisions Carried From Planning

The implementation plan on the issue put five positions to the owner and
asked for an objection before work started. The owner started implementation
by fiat without changing any of them. Reading that as acceptance is the
agent's interpretation, not a recorded statement.

1. **One unit, about twice the size budget, over a split.** The split would
   have kept the intent and its attempts here and moved the filed-issue row
   to a second contract unit. Rejected because ledgering is one transaction
   across all three tables and rebuilding a ledger row reads the intent and
   its attempts: the first half would ship a `ledgered` outcome nothing can
   reach, and the second would add a migration with its own pass through the
   exclusion lists.
2. **An issue is repository ID plus issue number,** over GitHub's global
   issue ID. Intake and `domain.IssueSubjectRef` identify issues this way,
   and it is how the contract reads §5.17's "canonical numeric issue ID under
   the canonical repository ID". The global ID is not stored.
3. **Origin is the project and run the proposal's subject resolves to.** The
   ledger row stores them, and every read re-derives them and refuses a
   mismatch.
4. **Three refusal reasons.** `definite_rejection` and `retry_bound_spent`
   come from §5.17. `precondition_failed` is new: #1634 re-screens before
   each create, and a screen that fails after the intent is open needs a
   terminal outcome or the repository stays blocked. #1634 had no plan when
   this was written, so the third reason is still a guess at its needs.
5. **Dedicated tables,** over the outbox and inbox ledger that revision 66
   used for the approved-specification comment
   (`devlog/2026-09-20-0903-spec-issue-comment.md`, decision 8). Filing
   differs: one outstanding intent per repository is a schema constraint,
   and intake looks rows up by repository and issue number at every
   observation.

## The Store Enforces the Evidence Rule

Chose to enforce §5.17's rule in the store over leaving it to #1634's
dispatcher. The rule is a pure transition function on the intent in
`domain`; each store write reads the intent, applies one transition, and
persists that transition's delta. A caller cannot start a second create
after an unproven one, refuse while the last attempt is unproven, or adopt
before a dispatch, whatever its own logic does.

Rejected: a store that records what it is told and a dispatcher that checks.
The rule exists for the crash between a create and its record, which is
where the dispatcher's in-memory view is least reliable, and a second caller
would have to reimplement it.

The same function validates on read. An intent whose rows describe a history
no transition sequence produces fails reconstruction, so rows changed outside
the store are not read as history.

## Writes Are Strict Transitions

Chose to refuse a repeat of a write already made, over accepting an
identical replay. Only opening an intent is idempotent: a repeat returns the
stored intent at whatever state it has reached.

A replay of "start attempt" is indistinguishable from a second attempt, and
a second attempt is the thing the ledger exists to prevent. Making some
writes replayable and not others would leave each caller to remember which.
Recovery instead opens (a read, if the intent exists) and continues from the
state it finds.

## Storage Shape

- **Intents and attempts are columns only; the ledger row has a body.**
  Intent and attempt rows are updated in place as the pre-dispatch set, the
  response, and the outcome arrive, and the one-outstanding-per-repository
  index needs the outcome as a column. A JSON body beside those columns
  would be a second representation to keep in step on every update. A ledger
  row is written once, so it follows 0087: the canonical body is the
  authority and the key columns are cross-checked against it.
- **No member `CHECK` on the outcome, response class, or refusal reason.**
  Widening one means rebuilding the table, as 0088 had to. The domain types
  validate on write and read. The schema names only the one member each
  column pairing depends on.
- **No `INSERT` triggers.** A restore reinserts every table wholesale in an
  order that puts these children before their parents, so a trigger that
  checked a parent on insert would fail every restore.
- **The terminal outcome does not carry the issue number.** The ledger row
  and a success response are the facts. Reconstruction requires the ledger
  row to exist exactly when the outcome is `ledgered`.

## Reads Re-Gate, and Cannot Be Wedged by Ordinary Work

Every read rebuilds from rows and re-derives what the rows claim: the
instance is still an approved follow-up filing, its derived target
repository is the intent's, and a ledger row is backed by a ledgered intent.
A row ledgered from a success response must name the issue that response
recorded; an adopted row must name an issue outside the pre-dispatch set,
which is all the intent records about an adoption. The approval is re-tied
to its authoring command and rendered digest, as `humanClosureApproval` does
for a closure, so a decline row rewritten to approve does not open an
intent.

#1625 found that a read gate on state ordinary work moves made every
attention item unreadable (`devlog/2026-10-05-0921-follow-up-filing-kind.md`).
The rows this gate reads are write-once (the proposal instance, its decision
row, the deciding command, the item binding) or immutable where it matters
(a project's repository identity, a run's pinned policy). A read fails only
for a real inconsistency.

A missing or unapproved backing row is reported as an inconsistency with the
cause as text, so `ErrNotFound` and "not approved" cannot reach a caller that
treats them as "nothing recorded".

**Known limit, accepted.** Reads select by key column. An intent whose
repository column was changed outside the store is invisible to reads keyed
on its old repository, though every read that selects it fails. Rejected:
scanning every intent on every repository read, which #1635 would pay at
each intake observation to cover a state the triggers already refuse.

**Consequence for #1635.** A ledger read that errors must demote to propose,
never fall through to `auto_start`.

## Refute-First Findings

Recorded per `docs/agent-workflow.md` §refute-first. A fresh-context reviewer
was asked to break the change before the first commit.

- **Confirmed, fixed: ledgering refused a caller time outside UTC.** Every
  other write normalized its time; the two ledgering writes passed it to the
  ledger row unchanged, and the row's validation refused it. Ledgering
  follows a create that cannot be undone, so a refusal there leaves the
  attempt unproven. Both writes now normalize, with a test in a fixed zone.
- **Confirmed, fixed: the instance-keyed ledger read answered "none" for a
  ledgered intent whose row was gone.** The intent read and the
  per-repository reads errored on the same state. With no row, the read now
  answers through the intent, and the tamper test covers it.
- **Allowed by decision: an intent whose attempts all recorded a rejection
  can still adopt, or end `ambiguous`.** The implementation plan states that
  the ledger does not make adoption stricter than §5.17, which requires only
  a dispatched attempt. The reviewer's objection stands as written: such an
  intent has evidence it created nothing, so by §5.17's own reasoning a
  candidate it adopts is foreign. Candidate validation is #1634's, and the
  first Revisit When entry carries the question.
- **Allowed by decision: an adopted row's issue number rewritten in its key
  column and body together still reads as valid.** Nothing but the row
  records which issue an adoption chose (Storage Shape). The update trigger
  refuses the rewrite; reaching it needs the triggers dropped. Rejected:
  copying the number onto the intent, a second representation the same
  writer could rewrite in the same pass.

Tried and not broken: a second create, or a refusal, while an earlier create
may have committed (the transition-reachable and `Validate`-accepted state
sets were enumerated to three attempts and are identical); `ErrNotFound` or
"not approved" leaking from an inconsistent row; the restore trigger
constants against the migration, byte for byte; a `CHECK` or trigger
blocking a store write; nil against empty pre-dispatch sets and unsorted or
duplicate input; and the claim that the approval re-gate reads only
write-once or immutable rows.

## Revisit When

- **#1634 lists the responses that show rejection before creation.**
  Adoption and the `ambiguous` outcome need only a dispatched attempt, the
  literal reading of §5.17. If they should need the last attempt to be
  unproven, tighten the transitions. The same function validates on read, so
  tightening it after rows exist makes rows recorded under the looser rule
  unreadable; tighten before #1634 writes any, or on the write side only.
- **#1634's refusal paths do not fit the three reason classes.** Adding a
  reason needs no migration.
- **`effect_proposal_instances` is rebuilt again.** `follow_up_filing_intents`
  references it and leaves deferred violations until the parent rows are
  reinserted, as 0077's and 0088's dependents do.
- **Per-repository reads become costly.** Each one reconstructs every intent
  the repository has ever had.
