# Filing Recovery Never Retries an Unproven Create

Work unit: #1441. Plan revision: 75.

Plan §5.17 said follow-up filing recovery "proves absence before retrying"
but named no mechanism. Revision 66 had already settled the same question for
the approved-specification comment (§5.11;
`devlog/2026-09-20-0903-spec-issue-comment.md`): GitHub offers no idempotency
key for creating a comment, so a dispatched create with no recorded response
can still commit after any settle interval, and a complete listing can lag.

## Finding

GitHub's REST endpoint for creating an issue also accepts no idempotency key,
request ID, or deduplication token (checked against the REST reference on
2026-09-30). So nothing can prove an unproven issue create failed, and the
§5.17 wording promised something no implementation could deliver.

## Chose the §5.11 Evidence Rule for Issue Creation

Chose to make §5.17 recovery follow §5.11: a create needs evidence that no
earlier create for the proposal instance committed (no dispatch-started
marker, or a recorded definite rejection). Every other dispatched attempt is
unproven; recovery waits the settle interval, lists again, and adopts a
single validating candidate. Two or more candidates, an incomplete listing,
or no candidate after the settle interval is residual ambiguity: a terminal
`ambiguous` outcome for the creation intent, recorded in the same step, and a
`system_health` item naming the repository and proposal. §5.17 now also
requires the dispatch-started marker, which the evidence rule depends on.

Rejected: keeping "proves absence" and leaving the mechanism to the
implementing unit. No mechanism exists, so the implementer would either
retry blindly (a duplicate filing, a fan-out effect) or invent this rule
without a plan decision.

## Chose to Release the Repository but Stop Adoption There

The ambiguous outcome is terminal, so it ends the repository's outstanding
intent and later filings there go ahead. The risk is a late commit: the
ambiguous create may land after the settle interval, and its issue passes a
later intent's candidate validation (same repository, same App authorship,
same candidate-visible fields; text markers are never matching keys). If the
later intent's own attempt is unproven and never committed, recovery would
adopt the stray and ledger it to the wrong proposal, and intake trusts that
ledger.

So once a repository holds an `ambiguous` filing outcome, later intents
there adopt no candidate: an unproven attempt is residual ambiguity at once,
and only a recorded success response ledgers an issue. The cost is more
attention items after an unproven attempt in that repository, which is rare.
A stray issue itself stays safe: it has no ledgered lineage, so intake forces
it to propose.

Rejected:

- **Block the repository until a human settles the ambiguous intent.** It is
  the tighter rule, but settling needs a new human action on the attention
  item, which is an API and client change and a non-goal of #1441.
- **Adopt as before.** It accepts wrong lineage in the ledger for a
  rare-but-reachable sequence, which §5.11 never faced: a wrongly adopted
  comment misattributes a comment, while a wrongly adopted issue enters the
  ledger intake trusts.

## Ledger States for #1626

The rule keys on the last attempt's recorded response, so the store needs a
record per attempt, not one state per intent. An attempt carries its
dispatch-started marker and, when one arrived, its response class: success,
transient rejection, definite rejection, or unproven (no response, or one
outside the implementing unit's rejection list). An intent is outstanding
until it reaches one terminal outcome: ledgered (a success response, or a
single candidate adopted after a dispatch), `refused` (a definite rejection,
or a transient one whose retry bound is spent), or `ambiguous`. Every
terminal outcome releases the repository. The per-repository "no adoption"
rule derives from the existence of an `ambiguous` outcome for that
repository; it needs no separate flag.

## Chose No Adoption Before the First Dispatch

Independent review found that the adoption stop covered only recovery, while
check-before-create could still adopt a late stray inside a new intent's
window. Under per-repository serialization, an intent that has not yet
dispatched has created nothing, so any candidate it sees is foreign. §5.17 now
lets only an intent with a dispatched attempt adopt, and the post-ambiguity
stop applies at every step.

## Chose to Exclude Pre-Dispatch Candidates

Codex review found that calling a pre-dispatch candidate foreign did not
keep it foreign: if the intent then dispatched and the create was rejected
or unproven, the same issue was the single candidate in the window and
would be adopted, corrupting proposal-instance lineage. The intent now
records the IDs of validating App-authored candidates seen before its first
dispatch, §5.11's pre-dispatch ID set, and a candidate must be in the intent
window and outside that set. Rejected: ending the intent as `ambiguous`
without dispatch when a pre-dispatch candidate exists (a new terminal path
that would also trigger the per-repository adoption stop); and deferring
the gap to its own issue (lineage corruption would stay reachable in the
plan). The coordinator chose the import; the owner confirms it in review.

## Left Open

§5.17 still bounds a candidate by the intent window, while §5.11 says no
clock defines a candidate. Dropping the window clock is candidate discovery,
not crash recovery, so it stays for the owner to decide on its own.

Revisit when GitHub offers an idempotency key for issue creation, or when a
human action to settle an ambiguous filing lands (it could then lift the
per-repository adoption stop).
