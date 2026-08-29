# Publication Reevaluation Rebuilds Authority From Durable Records

Issue: #419

## Decision

Chose a signet-owned, command-keyed outbox intent for production publication
reevaluation. The accepting transaction records the command, resolves the
displayed `publish_blocked` item, and enqueues the intent together. The engine
treats the payload as pointers only. It re-reads the immutable command, the
resolved item, and the original dispatched publication task before it verifies
or publishes anything. A disagreement stays pending and fails the reconcile
pass without writing a quarantine item. This follows the issue's authoritative
no-store-effect acceptance criterion, which is stricter than the implementation
plan's proposed reuse of the engine-owned task quarantine path.

Chose the latest explicitly activated trust profile only after an accepted
`rerun_trust_evaluation` command. The original publication attempt remains
bound to its admitted profile. A profile repair by itself cannot restart work
because the engine scans command-backed reevaluation intents, not repaired
profiles. This preserves the admission record while making the operator's
explicit rerun evaluate the repaired authority.

Each command gets a fresh verification invocation, checkpoint key, and blocked
item identity. This is required because candidate authorizations are
write-once and unique per repository, head, and trust profile. A repaired
profile can therefore record new authorization without changing the earlier
checkpoint or authorization. A reevaluation that blocks again creates a new
item under the command identity instead of trying to advance a terminal item.
Command acceptance rechecks that the latest activated profile has no existing
authorization for the blocked head. If the profile has not changed, the whole
accepting transaction rejects and leaves the item open; resolving the item and
letting the engine discover the uniqueness conflict would strand the intent.
The intent also pins that accepted profile digest and the next review round.
Recovery loads the exact profile instead of rereading mutable current state,
and review reconciliation ignores only records and failures older than the
pinned round. Fresh verification evidence therefore receives a fresh review,
while a later profile activation cannot invalidate a durable checkpoint.
Configuration failures on that path also start at the pinned review round;
otherwise an existing earlier review can make failure recording conflict.
The run coordinate in the new signet identities is canonical base64url rather
than a raw path segment because run IDs permit `/`; strict decoding and a
round-trip check reject aliases before an identity is trusted.

Normal definitive production blocks now offer
`[rerun_trust_evaluation, inspect_trust_failure, stop]`. The shadow-review
`stop` block keeps `[inspect_trust_failure, stop]` because it records an
operator refusal that trust reevaluation cannot change. Existing two-action
rows remain valid and are never rewritten.

Recipe revocation after an authorizing checkpoint is committed work, not a
new definitive trust refusal. It takes the existing durable recipe-revoked
hold and retries through the held-task pace. Restoring approval resumes the
same checkpoint without another verification or authorization. This matches
#527 decision 2: a recovery-time recipe gate pauses the affected run without
discarding durable progress or stopping the reconcile lane. Recipe revocation
before a checkpoint remains a definitive three-action block because no
authorization exists and a later profile repair can run fresh verification.

The append-only `publication_blocked` milestone remains first-observation-wins
per identity. Each reevaluation's block carries its own command-keyed
invocation (`publish-production-reevaluation/<encoded run>/<command>`): the
store keeps one milestone per (run, kind, invocation), so a rerun on the shared
publication invocation recorded no block at all, the domain conclusion kept the
original reason, and the hold taken while the rerun was queued never cleared.
Signet accepts that identity only for a command whose reevaluated item the same
pass authenticated. `publication_ready` keeps the plain publication invocation
because every ready authority binds to it and a run converges on one PR.
One signet classifier now re-reads the accepted rerun command, immutable item
chain, durable intent, project, and profile before it refines the domain's
blocked-first conclusion. Terminal inference from review and item combinations
was rejected after the same records proved reachable while work was still
live. Instead, each reevaluation terminal writer records one command-keyed
`production_publication_reevaluation_completed` outbox row in the same visible
transaction as the production terminal record. The marker starts dispatched,
so no delivery scan or pending-only promotion can touch it. Its payload names
the outcome, accepted intent, head, terminal invocation, and evidence item and
version. Signet treats those fields only as pointers: it strictly decodes the
marker, reconstructs the command and intent authority, and re-reads both the
named item and terminal record before using the explicit outcome. A missing
marker is live; a marker is terminal; a materialized result without its marker
is an integrity failure. There is no backfill because no reevaluation row can
exist before this branch first ships.

GetRun, ListRuns, follow, supervision snapshots, resume, both production retry
gates, and intake WIP counting use that same conclusion. The generic domain
classifier stays fail-closed where milestone-only callers cannot authenticate
item resolution. A later block after readiness is a contradictory history and
fails the signet projection rather than producing a published run whose latest
milestone says blocked.

## Rejected Options

- A domain-owned intent constant or blocked-item helper would turn this into a
  shared-contract unit and widen the change without another consumer.
- A new milestone kind or migration would duplicate attempt detail that the
  command-keyed checkpoint and item identities already preserve.
- Raw run IDs in slash-delimited intent and item keys would not be invertible
  for every valid run ID and could create ambiguous authority coordinates.
- Rescanning dispatched publication tasks when a profile changes would restart
  work without the operator command and violate the explicit-decision gate.
- Reusing the admission-pinned profile would make a profile repair impossible
  to evaluate. Reusing the original verification identity would conflict with
  write-once evidence and authorization records.
- Reusing an earlier authorization after a same-profile verification would
  falsely bind the new checkpoint to evidence and an invocation it did not
  produce. Same-profile commands are therefore rejected before resolution.
- Offering rerun on the shadow-review `stop` block would present a control that
  cannot reverse the operator's refusal.
- Making checkpoint-time recipe revocation a terminal
  `[inspect_trust_failure, stop]` block would preserve the dead end after the
  operator restores approval. Allowing a same-profile reevaluation to reuse
  the checkpoint would instead conflict with fresh command-keyed verification
  and write-once candidate authorization. The durable hold resumes the already
  committed checkpoint and needs neither exception.
- Letting `publication_ready` globally outrank `publication_blocked` in the
  domain conclusion would trust an unauthenticated contradictory timeline.
  Only the signet projection has proved that every definitive block was
  resolved by rerun, so only that authenticated boundary refines the outcome.
- A separate contract PR adding a resolution milestone to the domain would be
  larger, serialized behind the active contract chain, and leave #1010's
  reachable operator projections contradictory meanwhile. The command and
  item records already carry the resolution authority the shared classifier
  needs without a domain or schema change.
- A separate command-bound authority record would duplicate the same data
  inside the same SQLite trust domain. A writer that can rewrite the outbox can
  also rewrite a second command-keyed row or activate another profile. The
  payload instead names an operator-recorded, content-addressed profile that
  the consumer re-reads, binds to the repository, and uses for fresh
  verification and authorization; the accepting transaction writes that
  payload at the command's durability class. Forged rounds fail closed against
  immutable review state or advance exhaustion, and a forged digest can select
  only another operator-activated profile. The migration and serialized
  contract cost is therefore unjustified. Revisit if these database fields
  gain different writers or trust domains, or if the harm ceiling changes.
- Treating the reevaluation intent's dispatch status as completion would turn
  rescan bookkeeping into authority while still leaving the outcome to
  inference. The separate completion marker records a new lifecycle fact that
  no existing row carries, written by the code that makes the transition
  terminal. This does not reopen the declined #1013 design: that proposal
  duplicated acceptance-time profile and round fields that the consumer could
  already re-read inside the same SQLite trust domain.

## Refute-First Findings

The accepting-transaction tests prove that a conflicting intent rolls back the
command and item resolution. Consumer tests forge the payload, action, item
state, and task status and confirm that reconstruction and the consuming
reconcile path are read-only and fail closed. The refute pass also found that
routing every rerun through the new production outcome would alter the
fake-publication non-goal; non-production items therefore retain their prior
conclude-only behavior. It also found and corrected identity aliases, incomplete
action and artifact bindings, missing command ancestry, an attended-mode write
before reconstruction, and missing crash-after-block coverage. A final fresh
pass found no remaining actionable issue. The integration matrix proves that
profile repair alone does nothing, checkpoint recovery does not re-verify,
repeated blocking uses a fresh item, and ready-after-blocked authenticates only
after rerun resolution.

A later recurrence refute pass found no reachable defect in the checkpointed
recipe-revocation hold, but found that its first regression changed only the
engine's recipe set. The corrected test reopens the store with revoked policy,
then reopens it again after approval returns. It proves that recovery crosses
the real store trust boundary while reusing the same verification and
authorization.

The final consumer sweep found that the app/API projection had been corrected
while follow, supervision, resume, production reattempt, retry-parent replay,
and intake WIP counting still used milestone-only finality. Those reachable
consumers now share the authenticated classifier. The store-local generic
count remains milestone-only but has no production caller; the intake command
owns the resolution-aware count under the same write lock. The same sweep
found and closed the ready-before-later-block ordering gap.

The fresh-context refute found three classifier intersections. A valid
reevaluation hold uses the deterministic successor item with a one-action
inspection surface, so the classifier now treats an authenticated open hold
as live and its superseded form as recovered. Closed legacy two-action blocks
remain final blocked instead of becoming integrity errors. Finally, a pending
or dispatched reevaluation intent may legitimately have no successor while
work is live; only the atomic completion marker names the terminal transition.
Regression coverage pins all three cases, including recipe
revocation after a reevaluation checkpoint and recovery without another
verification or authorization.

The completion-marker refute pass removed the review-escalation terminal-class
list entirely. A live dispute and a terminal escalation can write the same
review and attention records, so comparing their combinations cannot prove the
lifecycle transition. The terminal writer now records that otherwise-missing
fact atomically and the classifier verifies it against the accepted command,
named item, head, item version, run, and terminal record.

## Follow-Up

The attended fake-publication lane still offers rerun without a consumer. Issue
#1007 owns that separate lifecycle.

## Revisit When

Revisit if profile selection becomes an explicit command payload, candidate
authorization uniqueness changes, or a shared domain identity is needed by a
third consumer. If a second lifecycle-marker kind is needed, promote the
pattern to a ledger table or milestone kind as a separate contract unit rather
than growing the outbox into one by accretion.
