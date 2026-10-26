# External Review Re-Entry: A Task Event, Not an Alert

Work unit: #2350. Mandatory note: it adds a `TaskEventKind` and a
`TaskEvent` detail to the API contract.

## Decision: Re-Entry Raises No Item or Push of Its Own

Chose to show an external review re-entry only on the task timeline, over
raising an attention item or push for it. Planning proposed this and the
owner may veto it; a veto makes the alert its own unit. Plan §1B.1 says
external review re-enters asynchronously and is "never a readiness
prerequisite". The alert is the item the re-entered cycle raises: a
`review_dispute`, a fresh ready card, or a remediation result. The
2026-10-26 exit run on #1613 passed the Wave 8 "external review alerts"
gate this way, through the `review_dispute` push about four minutes after
the review. A second alert for the re-entry itself would repeat that one.

## Decision: One Event per External Supersession, at the Card's Conclusion

The event is `external_review_reentered`, with `external_review` detail:
the superseded ready card, the triggering finding, the reviewer's login,
the new review round, and the published head the review was on. It reads
only records the re-entry already wrote: the `external_review` successor
on the publication chain, the predecessor card's supersession conclusion,
the finding, and the card's PR binding. No new persistence log.

- **Instant:** the predecessor card's `ConcludedAt`, over the successor's
  creation or the finding's observation. The conclusion is the moment the
  card stopped being ready, which is what the timeline reader needs; the
  other two can precede it by a reconcile pass.
- **Finding read ungated:** `GetFinding`, over
  `GetAdmittedExternalFinding`, which re-gates the finding against the
  current trust profile. The event records what already happened: the
  re-entry was admitted under the profile active then. Re-gating would
  erase it from history once the owner removes that reviewer from the
  profile. A test pins that the event survives such a change. The event
  carries the reviewer's login and head, never the finding body.
- **Legacy records skip, not fail:** a predecessor with no conclusion
  draws no event. Conclusions shipped 2026-10-21 and external re-entry on
  2026-10-02, so a re-entry recorded between them has none, and the
  timeline cannot invent the instant.
- **Fail closed on mismatched records:** a missing or foreign predecessor,
  a conclusion other than supersession, a finding with no external detail
  or from another run, and a PR binding from another run each fail the
  read as an integrity error, over dropping the event, because each means
  the successor chain disagrees with the records it links.

## Decision: The Task Row and Header Read One Stage

The task row's context line read the run's workflow phase, which has no
Review stage, while the detail header read the timeline's rail, so after a
re-entry the row said Verification and the header said Review. Chose one
function, `TaskDisplay.currentStage`, that both call, over patching the
row's phase: the rail's newest start names the stage, and the run's
publication position says Verification only when its publication is newer
than that start. A publication the re-entry withdrew is older than the
Review start the re-entry drew, so it no longer wins. Review's round comes
from the newest re-entry or round request.

Revisit when: an owner veto asks for a re-entry alert.

Follow-up: #2363 records the re-entry in the PR body's Disposition History.
