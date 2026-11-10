# Wave 9 Exit Review: Findings That Change Direction

The §11 adversarial review of Wave 9 (tracker #2386) audited `main` at
`a769f16db153a8ba3a6f542f3446ed2cd0bf67dc` and filed #2951 through #2969. The
findings summary on #2386 holds the evidence and the risk dispositions. This
note records the findings that change what earlier notes or issues assumed,
and the reviewer's four recommendations. Each is the reviewer's (an agent
session the owner ran); none is an owner decision until the owner adopts it
on the tracker.

## Recommendations

- **Hold the exit run (#2395) until #2951 lands.** Chose this over running
  the exit run and waiving two of its requirements. The Exit asks the
  operator to move a parked task with the alternate-agent card, and to see
  the retry card on a quota failure with no known reset, with no real
  allowance spent (revision 99). Only the scenario driver reports a writer's
  quota failure. The scenario daemon has one subscription and answers the
  move and the alternate-agent commands with a fail-closed 500, and no
  scenario fails without a reset. So neither can be shown today. Rejected:
  draining a subscription, which revision 99 forbids and which would not
  park anyway, since no real driver classifies a quota refusal (#2957). The
  other option is the owner's: narrow the two requirements to what the
  engine tests already prove.
- **Fix the memory reservation before raising concurrency, not before
  exit.** `2026-11-03-0946-memory-reservation-at-admission.md` assumed that
  every verification job reserves memory and that "a pursued launch
  refreshes it once a minute". Neither holds. Re-entry and handoff
  verification reserve nothing (#2952). A publication-lane wait isn't
  refreshed while the lane works on another task, so it lapses after five
  minutes (#2953). A clean retry releases a reservation while an earlier
  room is unproven (#2954). Chose deferral over an exit blocker because the
  exit run shows a capacity hold on one task's launches. It doesn't depend
  on the order of waits or on re-entry rooms being counted.
- **Separate "the proof could not run" from "the adapter failed".** The
  Claude adapter proof runs on the lineup implementer's subscription and
  writes one record for the adapter. When that generation is marked, the
  pass records a failed proof and admission refuses every Claude writer on
  every enrollment (#2955), against #1740's rule that a mark holds only its
  own identity's admissions. Recommend the fix before a second subscription
  is relied on as cover for a marked one. The exit run meets it if the
  lineup implementer's generation is marked during the run, or, by trace
  only, if the proof fails for a reason specific to that subscription.
- **Track the real quota classifier.** #2421 left it out on purpose and no
  issue picked it up, so the park in plan §4 doesn't happen on a real run.
  #2957 now tracks it. The decision to prove the park by injection stands.

## What Did Not Change

No finding grants authority, leaks a credential, or loses data. The task-line
anchors, the alternate-agent retry's record and re-gate, the review slot
proof, the setup-token handling, and the client receipt checks held under
trace and their refusal tests.

Revisit when the owner dispositions the findings on #2386: a recommendation
the owner declines there is superseded by that record.
