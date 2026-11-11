# Sidebar Sections at the Foot, No Empty Needs You

Owner decision, 11 Nov 2026, from T3 Code's sidebar as the reference: the
unified list stops drawing an empty state for Needs You, a section with no
rows draws no head, and on the Mac Working and Finished rest at the
sidebar's foot until Needs You pushes them down. This reverses the 30 Oct
unified-list handoff, which put the R13 empty state inside Needs You "so
Working stays one line below it"
(`docs/design/handoff-2026-10-30-unified-list/README.md`).

## Empty Space Over a Placeholder

Chose an empty Needs You over the glyph-and-statement placeholder. With the
sections at the foot, the empty space above them already says nothing is
waiting, and a centered statement in the sidebar repeated what the Mac
detail pane says after the last item closes. The detail pane keeps its
`Nothing needs you.` result; R13 still governs every other centered empty
state.

## One Column on the Mac, Held at the Foot

The owner's rule: Needs You always pushes the other sections down, even
below the fold, and neither Needs You nor Working is ever capped; only
Finished is. So the Mac draws one scrolling column, Needs You, a gap, then
the sections, at least the sidebar's height: while everything fits the gap
holds the sections at the foot, and past that it is gone.

Rejected, in order:

- **A pinned block for the sections** under a separately scrolling Needs
  You, capped at 60% of the sidebar. It was built first; the owner turned
  it down because it capped Working and kept the sections in view over a
  long Needs You.
- **A measured spacer row in the system List.** It would keep the List's
  own selection and arrow keys, but the spacer is measured after layout,
  so each section toggle or arriving row would draw one frame at the old
  height and jump.

The column is a ScrollView over two lazy stacks, so it sizes in one layout
pass and still mounts only the rows on screen (a probe of 1,000 rows in this
shape mounted 15). Leaving the List costs its built-in row navigation, which
the column replaces: the arrow keys step through every drawn row, now
Working and Finished too, and j/k stay within Needs You as before.

On iPhone the sections follow Needs You in the one scrolling list, as T3's
phone does: a phone screen has no room to hold them above its bottom bar,
and the single list keeps pull to refresh and the draft's swipe.

## Finished Pages Its Newest Rows

An expanded Finished draws its 10 newest rows and a `Show N More` line that
adds up to 25 more, on both clients, so years of finished work can't bury
the sections above it. The page lives in the view and starts over when
Finished collapses; it isn't a per-device preference like the disclosure
state, because a long page is a one-off look back, not a setting. A route
to an older finished row grows the page to hold it, so the row it opens is
drawn and the arrow keys step on from there.

Revisit when: operators page back through Finished often enough to want
the page's depth kept per device.
