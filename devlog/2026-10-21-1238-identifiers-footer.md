# Gather the Task Timeline's Identifiers in One Footer

Chose one `Identifiers` disclosure at the foot of the task timeline, plus a
copy context menu on each thing an identifier names, over a Technical
Details disclosure on every card. The task header, each campaign section,
and each run card each drew one, so one screen showed the same label four
times. Each was an interactive row hiding one to three 70-character ids
that nobody reads in passing. The owner's verdict (2026-10-21): noisy and
annoying; keep the data out of the way but reachable (#2117).

The footer sits under the page's one hairline, as the run timeline's does,
and holds the same rows the cards held, grouped by owner in page order: the
task, then each campaign followed by its attempts. It is collapsed by
default and remembered per task like the page's other folds. Copying a
single id needs no trip there: the task eyebrow keeps `Copy task ID`, a
campaign header offers its campaign id and approved specification digest,
and a run card's header offers every row the run holds. `More Actions`
gains `Copy all identifiers` for pasting the whole set into a terminal or
an issue. Copying needs no daemon, so that item stays offered, with the
menu, in Stop states where Stop itself is unavailable or already sent. The project, issue, lifecycle, and source line, which sat in the
header disclosure's closed summary only so the disclosure said something,
became the mono identity line under the task name.

The shared component's label became `Identifiers`, so the task timeline,
the run timeline, and the specification-approval reader say the same word.

## Rejected Options

- **Keep the per-card disclosures and shorten their summaries.** The noise
  is the repeated interactive row, not its summary text.
- **An inspector column.** The owner rejected it (2026-10-21); at compact
  width it would also need a second presentation.
- **A sheet or popover behind a button.** The owner rejected the sheet. It
  costs a modal hop when comparing an id with the page, and adds a
  presentation the timeline does not otherwise use.
- **Hover-revealed copy controls.** A touch platform has no hover.

## Revisit When

A task page holds so many campaigns and attempts that one grouped footer
stops scanning, or a reader needs a card's ids beside the card more than
once in passing.
