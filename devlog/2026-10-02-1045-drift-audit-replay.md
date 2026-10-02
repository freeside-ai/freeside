# Drift Audit Replay: Evidence and the Route Recommendation

Work unit #1053, plan §7 "Review Drift". This note answers the "Revisit when"
in `2026-09-01-1246-review-drift-audit.md` (flip `drift_audit_route` to `park`
if reversal lists are wrong too often) and the input-size question in
`2026-10-01-1535-drift-audit-site.md`.

## Recommendation: Flip `drift_audit_route` to `park`

The agent recommends `park`; the owner decides, and the flip is its own plan
change. This unit changes no default. Follow-up: #1696.

The replay audited three merged pull requests the owner judged over-hardened
(#279, #960, #1520), once each, with `claude-opus-5-5` through the production
driver. Against the owner's marks:

- **Reversal lists: 4 of 6 proposed reversals were right.** All six came from
  #1520. The two wrong ones (`pr1520-r01-f1`, `pr1520-r06-f1`) would have
  undone fixes the owner wants kept.
- **Verdicts: 1 of 3 agreed.** The audit called #279 and #960 `converged`
  where the owner expected `over_hardened`, and so missed 10 fixes the owner
  would undo.
- **Automatic routes: 0 of 3.** #1520's list was valid, but every finding on
  it was P1, and the gate never routes a P0, P1, or unset-severity reversal
  without a human.

A list that is one-third wrong is too wrong to apply unattended. Severity was
the only gate condition that stopped #1520. Both wrong reversals are of
findings from earlier rounds (1 and 6), which a production audit of round 10
could cite just as the replay's did; had Codex badged them P2, no gate
condition would have refused them. Parking costs one human look at a round
that has already run long, and the human route still offers the
simplification round.

Rejected: keeping `auto` because the severity ceiling held. The ceiling
depends on the reviewer's badge, which is the same signal the audit exists to
second-guess. Rejected: recommending nothing until a larger sample. The
default acts unattended today, and the evidence that exists points one way.

## The Misses Matter Less Than the Wrong Reversals

A missed over-hardening leaves the run where it would be without the auditor.
A wrong reversal makes the candidate worse. So the 2 wrong reversals drive the
recommendation, and the 2 missed verdicts are a statement about how much the
auditor is worth, not about whether it is safe.

For #279 the miss has a known cause. That PR closes no issue, so the replay
gave the auditor the PR description as the specification, and the description
was edited during review to describe the hardening. The auditor read the
hardening as specified. A production run has an issue-backed specification
written before review, so #279 understates the auditor there. #960 has no such
excuse: its specification is issue #959, and the audit still called the
certificate-check rounds converged.

## Input Sizes Leave Room

The assembled inputs were 181,440 bytes (#279, 17 rounds), 208,976 (#960, 9
rounds), and 109,916 (#1520, 10 rounds), against a 2,097,152-byte limit. The
largest is 10% of the limit. These were chosen as long, heavily reviewed pull
requests, so the bound is not close for runs of this shape, and the fail-safe
path was not exercised.

## What the Evidence Cannot Show

- **Three pull requests, one audit each.** The rates are counts, not
  estimates. A second run of the same input may answer differently; the test
  keeps the first answer and refuses to replace it.
- **The baseline is the owner's sign-off on the agent's reading.** The owner
  recorded expected reversals before any audit ran (comment on #1053), but
  formed them from the agent's summaries of each finding and its
  recommendation, not from an independent read of the code.
- **The sample is all positives.** Every pull request was nominated as
  over-hardened, so the replay says nothing about how often the auditor calls
  a healthy run over-hardened.
- **The replay audits a round production never audits.** It audits the
  merged head as the round after the last one with findings, so every finding
  can be cited (the issue's contract: the question is whether the finished
  pull request ended over-built). Production audits only a round with
  findings and refuses a reversal of that round's own batch, so
  `pr1520-r10-f1` could not be on a production list, and the engine routes
  only when the audited batch has a fix to route. The gate answers in the
  golden are the store gate's on that synthetic round. They show which
  conditions held, not that a real run would have routed.
- **The replay's history differs from production's.** Production gives the
  auditor adjudication entries and a fixed reason for every `fixed`
  disposition. The replay has no adjudications and uses the first two
  sentences of the author's reply as each reason, which may tell the auditor
  more than production would.

## Fixture Choices

Chose the first two sentences of the first reply as a disposition's reason,
over the plan's first sentence, because #279's replies open with a bare
"Fixed in `<sha>`." and say what changed only in the second. The rule is
applied to all three fixtures.

Chose to record the owner's expectations as an issue comment, over the plan's
commit to the branch, because a commit with expectations and no recording
would fail the default test, and every commit stays green.

Chose a direct call to the site and the pure route gate over a run through
the engine (the issue's non-goal). The replay therefore fabricates remediation
IDs and adjudication digests to make valid disposition records; they carry no
evidence and are named as synthetic in the test.

## Revisit When

- Parked audits accumulate with owner decisions on them. Those are a larger,
  unselected sample with a real baseline; revisit `auto` if the owner accepts
  reversal lists unchanged at a rate they would trust unattended.
- The auditor's instruction or model changes. Run the live replay again and
  compare against the recorded answers before trusting the old counts.
- A production audit fails safe on input size. The replay did not come close.
