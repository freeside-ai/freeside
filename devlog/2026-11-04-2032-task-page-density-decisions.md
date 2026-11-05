# Task Page Density Decisions

The owner answered the four ◆ items the 4 Nov task page density
counter-proposal (`docs/design/handoff-2026-11-04-task-page-density/`) left
open, chose what not to build, and accepted its remaining defaults. The
README carries each answer and the units that build it; this note keeps the
reasons. The reasons are the counter-proposal's; where it gives none, the
note records the choice alone.

## Counter-Proposal Over the Original

Chose the counter-proposal, revision 2, as the canvas over the original 4
Nov proposal. Both diagnose the same problem: forensic facts draw at the
same weight as the facts an operator reads. The counter-proposal gets the
density with no plan change and no new mode. From the original it keeps
three altitudes (glance, read, forensic) as the lens, the decision's
evidence above its actions, admission facts out of reading weight, and
milestones off the task page. It rejects four things:

- **The per-card Details fold.** It re-adds the per-card technical fold the
  owner removed on 21 Oct as noisy and annoying (#2117,
  `2026-10-21-1238-identifiers-footer.md`), would put three on one page,
  mixes reading (Recorded Context) with copying (digests), and its name
  collides with Run Details.
- **The ⌥⌘D technical-detail mode.** It is persistent per-device state
  stacked on per-task fold memory, it changes how values render, and it
  doubles the screenshot matrix, all for a debugging case that Copy all
  identifiers and hover-for-exact-time already cover.
- **The plan §9 provenance revision.** What makes the finding labels hard
  to read is the sentence-long producer keyword, not the word
  "(unverified)". Shortening the keyword fixes it and keeps the word.
- **Frame 5's pick sentence.** "Accept All Dispositions keeps that choice"
  is false: Accept All sends the proposed routes, and only Choose Another
  Route sends a held pick. An operator who trusted it would lose their
  override. The app's current wording is correct.

The owner decided on 2026-11-04.

## Identifiers (◆A)

Chose to move a seated card's Technical Bindings into the page's one
Identifiers footer, as a "Decision" group, over keeping a technical fold on
the card. It extends #2117 to the last surface it missed, under the
counter-proposal's rule: if you'd copy it, it's in Identifiers; if you'd
read it, it's in the body. The footer already groups ids by owner for the
task, campaigns, and runs, and a Copy menu on the card eyebrow (item ID,
evidence digest) keeps single ids one gesture away, as the task eyebrow and
run headers already do. A scoped card with no task page has no footer to
join, so it keeps one Identifiers fold of its own, under the same word
every other surface uses. The owner decided on 2026-11-04.

## Task Actions (◆B)

Chose `Task Actions ▾` in the outline row's trailing slot, which the 30 Oct
handoff left empty (its ◆5), and the navigation bar's trailing slot on
iPhone, over the task's More Actions menu in the body. The page draws two
menus named More Actions, and the task's floats alone in the body. Moving
it leaves one menu per scope, and the pinned row keeps Stop Task reachable
while scrolled. The owner decided on 2026-11-04.

## Open Current Run and Timeline (◆C)

Chose an always-open Current Run section and an always-open Timeline over
the 30 Oct rule that every task-page fold starts closed, for these two
sections only. With every fold closed, the live review round sits two
clicks down, inside the current campaign's fold. Opening the Timeline by
default works only if the page stops nesting the present inside the past,
so the body splits by tense: Current Run holds what is happening, the
Timeline what happened. Earlier attempts and earlier campaigns keep
folding, with memory per id as today.

The Task fold built from the inspector on 30 Oct
(`2026-10-30-1800-task-page-inspector-fold.md`) dissolves because every row
it held has a better home and none is dropped: project and stage already
repeat the eyebrow, the admission rows become one line under the current
attempt, the pull request moves to the documents line, and the repository
and policy digest go to Identifiers. Cost owner, credential mode, egress,
and isolation stay on the run page, drawn per invocation. The trade is that
a task with a long history is taller by default, because the history is now
a primary part of the page. The owner decided on 2026-11-04.

Revisit when measurement shows tasks routinely past about 15 rail rows. The
counter-proposal's fallback then is the newest 12 rows and a "Show N
earlier events" row that expands in place, with no stage bands.

## Documents Line (◆D)

Chose one quiet line under the status linking Prompt, Specification, and
Pull Request (Issue in place of Prompt for an issue-sourced task) over
leaving them off the task page, where neither the prompt nor the
specification is reachable today. It costs one line and puts the three
things an operator rereads before deciding one click away in every state.

Chose links only over the prompt excerpt variation, which costs two lines
on every task. The counter-proposal recommends the excerpt only if
operators keep opening the prompt; the owner chose not to build it.

The owner also approved the daemon change the Prompt link needs. The
counter-proposal expects the prompt to need a daemon read, noting, without
having verified it, that a task's source carries only a work-item artifact
id or an issue reference and that the API has no read by artifact id. The
owner's answer carries the work-item artifact's digest on the synced task,
through #2610 (`kind:contract`). The Specification, Pull Request, and Issue
links don't wait for it. The owner decided on 2026-11-04.

Revisit the excerpt if operators keep opening the prompt, the condition
under which the counter-proposal would use it.

## Not Built

Chose not to build ⌥-click recursive open or its optional one-shot
`View ▸ Expand All Folds` command, which the counter-proposal offered in
place of the ⌥⌘D mode. The owner gave no further reason. Stage bands were
already removed in revision 2 after owner feedback. The prompt excerpt and
the long-rail cap are covered above. The owner decided on 2026-11-04.

## Accepted Defaults

Chose the counter-proposal's defaults without change. The owner decided on
2026-11-04.

- **Timeline over History.** The outline chip already says Timeline, the
  run page has a "Run timeline", and "History" collides with `Open run
  history ›`.
- **The reason clamp.** A finding's reason sits in its quote because it is
  what accepting rests on. Two lines per finding is about 40pt; past three
  findings, one line keeps six findings' actions inside the 520pt
  first-viewport budget (#1141). A reason opened stays open, remembered per
  finding id like a fold.
- **"(unverified)" on every label.** A fixed keyword, "Proposed route
  (unverified)", inside the quote with a compact source mark replaces the
  producer sentence, so plan §9 needs no revision.
- **The body unit's split.** Decided when that unit is planned.
