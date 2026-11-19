# No Fill Without A Recommendation

**Decision (owner, 18 Nov 2026, #3359 exit box 1):** a decision card fills
an action only when it has a recommendation. A card with no recommendation
draws every action in its row outlined. This reverses the forward-action
fallback in R6 (`docs/design/handoff-2026-10-06-refined-interfaces/README.md`)
and in "One Rule Names The Filled Action"
(`devlog/2026-10-08-1028-refined-decision-cards.md`). The handoff README is a
dated record and stays as it was; this note supersedes its fallback.

Chose no fill over the type's forward action (Answer and Retry, View PR,
Retry, Start, Approve on an effect proposal, Acknowledge) because a filled
button reads as advice, and nothing has judged those actions. On a quota
failure, Retry was filled even when switching to another agent may be the
better move. The owner decided from the enabled renders #3361 recorded.

The rest of R6 holds:

- **A recommendation keeps the fill,** in its own block, and a held route
  pick on a finding adjudication still moves it to Choose Another Route.
- **A stale final review's row fills nothing,** and a destructive
  recommendation takes the wax outline instead of the fill.
- **The keyboard already matched.** Return sends only a filled
  recommendation (`DecisionKeyboardGate.canTakeRecommendation`), never the
  fallback fill, so it doesn't change.

`DecisionCardComposition.forwardAction` goes with the fallback, since the
fill was its only use. Its mapping test goes as `accepted: the rule it pinned
is retired`.
