# iPhone Bottom Bar

The owner reviewed the installed iPhone build on 2026-11-01 and asked for
three changes, using T3 Code's mobile list as the model. They revise the 30 Oct
unified-list decision that put the word Settings in the iPhone bar's trailing
slot under a `Freeside` title (`devlog/2026-10-30-1427-unified-list-gate-answers.md`,
`devlog/2026-10-30-2010-iphone-stack-settings-sheet.md`).

## The List Draws No App Name

Chose to hide the list's top bar over keeping the inline title, because the
owner sees no reason to name the app inside it. The navigation title stays set,
so VoiceOver and a pushed page's back button still have it. Pushed pages keep
their bar.

## Settings and New Task Sit in a Bottom Bar

Chose a native `.bottomBar` toolbar, gear on the left and New Task on the right,
over a custom floating bar, because the system draws the same glass capsule on
current iOS and the app adds no control recipe. The bar has no overflow, and
review reproduced both recovery buttons running it off the edges, so recovery
takes a row above the list while any is pending, and the top bar goes entirely.

The filter stays in the tools row. The Mac keeps New Task there and Settings in
its toolbar: the owner's review covered the phone.

## The Advance Toggle Leaves the iPhone

Chose to remove the Decision Flow menu over moving its toggle into the Settings
sheet, because the owner doesn't think the iPhone needs it and wants the one
gear to mean Settings. The Mac keeps the toggle in its Settings window. The
preference stays in the shared code, so a phone that had turned it on stays on
with no control to turn it off; it defaults to off, and the owner's phone never
set it.

Revisit when a second iPhone surface needs a place in the bar, which would give
the capsule the tabs T3 Code's has.
