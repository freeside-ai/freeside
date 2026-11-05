# Usage Capacity Cards

#2579 redraws the Usage pane as one card per pool with a capacity block per
window, from the 4 Nov handoff
(`docs/design/handoff-2026-11-04-settings-usage-new-task/`). The pane
matches the canvas's band 2 frames `Usage dusk` and `Usage day`, and the
block matches `R32 states`. The composer and role chips (R33) are recipes
only; #2581 composes them. This note keeps the choices the canvas leaves
open or that depart from it.

## What the Card Leaves Out

Chose to draw neither the sub-cap share nor the parked count, over drawing
them from values the client lacks. The reasons, and who owns each, are in
`devlog/2026-11-04-1251-settings-usage-gate-answers.md` (Values Sync
Lacks): no field carries the sub-cap, and the parked count waits on #2578
and #2583.

## What a Block Says

- **What is left, not what is used.** Chose `N% left` over the used share
  the old rows drew, because the canvas reads every window that way and a
  meter that fills with what remains empties as the window runs out.
- **A reading with no percent draws no meter** unless it is used up. The
  owner decided this on 2026-11-04; the reason is in the gate-answers
  note (Wording).
- **Every used-up window draws a meter.** Chose an empty track in a wax rim
  for each used-up window, including a mark-only window and a reading with
  no percent, over drawing it only where a percent exists. Used up means
  nothing is left in any unit, so the meter states no share the record
  doesn't give.
- **A used-up window resets with its last live cause.** Chose the latest
  `resets_at` among an exhausted reading and the window's live marks, and
  no reset line when any live cause has none, over the reading's own reset.
  The window stays used up until every cause lapses. The default fixture
  shows the gap: `seven_day_opus`'s reading resets in about 23 hours, but
  its refusal mark holds for about 47.
- **A reset on today's weekday names its date.** Chose `shortTime` when a
  reset under seven days out falls on today's weekday, over the contract's
  weekday for everything from one day to seven. A weekly window that has
  just rolled over resets on the same weekday next week, and `Resets Wed
  2:39 AM` read on a Wednesday at 2:40 says a minute ago. The fresh-context
  pre-push review found it.
- **The fill is `accentText`, not the canvas's accent.** It is the fill the
  pressed button already uses, and it clears the 3:1 non-text minimum on
  the ground-3 track in every cut, which `DesignContrastTests` now pins
  beside the wax fill (the wax pair also covers the used-up rim).

## The Card's Foot and Grouping

- **Refresh and Collectors stay per card.** Chose a `Collectors` fold and a
  Refresh button on each card over the canvas's page-level ones, because
  `refresh_usage` names one pool and one usage command is pending at a
  time (`AccountUsageModel.canSend`). A page-level Refresh needs a
  multi-pool command path the model doesn't have, and this unit leaves the
  model unchanged.
- **The provider keyword is the record's id** (`anthropic`, `openai`).
  Chose it over a name map to the canvas's `Claude` and `Codex`, which are
  the design's words: no record carries a display name, and a client-side
  map would be one more list to keep in step with the daemon.

## Recipes

- **The role keyword draws at the floor.** Chose the ladder's keyword size
  (11pt on the Mac, 11.5pt on iPhone) over the canvas's 10pt, because
  nothing draws under the floor (R10).
- **The meter draws through its own gauge style.** `ImageRenderer`, which
  the screenshot suite draws through, renders the built-in
  `.linearCapacity` gauge style as the system's unsupported placeholder,
  while a custom `GaugeStyle` draws its track, fill, and rim.
  `CapacityGaugeStyle` exists for that reason, and `CapacityMeterTests`
  reads its pixels in every cut. The gauge's labels draw nothing under the
  custom style; the block combines its children for VoiceOver and hides the
  meter, whose value the block's text already says.

## Faint Text on the Floor

Two lines the contract draws faint sit at the disabled-text floor (3:1),
not the 4.5:1 text floor:

- The band's attempt line (`read 2:40 AM`): `inkFaint` on ground-3 is
  3.53:1 in day and 3.84:1 in dusk.
- The role chip's keyword over `Default`: `inkFaint` on ground-2 is 4.31:1
  in day and 4.43:1 in dusk.

Chose to keep both faint, as the contract and canvas draw them, over
lifting them to dim. No floor was relaxed. Both say something stated
elsewhere (the attempt's time is on hover, and `Default` itself draws in
dim), but neither is a disabled control, so the owner may want them at
4.5:1.

Revisit when the API gains a sub-cap field or #2578 merges (the card then
gains the share or the parked count), or when a multi-pool refresh command
exists (Refresh can then move to the page).
