# Finished Rows Draw Outcome Glyphs and Keep the Name Whole

**Decision (owner, 26 Nov 2026, #3671, tracker #3669):** a compact
Finished row draws how its task ended as the lifecycle glyph the app
already names (`checkmark.circle`, `stop.circle`, `minus.circle`), and a
look-alike task's short id moves from the title line to the end of the
context line (`freeside · #80 · task-c09e6f18…`). This revises the
Finished Outcomes section of
`devlog/2026-11-19-1259-issue-backed-task-names.md`, which drew the outcome
as a timeline marker before the name, and the #1922 rule that a compact
Finished row drew the short id beside the name.

## Why

- **Color alone told finished from stopped.** Both were a filled dot,
  diff green against wax, so with Differentiate Without Color on, in
  grayscale, or to a reader who doesn't see the two hues apart, the only
  difference was gone. The glyph's shape now carries the outcome; each
  keeps its old color as a second cue.
- **The short id cut the name.** Three retries of one issue share a name,
  so each is a look-alike, and the id beside the name left room for
  `Drain f…launch` (`unified-list-finished-issue-retries`). On the context
  line the id sits in the mono face that line already uses, and the title
  line holds the name and the time only.

## Chose the Lifecycle Glyphs Over New Marks

The task page's lifecycle label already pairs each outcome with these
symbols (`TaskDisplay.lifecycleLabel`), so the row and the page say the
same thing. The timeline's `ChronologyMarker` keeps its dots; only the
Finished row stops using them.

## Chose the Context Line for Every Look-Alike

A look-alike with no issue gets a context line of project, then short id,
so one rule places the id whatever the task has. A task with neither an
issue nor a look-alike still draws no context line. The id keeps
`ShortIdentifier.short`'s trailing `…`, the form the Identifiers footer and
the Stop sheet print.

The context line now runs under the trailing time as well as the title,
since the id lengthens it past the title's column at the sidebar's width.

## Spoken Once

VoiceOver reads the outcome word, not the glyph's name. A look-alike row
still speaks when the task was created beside its name, but the short id
only once, with the context line, where it used to be read in both.

Working and Needs You rows are unchanged: their context lines already end
with the full look-alike marker.

Revisit when the Finished row gains another per-row fact that competes
for the context line's width on iPhone.
