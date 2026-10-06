# External Finding Adjudication

Issue #1767 lets an external review cycle answer the findings that started
it. #524 ended every such cycle on a person
(`2026-10-05-0927-external-review-response.md`), and #1749 gave an admitted
external finding a durable outcome
(`2026-10-05-2255-external-finding-disposition.md`). This unit sends the
cycle's open external findings to the adjudicator in the cycle's first round,
records a declined or deferred outcome for each, and re-earns readiness when
every finding is answered. It changes `daemon/internal/engine`,
`daemon/internal/inference`, `daemon/internal/claudeinference`, and
`daemon/internal/integration`, and no shared package.

It does not remediate. The issue asks for remediation too; the section
"Remediation Is Not in This Unit" says why it stopped at the seam.

## The Issue's Decisions as Built

The owner stated six decisions on #1767. Four are built here.

1. **Adjudicate only at the admitted base.** A round goes to adjudication
   only when it is the cycle's first round and the cycle's base equals the
   base the predecessor's producer was admitted at
   (`externalReviewMayAdjudicate`). Any other cycle ends on a person as it
   did under #524.
3. **A reviewer's words reach a model only quoted, in a list of their own.**
   The adjudicator input gains `external_findings`. Each element carries the
   finding ID, the reviewer login, the thread ID, the head, the
   daemon-derived location, the message cut to 4096 bytes and quoted, and a
   fixed notice that the text is data. The external `domain.Finding` never
   appears in `findings`: the engine routes it to the quoting helper, and the
   inference client refuses one in `findings` before any model call. A unit
   test pins the exact bytes of one element.
4. **A cycle whose first review record missed its round ends on a person.**
   The round test is `record.Round == task.Successor.ReviewRound`, the round
   #1749's store gate requires for a disposition.
6. **An answered finding is not judged twice.** A finding with a disposition
   from an earlier round is left out of the batch and named on the card and
   on every item the cycle ends on as already answered. The trigger also
   reads dispositions, so an answered finding starts no second cycle.

Decision 2 (a `fixed` disposition names the remediated head's review record)
belongs to remediation and is not built. Decision 5 (no follow-up filing
from an external finding) is built as a skip in `planFollowUpFilings`.
Follow-up: #1787.

## Decisions Made in Implementation

The owner can veto each of these.

- **The cycle waits only while an adjudication card is open.** Chose to
  finish the cycle's task on every other ending that needs a person, with an
  attention item as the durable surface, over leaving the task pending as an
  ordinary round does. Nothing reads a dispute answer from a pending
  re-entered task, so pending would never end. The endings:
  - A dispute route, or a decline or defer that needs a second decision:
    the review-dispute item at the round's review identity.
  - A decided card whose routes are not all decline or defer, a remediate
    route, or a convergence stop: a handoff item at
    `production-external-review-handoff-<run>-<round>`. The round's review
    identity may already hold the decided card, and an item's type is bound
    when it is first written.
  - A card answered Stop: the task ends with no further item.
- **No adjudicator, or an unaccepted or unavailable batch, ends the cycle on
  a person** through the item #524 already raises.
- **An unrated external finding counts as high** for the rule that the
  adjudicator alone never declines or defers a critical or high finding
  (plan §7). A reviewer's severity is the badge on their comment, and most
  carry none.
- **The card binds the normalized message; the reason shows the quoted
  text.** The plan asked for a quoted `FindingMessage` in the card binding.
  The store re-derives the binding from the stored finding, and changing
  that is a `daemon/internal/store` gate change this unit does not need. The
  quoted, cut text appears in the card's reason, which is what a person
  reads.
- **No drift audit and no remediation preparation in an external first
  round.** The audit's verdict parks a round on an item that answers only
  the review record's findings.
- **Readiness in an external first round needs an outcome for every open
  external finding,** even when Freeside's own review of the round is clean.

## Remediation Is Not in This Unit

Chose to stop at the plan's step 8 seam and ask the owner (comment on #1767)
over widening the scope. Remediating an external finding needs changes the
issue's Scope does not name:

- `authenticateRemediationLineage` in `daemon/internal/signet` accepts only
  findings of the round's review record.
- `RecordProductionExecutionExport` has never recorded a remediation export
  from a re-entered task, and its source-tree checkpoint identity assumes a
  first-cycle task.
- Several gates key on `Outcome == ReviewFindings`, which a clean record
  with an external finding to fix does not satisfy.

Until remediation exists, a remediate route ends the cycle on a person.

## Refute-First Results

An independent reviewer, given only the diff and the intended outcome, tried
to prove the change wrong (`docs/agent-workflow.md` §refute-first).

Confirmed and fixed:

- **A cycle could start under an unfinished task.** A cycle's task ends a
  few records after its ready item. A finding left in between started the
  next cycle, and the unfinished task then came back to a round that had
  passed and raised a dispute item against findings already answered. The
  trigger now starts nothing until the task that wrote the ready item has
  ended.
- **A finding left after the round was judged held the round open.** The
  cycle's admitted findings are read live, so in that same window the
  unfinished task counted a finding it never judged and could not end. The
  round's stored adjudication now fixes the set the round answers; a later
  finding has no outcome and starts the next cycle.
- **A restart without an adjudicator wedged a cycle waiting on its card.**
  The round went back to the #524 ending and tried to write a dispute item
  under the card's identity on every pass. Whether an adjudicator is
  configured is no longer asked before adjudication: an unjudged round
  still ends on a person inside it, and a judged round keeps its card.

Allowed by decision:

- **Quoted reviewer text reaches the discussion model.** When a person
  presses Discuss on a review-dispute item, the item's reason is model
  input, and every item an external review cycle ends on names the
  findings there, quoted and cut to 256 bytes. That is not a list of its
  own. The path predates this unit (#524's item carries the same note);
  the site's prompt calls its input untrusted and its output is a reply a
  person reads. Follow-up: #1788.
- **A person with write access can edit a listed reviewer's comment**
  (#524's accepted limit). That text now reaches the adjudicator. The
  framing treats every external finding's text as untrusted whoever wrote
  it, and the adjudicator alone can only decline or defer a finding whose
  badge rates it below high.
- **An item can name a declined finding as open.** If the cycle's first
  round records outcomes and a reviewer-configuration change then moves the
  cycle to a later round, the cycle ends on a person and its item lists
  those findings as needing a decision. Nothing automatic reads the list.

Disproved by a check: a run reaching ready with an unanswered external
finding, an external finding in `findings`, the quote escaped or the limit
passed by multi-byte or control characters, an outcome keyed to the wrong
finding or round, a loop of cycles, a pending task nothing wakes after a
dispute, a stop, a remediate route, or a parked route, and a change in an
ordinary round's behavior from the shared refactors.

Not covered by a test: a process lost between a person-ending item and the
end of the cycle's task. No seam interrupts there. The replay computes the
same ending and writes the same item identity.

## Limits

- **The base gate has no end-to-end test of the mismatch.** The trigger
  refuses a head someone else pushed, and a base advance stops the cycle
  before review, so the harness cannot build a reviewing cycle on another
  base. A unit test pins the predicate, and an integration test pins that a
  cycle after a base advance judges nothing.
- **Prior dispositions sent to the adjudicator are review dispositions
  only.** An external finding's earlier outcome is not model input; it is
  named on the card.
- **A second decision's item text says "critical or high"** for a plain
  dispute route as well. The wording predates this unit.

## Revisit When

- Remediation of external findings is built. The seam is
  `externalReviewRoundEndsOnPerson`. The drift auditor's input lists every
  round's adjudication entries, and `store/drift_audit.go` refuses a
  reversal that names a finding outside the review record, so an audited
  round that follows an external first round must filter external entries
  first.
- A reviewing cycle on a base other than the admitted one becomes reachable,
  for example when project-image reuse (#1786) lets a producer be admitted
  at the cycle's base. Decision 1's equality may then become "the image
  serves the cycle's base".
- The comment-edit limit is closed by reading the editor through GraphQL.
  The framing here then stops being the only defense for edited text.
