# Authored Prose Escapes Pipe-Free Table Delimiter Rows

Issue #3073, deferred from #3053
(`devlog/2026-11-11-1003-column-zero-fences.md`). Tier A: the authored-prose
renderer screens agent prose before it reaches GitHub.

Chose to escape a pipe-free table delimiter row at the block level, matched
on the line after inline neutralization, over escaping every `-` or `:` in
prose or touching code spans, because it changes only the lines that can open
a table and leaves visible code untouched.

## The Defect

`escapeInertText` escapes every `|` outside code spans, but
`neutralizeInlineText` copies code spans through verbatim, and GitHub splits
table cells on a `|` even inside a code span. A delimiter row with no pipe
(`-:`, `:-`, `:-:`, with any number of hyphens) still opens a one-column
table under a paragraph line. A body cell such as `` `a @octocat|` `` is then
split, its backtick is left unpaired, and the text before the `|` renders
live: GitHub's Markdown API returned a `user-mention` link, a live `<b>`, and
a live `<a href>` from such cells.

## Decision

- **The rule.** `tableDelimiterRowRe` matches a line that is spaces, then
  `:-…`, `:-…:`, or `-…:`, then optional spaces. `renderAuthoredBodyProse`
  inserts `\` before the first non-space character. The escape renders
  invisibly and keeps the indentation, so a list item's continuation row stays
  in its item. Indentation is unbounded; over-matching a 4-space indented line
  only adds a visible backslash in code, the safe direction.
- **Matched after neutralization, not on the input line.** Stripping a link
  or image to its text can leave a bare row: `[-:](url)`, `-:[](url)`,
  `![:-](url)`, `[-](url):`, and `[ ](url)-:` all pass the screen and
  neutralize to a bare row, from which GitHub opened a table. Matching the
  neutralized line catches every such form. A list item line never
  neutralizes to a row, because its marker is followed by a space.
- **Padding is spaces only.** cmark-gfm's delimiter grammar also accepts
  `\v` and `\f` as padding, but the screen refuses every control character
  except `\n` (`importer/message.go`), so none reaches the renderer.
- **Out of scope by contract.** Hyphen-only lines (GitHub reads `-` and `--`
  as setext underlines; `---` and longer are thematic breaks) and rows with a
  `|` (already escaped; GitHub opens no table from `\|-:` or `-:\|`).
- **Rejected: escape every `-` or `:` in prose.** It changes far more text
  than the rows that can open a table.
- **Rejected: escape `|` inside code spans.** GitHub keeps backslashes inside
  code spans literally, so the escape would change visible code.
- **Rejected: a Markdown parser or dependency** (issue non-goal).

## Evidence and Refute-First Findings

- **GitHub builds the table, confirmed.** During planning, GitHub's Markdown
  API rendered `x` / `-:` / `` `@octocat|` `` and its `:-`, `:-:`, `|-:`,
  3-space, and list-nested variants as a one-column table. `\-:`, `-\:`,
  `\:-`, `\:-:`, `-:\|`, and a 4-space `-:` after a paragraph did not.
- **Link-stripped rows, confirmed and fixed.** The first version matched the
  input line; the refute-first pass found the stripped-link bypass above, and
  the rule moved to the neutralized line.
- **Live probe, no table survives.** 42 screened bodies were rendered and sent
  to GitHub's Markdown API: row shapes (`-:` to `:--:`, trailing spaces, 3, 4,
  and 6 space indents), header contexts (emphasis, a hard break, ordered,
  `10.`, and nested lists, a lazy line, an escaped blockquote), eight
  link-stripped forms, and shapes the rule deliberately skips (`- :`, `-:-`,
  `::-`, `-::`, Unicode dashes, no-break-space padding, an input `\-:`). None
  produced `<table` or `user-mention`. With the renderer's escape removed, every
  reachable shape opened a table, so the probe discriminates. A row under an
  ATX heading, after a blank line, or as a lazy list line opens no table.
- **Fresh-context review, no defect in the change.** It argued completeness
  from cmark-gfm's grammar: a delimiter row must share a paragraph's
  container, the screen leaves spaces as the only padding, and no other
  renderer branch emits a row-shaped line. Against GitHub's API it confirmed
  the escaped forms, list continuation staying in its `<li>`, hyphen-only
  lines rendering as setext headings or text, and no-break-space padding
  opening no table.
- **Allowed by decision: a visible backslash in indented code.** `a`, blank,
  then `    -:` renders `\-:` inside the code block, the same cost `\|`
  already has there.
- **Pre-existing, out of scope: rows and rules built from link text.** The
  thematic-break check also reads the input line, so `[---](url)` renders a
  live `<hr>` (confirmed on GitHub). Nothing in it goes live, but the
  renderer's doc says rules are neutralized. Follow-up: #3368.

## Revisit When

`escapeInertText` stops escaping `|`: every multi-cell delimiter row then
reopens, and this rule covers only one-column tables. Also revisit if
`neutralizeInlineText` or link stripping changes how a line shrinks, or the
screen starts admitting tabs or other control characters.
