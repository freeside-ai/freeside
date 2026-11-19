# Agent Transcript as a Conversation

**Decision (owner, 19 Nov 2026, #3459):** the agent transcript reads as a
conversation, in a sheet opened from the run page. The owner decided from
boards shown in four rounds. The rules are in the Design decided comments
on #1131 (the sheet) and #1926 (a run in progress); this note keeps the
reasons and what was declined.

## A Conversation, Not a Record

Chose a conversation over a sectioned sheet with boxed tool calls and a
block of daemon facts, the first round's design. The owner's words: "much
simpler and conversational", taking its cues from T3 Code and Claude Code's
desktop app. The operator opens a transcript to follow what the agent did,
and a record's sections put the daemon's bookkeeping ahead of that.

- **It starts with the prompt,** as a bubble on the trailing side, with
  the agent's messages as plain prose on the leading side. The owner
  noted that two sides also leave room for steering if it lands: a
  steering message would sit with the prompt.
- **Work is one dim sentence** that opens to a line per call, and a call
  opens to its output. T3 Code's grouping, kept as #3459 listed it.
- **A finished run folds** behind `Worked for`, over its final message. A
  failed run never folds, because its last calls are what the operator
  came for.
- **A long transcript opens at its end,** which answers #1131's report
  that the head cut hid the result.

## A Sheet From the Run Page

Chose a sheet opened by an `Open Transcript` link under each writer
invocation on the run page, over a section of the run page itself. A
transcript is long and is read on its own, and one sheet serves a finished
run and a running one (#1926). The attachment route on a card stays.

## No Usage on the Sheet

Chose no timestamps, cost, or token usage on the sheet, over #1131's
original Expected list, which asked for them. Spend already has a home on
the run page, and the only time the sheet shows is `Worked for` or `Failed
after`.

## One Line for Unreadable Lines

Chose one quiet line after the conversation (`2 lines Freeside could not
read`, opening to each line with its number) over the diagnostics section
#3459 proposed and the Reviewer output sheet uses. Nothing is dropped; a
section gave a rare parser miss the weight of a finding.

## The Reviewer Output Sheet Stays

The owner left the Reviewer output sheet as it is for now. Revisit when
the transcript sheet is built: the two then show the same kind of stream
in two forms.

## No Handoff Directory

#3459 asked for a canvas and a README under `docs/design/`, in the form of
the 4 Nov task page density handoff. The owner chose the form the design
work has used since it moved to FigCraft: the frames stay in
`freeside-design`, on a proposal page, and each building issue carries the
rules, one board, and the frame names. A canvas copied here would go stale
as the proposal page is redrawn, and a README would repeat the issues.
The older `docs/design/handoff-*` directories stay as dated records.
