# Shared Tracker Format and Label-Based Wave Resolution

Work unit: #1548. Plan revision 70. This note is mandatory because it records
owner choices and revises the coordination contract.

## Decision

Freeside adopts the owner's shared tracker-issue format for every tracker,
copied verbatim into `docs/tracker-format.md` from the free-skills agent-setup
reference (freeasinbird/free-skills#270 at `a79d39f`). AGENTS.md and
docs/coordination.md cite it and keep only Freeside's additions: how
`merges-after`, `stacked-on`, and `exclusive-with` appear in a diagram that
the shared legend draws for `starts-after` only; the structural **Startable
now** rule, including the `stacked-on` base conditions; lane grouping; and the
critical-path proxy.

Two owner choices came with the format (2026-09-25):

- **A tracker carries start order only.** **Mergeable next** leaves the
  tracker and the merge-cleanup refresh. The `merges-after` relation stays,
  because open units still declare it (#1500 and #1503 after #1501, #1332,
  #408, #830, #831). Its start, handoff, and integration checks are
  unchanged; only the tracker projection goes.
- **The wave tracker is found by its milestone plus the `tracker` label, not
  by title,** so the title can read `Wave N: <Name>`.

## Changed Assumption Behind Revision 34

Revision 34 (devlog 2026-08-17-2108-inter-wave-state.md) resolved wave state
over pinned title matches and rejected "zero matches means inter-wave",
because an unpinned or never-created tracker looked the same as a completed
wave. That argument depended on pins carrying authority. With identification
by label and milestone, pins carry none, so an interrupted pin swap can't
misstate wave state. The remaining failure, a wave tracker losing its label
or milestone, reads as inter-wave and fails closed: the scheduling door
shuts and fiat is unaffected. So the resolver now reads one open labeled,
milestoned issue as active-wave, none as inter-wave, and more than one as
invalid.

Rejected: keeping the pinned closed tracker as the inter-wave marker under
the new identification. It would have kept #828's non-atomic pin swap as an
authority step, and today's marker (#1001) has neither the label nor a
milestone, so adopting it would have meant editing a closed tracker.

Freeside's milestones name phases (1A, 1B), not waves. A wave tracker carries
its phase milestone; no other tracker carries a milestone, which is what lets
the milestone mark a wave tracker.

## Critical Path

The shared format draws the critical path with thick arrows but doesn't say
how to derive it. Freeside uses the longest chain of unmerged units linked by
`starts-after`, counted in units, until #674 settles whether an estimate
source exists. It is drawn at planning and redrawn only with a Dependencies
change, because the format's merge refresh never changes edges. This is the proxy wave 5's tracker used in practice.

## Revisit When

- The shared format changes upstream: recopy it verbatim, then reconcile the
  Freeside additions.
- Freeside moves to per-wave milestones, or a non-wave tracker needs a
  milestone.
- Integration order needs a tracker view again, for example when several
  `merges-after` chains queue at once.
