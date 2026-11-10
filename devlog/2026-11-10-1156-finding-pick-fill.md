# A Held Finding Pick Takes the Card's Fill

Decision record for #3011. The owner decided on 10 Nov 2026 to add a
clause to R6, the rule that names a decision card's one filled control.
The note is mandatory because the clause changes a recipe in
`app/SURFACES.md`.

## R6 Gains a Pick Clause

The recommendation's action is the filled control unless the operator
holds a pick, when the control that sends the picks is filled. On a
`finding_adjudication` card that draws its recommendation, a held route
pick moves the fill from the acceptance (Accept Recommended Route or
Accept All Dispositions) to Choose Another Route, and clearing every pick
moves it back. The card still draws exactly one filled control.

Chose this over leaving the fill on the acceptance because the acceptance
sends the proposed routes: the daemon gives every finding its proposed
route and the held pick is lost with no confirmation. The per-finding
notice ("Accepting does not send it.") warned about this in words while
the fill pointed the other way. The 4 Nov task page density note rejected
a sentence on this card for the same loss; this closes it on the button.

Rejected options, both non-goals in the issue contract:

- **A confirmation step on the acceptance while a pick is held.** It adds
  a dialog to a path the card already warns against, where moving the
  fill points the operator at the right control without one.
- **A count on either label.** The per-finding notices already say which
  findings carry a pick, and R32 keeps the route command's label fixed.

## Tone Changes, Position Stays

Only the tone swaps. The acceptance stays in the recommendation block and
Choose Another Route in the control row; they are separate views, so no
control moves. `DecisionCardComposition.recommendationIsFilled` decides the
block's tone, and the iPhone sticky footer that stands in for the block's
button reads the same answer, so a scrolled-away block never leaves a
second filled control on screen. A destructive recommendation keeps its
own tone. `review_dispute` is unchanged: its routed choices are peers, and
the card fills nothing.

## Return Follows the Fill

The issue assumed the card marks no default action, so only the fill
needed to move. On the Mac, though, Return and the Take Recommendation
command take the recommendation whenever it validates, so Return still
sent the acceptance and dropped a held pick. Chose to make Return take
the recommendation only while its button holds the fill
(`DecisionKeyboardGate`), over leaving the keyboard for a follow-up: the
loss is the one this unit closes, and a fill that points one way while
Return goes the other is worse than either alone. Return does not move to
Choose Another Route: Return takes only a recommendation. This is a
departure from the issue the owner can veto.

## The Golden Renders the Card Validated

The `alternative-selected` screenshot surfaces now render with a
validated `DecisionModel`, as the routed-dispute surfaces do. Without it
every control draws disabled, which drops the tone, so the golden showed
no fill before or after and could not pin the rule.

Revisit when another card type lets the operator hold a choice that its
recommendation's action would discard.
