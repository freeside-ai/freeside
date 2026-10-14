# Accept Each Adjudicated Finding Independently

The owner decided in #1990 that one unaccepted answer should not discard the
other answers, and that each declined external finding should get its own
reply. Chose per-finding acceptance over the former batch confidence gate.
Malformed or incomplete answer sets still fail as a batch: the stored artifact
requires one valid entry per finding, and the production client returns a
complete set or an error. No synthetic answer is invented for missing output.

A valid below-threshold required answer becomes a model entry with unknown
compatibility and a parking route, preserving confidence and rationale. Keeping
its original remediation route would let remediation selection fix work the
engine never accepted. Other goals keep their entry, but the execution gate
holds it for the person.

The round raises one routed dispute for every below-threshold, disputed, or
severity-held finding, plus accepted parking routes in that round. Accepted
declines and defers below the severity ceiling are recorded before the dispute
is raised, which lets their external replies proceed. Recorded outcomes are
final and retain their original adjudication and timestamp after the human
answer. The full batch remains bound even when early external outcomes remove
findings from the live open-findings list.

This narrows the older decision in
[mixed adjudication parking](2026-10-13-1314-mixed-adjudication-parking.md): a
dispute no longer stops accepted declines and defers from being recorded.
It preserves that decision's remediation hold. No fix starts while the dispute
is open; one dispatch combines accepted fixes with the person's choices.
Fixing first was rejected because a later human answer could require a second
remediation in a round that permits only one.

Rejected putting low-confidence answers on the ordinary adjudication card,
because accepting a parked route supplies no continuation. Rejected listing
the whole batch on the dispute, because it would ask again about findings
already declined and replied to. The subset contract from #2009 preserves
omitted entries while requiring an answer for every listed finding.

Rounds with no dispute retain their existing card and disposition timing.
Recording a decline before that card is answered would remove the operator's
alternative route choice. Discuss revisions retain whole-batch acceptance:
low-confidence answers now enter the routed dispute, so they no longer need
a per-answer Discuss revision to resume.

Verification covers a reply posted through a local forge fixture while the
dispute is open, preservation across restart, and one remediation containing
both accepted and human-selected fixes on the original pull request. Engine
regressions cover mixed confidence, critical/high ceilings, unchanged stored
outcomes after a later decision, and conversion of required low-confidence
answers before storage.

Independent refutation found no actionable defect. It traced the severity and
confidence gates before early outcomes, the human-decision barrier before
remediation, and the stored batch binding after an external decline leaves the
open list. It also checked that deferred follow-up proposals commit with the
disposition and that a human successor cannot recreate them under a new digest.
The reviewer inspected code and test coverage; it did not rerun the tests.

Revisit when the adjudicator can return a partial answer set, the ordinary
card gains per-finding decisions, or a round permits multiple remediations.
