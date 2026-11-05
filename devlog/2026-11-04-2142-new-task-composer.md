# New Task Composer

#2581 redraws New Task as the composer the 4 Nov handoff draws
(`docs/design/handoff-2026-11-04-settings-usage-new-task/`, canvas band 3).
Chose the composer over the labelled form it replaces, as the handoff
decided. This note keeps which frame each surface matches and the choices
the canvas leaves open or that depart from it.

## Frames Each Surface Matches

- **Mac, closed and open:** `New task closed` and `New task open`. The
  goldens draw at the matrix's 380pt Mac width, not the canvas's 560pt, so
  the role chips wrap onto several rows there; the live sheet's ideal
  width is 560pt.
- **Mac, grown:** `New task grown`, as `new-task-sheet-grown` and its dark
  pair: a long draft clipped at the cap with the foot under it.
- **iPhone:** `iPhone new task`. The goldens can't draw the navigation bar,
  so the phone-width goldens show the composer alone, and the bar is checked
  on a simulator or device.
- **Chips:** `R33 states`, with one gap: a role chip never takes its active
  wash, because SwiftUI doesn't tell a `Menu`'s label that the menu is open.

## The Summary Reads Verbs

Chose `claude-opus writes · codex reviews` over `Writer claude-opus ·
Reviewer codex` everywhere `AgentChoiceDisplay.summary` is read, the
recovery sheet included. The handoff README's Client Behaviour says the
writer summary rule is unchanged, but its open item on the Agents chip's
wording changes the wording everywhere it is read; the issue contract
follows that item. Which agents the sentence names is unchanged: the
writer once when the three writer lines agree, otherwise each agent with
its roles' verbs (`claude-opus specifies · codex implements and
remediates`).

## The iPhone Composer Has a Navigation Bar

The iPhone composer is the one sheet that leaves the System Chrome rule
that no sheet uses a navigation bar
(`devlog/2026-09-12-0945-chrome-under-design-language.md`): Cancel and
Submit sit in its bar, the iOS form idiom. The owner decided this as ◆3 on
2026-11-04 (`devlog/2026-11-04-1251-settings-usage-gate-answers.md`),
choosing the sheet as drawn over the composer as an editable row at the
head of the list. The Mac sheet keeps the rule and has no button at all.

## Return on the Mac

- **Return submits; while the draft can't send it does nothing.** Chose to
  swallow Return over inserting a line when the form can't send, because a
  disabled default button would do nothing, and a Return that sometimes
  submits and sometimes adds a line teaches the operator to distrust it.
  Shift-Return always inserts a line.
- **Nothing in the sheet binds the default action.** A default button
  takes Return before the text view sees it, so the sheet keeps only a
  hidden cancel button for Escape.

## How Far the Text Area Grows

Chose 60% of a fixed 600pt ceiling (360pt) for the Mac cap over 60% of the
sheet's height, because a macOS sheet sizes to its content and has no
height of its own until the content picks one. On iPhone the cap is 60%
of the height the sheet gives the composer below its bar. The keyboard
shortens that height, so the cap drops while the keyboard is up, which
keeps the foot on screen above it.

Revisit when: a Mac sheet gets a fixed or resizable height, or the canvas
redraws the grown frame against one.

## Two Recipe Fixes

- **A disabled composer chip reads faint.** With no agent catalog the
  Agents chip is disabled; R33 draws no disabled state, so the chip's text
  takes the faint ink the design language's other disabled controls use,
  rather than look live.
- **A role chip stacks at accessibility sizes.** At phone width the
  one-line chip broke its keyword or the agent's name inside a word, so it
  falls back to the keyword over the name when the line doesn't fit.
