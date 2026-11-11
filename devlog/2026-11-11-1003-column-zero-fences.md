# Authored Prose Emits Every Fence at Column 0

Issue #3053, deferred from #3022
(`devlog/2026-11-10-1757-readable-pr-bodies.md`). Tier A: the authored-prose
renderer screens agent prose before it reaches GitHub.

Chose to emit every fence the renderer keeps live at column 0 over teaching
the renderer to track list items and blockquotes, because a column-0 fence
ends on GitHub exactly where the renderer ends it, with no Markdown parser to
get wrong in a security screen. The planner made this choice in the issue.

## The Defect

`renderAuthoredBodyProse` emitted a fence opener indented 1 to 3 spaces as is
and decided closure on the input line, as if the fence were top-level. Inside
a list item or blockquote GitHub ends the fence when the container ends, so
the two disagreed in both directions:

- A later top-level fence opener read to the renderer as the list fence's
  closer, so it appended no closer and the publisher's sections rendered as
  code.
- GitHub could close a list item's fence on a line the renderer didn't treat
  as a closer (a closer indented five spaces under a `- ` item), so the lines
  after it were emitted raw but rendered as live prose: an `@`-mention or raw
  HTML there was never neutralized.

## Decision

- **Fences go to column 0.** The opener's 0 to 3 leading spaces are stripped,
  up to as many are stripped from each line inside the fence (as GFM does with
  an indented fence's content), and closure is decided on the emitted line.
  Deciding it on the input line would recreate the second defect: a 5-space
  closer under a 2-space opener becomes a 3-space closer, which GitHub
  accepts.
- **Why column 0 is exact.** A column-0 fence opener is always top-level: no
  list item continues past it, and the neutralized prose holds no container
  or block that could absorb it, because `escapeInertText` escapes `>`, `<`,
  `|`, backticks, and tildes, and the screen refuses tabs and carriage
  returns. So no blockquote, HTML block, table, or other fence exists outside
  the renderer's own fences.
- **Accepted cost.** A list item's code block renders after the item instead
  of nested in it.
- **Rejected: a container-tracking block parser.** Tracking list items,
  paragraphs, lazy continuation, and blockquotes would keep code nested, but
  it is a mini Markdown parser inside a Tier A screen, more code that must
  agree with GitHub on every edge. Also rejected: a Markdown parser
  dependency (issue non-goal).

## Refute-First Findings

- **Differential check, confirmed.** 50,000 generated bodies mixing list
  markers, blockquotes, 0 to 6 space indents, backtick and tilde fences,
  mentions, raw HTML, and table rows were rendered by both the old and new
  renderer, then by goldmark with GFM with a sentinel paragraph appended. The
  new renderer had no case where the sentinel landed in code or a mention, raw
  tag, or table rendered live outside code. The old renderer failed 775
  cases, both kinds. goldmark stands in for GitHub's cmark-gfm; the fence and
  container rules exercised here are CommonMark, where the two agree.
- **Fresh-context review, no defect in the change.** It tried lists (empty,
  nested, `10.`), lazy continuation, blockquotes, HTML blocks, link reference
  definitions, footnotes, setext lines, indented code, tables, tilde fences,
  and code spans posing as openers; each ends at, or can't contain, a
  column-0 fence line.
- **Allowed by decision: the test helper shares the renderer's grammar.**
  `assertFencesTopLevel` uses the same `FenceOpener` and `ClosesFence`, so it
  can't catch an error in `fence.go` and passes any output that keeps the
  column-0 invariant. It pins that invariant as the issue asks; agreement
  with GitHub rests on the exact expected strings and the differential check.
- **Pre-existing, out of scope: a pipe-free table delimiter row.** The review
  found that `x`, `-:`, then a code span holding a `|` forms a one-column
  table on goldmark that splits the code span and leaves its text live. It
  doesn't involve fences. Follow-up: #3073.

## Revisit When

`escapeInertText` stops escaping `>`, `<`, `|`, backticks, or tildes, or the
screen starts admitting tabs: then a column-0 opener might sit inside a
container and this argument needs another look.
