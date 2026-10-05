# Contract Units Declare No `merges-after`

The owner raised the problem on 2026-10-05, reading tracker #1616, and asked
for a fix. The rule's form is the agent's and is open to the owner's review.
It amends the regime #1709 set up
(`devlog/2026-10-02-2238-assessed-contract-serialization.md`).

## Changed Assumption

**`merges-after` was assumed harmless for any unit.** It "constrains
integration order, never start order", so a unit that carries it may start
at once and wait to merge. For a contract unit the wait has a cost. Its open
PR is the active claim, the claim never expires, and it counts toward the
cap of two. A contract unit that starts ahead of an unmerged prerequisite
holds a contract slot until someone else's work merges.

#1425 showed it. Its `starts-after` #1600 was retyped to `merges-after` on
2026-10-02, because the edge was queue order and #1425 reads nothing #1600
adds. Tracker #1616 then drew #1425 startable behind a fenced #1600. Had
#1425 been claimed:

- **The pair would have deadlocked.** It is unassessed, so #1425's claim
  blocks any claim or planning reservation on #1600, and #1425 can't merge
  until #1600 does. Only a spine assessment or releasing #1425 ends that.
- **Assessed independent, the pair would have taken both slots** until
  #1600 merged, with every other contract unit waiting.
- **#1425 would always merge second,** so it would always pay the
  revalidation a second contract owes.

## Chose `starts-after` for Every Ordering Prerequisite of a Contract Unit

Chose that a contract unit declares no `merges-after`, over leaving the
relation available to contract units. An ordering prerequisite of a contract
unit is either required, and then it is `starts-after`, or it isn't, and
then the spine records no order. The claim gate reads a `merges-after` left
on a contract unit as `starts-after`, so a field nobody migrated fails
closed.

This doesn't bring back the queue-order edges #1709 removed. #1709 objected
to start order that nothing required. Under this rule an order nothing
requires isn't recorded at all, and a pair with no order is independent or
unassessed.

Rejected:

- **Let the unit start once its prerequisite has an open PR.** The wait
  shrinks to the prerequisite's review time and some overlap survives. The
  claim gate would have to read the prerequisite's live claim state, and the
  waiting PR would still hold one of two slots through that review.
- **Stop counting a waiting contract PR toward the cap.** The cap bounds
  owner review of concurrent contracts, and a waiting PR still needs
  revalidation and review when it lands. Occupancy would also need a third
  claim state.
- **Limit the rule to an edge between two contract units.** The slot is held
  whatever kind of unit the prerequisite is, and one statement is simpler
  than two.
- **Fence the unit on its tracker.** That hides one case on one page while
  the claim gate still admits it.
- **Project a leftover `merges-after` as `starts-after` on trackers.** Only
  the claim gate reads it that way. The state exists only when this rule is
  broken, and the repair is retyping the field, which redraws every tracker
  that lists the unit. A tracker that follows the field as written keeps
  the stray edge visible until then.

Another unit's `merges-after` on a contract unit is unaffected, such as #408
on #873: the waiting PR is not a contract PR and holds no slot.

#1425 was the only open contract unit with an unmet `merges-after` when the
rule was written. #1332 and #830 also carry one, and both prerequisites have
merged.

## Revisit When

- A contract unit sits unstarted behind a prerequisite whose PR is already
  in review, and the idle time costs more than a held slot would. That is
  the case for the first rejected option.
- The cap rises to three. A held slot then costs less.
- A contract unit declares `stacked-on`. Its child PR waits on its base the
  same way, and this rule doesn't cover it.
