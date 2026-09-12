# Bring the Sidebar Controls Under the Design Language

Owner decision, delivered as the second revision of the 12 Sept 2026 Claude
Design chrome handoff ("design_handoff_freeside_chrome2", README §M2). It
adds a fourth client-only change, M2, with a live dusk screenshot
(`screenshot-2026-09-12-sidebar.png`) showing the seams. This note revises
the "What stays native" bullet of
`devlog/2026-09-12-0945-chrome-under-design-language.md`, which kept the
project `Picker(.menu)` and the segmented scope controls as system chrome;
that note is frozen, so the correction lives here. No daemon, API, or schema
change.

## Decisions

- **The section switcher and both scope controls become
  `FreesideSegmentedControl`.** The system `Picker(.segmented)` drew
  system-gray on both grounds and rendered its "Section" and "Scope" labels
  as leading words, so the sidebar read as the one surface still outside the
  §15 palette. Chose a Freeside control (ground container, rule border, the
  selected segment on its own `segmentSelected` fill, counts in the mono
  caption after the word, hover and focus in the accent, stacked from
  xxxLarge) over tinting the system control because a tinted picker cannot
  carry a count in the mono caption or the selected-segment fill, and still
  draws its leading label.
- **The project filter's trigger becomes a Freeside label; its popup list
  stays native.** Chose `FreesideMenuTriggerLabel` over a `Menu` (the
  current project or "All projects" with the up-down chevron, no "Project"
  word) over a full custom popup because the system list renders a long
  project list better than a custom view would. The popup list is the only
  piece of the revised bullet that survives on the System Chrome list, beside
  the "More actions ▾" and route-picker popups, context menus, the macOS
  window title, toolbar, and inspector toggle, and the non-destructive
  dialogs.
- **Fact values right-align.** On macOS `LabeledContent` set each fact value
  inline after its label, so the operational summary never lined up. Chose an
  explicit label, spacer, and right-aligned value in `FactRow`'s trailing
  branch on both platforms; the 40-character stacking rule is untouched.

## Deviations From the Handoff

- The disabled-while-loading state is not wired to `store.loadState`: the
  pickers it replaces were never disabled, and the store reads `idle` with
  cached rows on a cold start, so wiring it would lock the control exactly
  when the operator most wants to switch scope. The control keeps the
  disabled style for a future caller.
- Segment labels scale down to 0.8 before truncating: at the sidebar's 280pt
  minimum, "Resolved" with its count and the urgent chip do not fit three-up
  at full size.
- The section switcher drops the Picker's SF Symbols; the handoff shows words
  only.
- The stacked count is not the mono caption: inline, the count sits in the
  mono caption after the word ("Open 6"); from xxxLarge the handoff specifies
  the phrase "Open · 6", and its stacked mock draws that phrase as one sans
  run, so the control does too.

Revisit when: a control on the remaining System Chrome list gains Freeside
vocabulary of its own, or the sidebar minimum width changes.
