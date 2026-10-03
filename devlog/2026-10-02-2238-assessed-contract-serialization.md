# Contract Serialization by Assessed Conflict

Issue #1709, plan revision 79. Owner decision of 2026-10-02 for the regime,
the cap, and the deferrals; the agent's choices inside that decision are
marked as such below and are open to the owner's review.

## Changed Assumption

**The contract regime treated every pair of contract units as conflicting.**
Each `kind:contract` unit stood `exclusive-with` every other, so one chain
carried dependency, conflict prevention, and queue order together. Wave 8
holds seven open contract units in that chain, and two more trackers' units
(#1600 and #1425 on #1616) compete for a position in it. A slot runs days
from claim to merge. Wave 7's exit-repair record says its later links were
"serialization, not a claim that their code depends on one another" (#1001,
comment 5573389160), and #1425's Dependencies already said the owner might
swap its order with #1600. The chain was serializing work that nothing
showed to conflict.

## Chose Assessed Conflicts Over the Repo-Wide Regime

Chose (owner) to keep `kind:contract` as a classification that invokes
contract review and verification, and to replace the automatic exclusion
with a spine assessment of each pair, recorded in both issues' Dependencies
fields. An unassessed pair stays serialized. The hazards the regime guarded
against are real (incompatible field meanings, conflicting admission rules,
migration collisions, broken reconstruction, mismatched generated
consumers), so the change asks for a stated reason to run two contracts
together, not a stated reason to block them. Section 5.18 of the plan
already projects the daemon's frontier this way: declared conflicts block,
and unknown scope serializes. The process now matches the product.

Rejected:

- **Keeping the regime and only retyping queue-order edges.** Retyping
  `starts-after` to `merges-after` removes false start order but leaves
  every pair exclusive, so nothing runs concurrently.
- **Treating different files or a clean merge as independence.** Two
  contracts can touch disjoint files and still disagree about a field's
  meaning. The assessment compares behavior, invariants, consumers,
  persistence, generated artifacts, and recovery paths.

## Chose a Cap of Two

Chose (owner) at most two concurrent contract implementations inside the
four-front width. The gain is elapsed time, and owner review bounds it:
every contract PR waits for the same reviewer. The cap rises to three only
when a third contract unlocks a cluster nothing active unlocks, a contract
PR waits under a day for owner review at two, and all three units are
pairwise assessed on their issues.

Rejected: **starting at three.** Nothing yet shows that review keeps up
with two. Owner review hours per merged contract is the pilot's stop
signal: if it rises while elapsed time doesn't fall, the cap stays at two.

## Deferred Standing Tracker Authorization

Chose (owner) to leave the two authorization doors, scheduling and fiat, as
they are. Rejected for now: a third door that lets an owner-approved ad hoc
tracker authorize its listed units. Per-unit fiat isn't the bottleneck, fiat
is the owner's control over review capacity, and a batch fiat already
covers several units. If the question returns, the cheapest form is to
widen the scheduling door to owner-approved ad hoc trackers.

- Follow-up: #1711 (standing authorization for ad hoc trackers,
  `needs-human`).
- Follow-up: #1712 (migration protocol for concurrent contract units). The
  pilot avoids pairs that both add a migration, so the protocol waits for
  the first pair that needs it.

## Choices Made in Implementation (Agent)

1. **Independence needs its own record; `merges-after` alone doesn't say
   it.** Chose a `Contract assessment` line in both units' Dependencies
   fields over reading any typed edge between two contracts as their
   assessment. #1425 already carried `merges-after` #1600 while the old
   regime held, so reading that edge as independence would have opened the
   pair to concurrent work the moment the policy merged, before anyone
   assessed it. A verdict other than independent is written as the typed
   relationship it yields.
2. **The claim gate treats a missing record as a conflict.** Chose to fail
   closed at claim time, beside the spine's `starts-after` fallback at
   scheduling, over relying on the fallback alone. A pair the spine never
   reached then still serializes.
3. **Only active or reserved contract units block a contract claim.** The
   old gate blocked on every open scheduled contract unit that wasn't
   downstream, which is how the chain kept its order. Order now comes from
   the `starts-after` edges the chain already records; concurrency comes
   from the assessment and the cap.
4. **Surface overlap doesn't block one contract unit against another.** The
   gate's "touching the shared-package surfaces your work will change"
   clause now applies to non-contract work. Most contract units name
   `daemon/internal/domain`, so applying it between contracts would cancel
   every independence assessment.
5. **The cap arbitrates like a conflict.** Two claimants can each see one
   active contract and both claim. Chose the existing ordering key
   (`created_at`, then comment ID): the latest claim past the cap releases.
6. **Recording independence may happen while an endpoint is claimed.** It
   only relaxes, and the pilot assesses a second contract against a chain
   head that is nearly always claimed. Withdrawing independence restricts,
   so it follows the `exclusive-with` edit protocol.
7. **A handed-off PR is revalidated when it is selected for integration,
   not on every intermediate base advance.** Its evidence is stale from the
   advance on, and it never merges stale. When the advance landed another
   contract unit, a contract PR also re-runs its generated consumers and
   component checks, because a clean textual merge doesn't show that two
   contracts compose.
8. **The occupancy label mirrors claim state and the tool never writes
   it.** `coord:contract-active` exists because label filters can't
   evaluate claim comments and PR-backed claims. It is duplicated state, so
   `trackercollect contracts` reports drift (`missing-label`,
   `stale-label`) and people repair it. Chose a read-only report over
   automatic repair: a repair driven by an incomplete read could clear a
   label on a claimed unit.
11. **A scope edit removes the edited unit's independence records.** PR
    review found that the record named a base commit and nothing tied it to
    the issue bodies, so a later scope edit left a false record standing and
    "the spine reassesses" named a condition nobody checked. Chose a duty
    on the editor: whoever changes an assessed contract unit's Objective,
    Scope, or Affected interfaces removes that issue's records in the same
    edit, and the pair serializes until the spine reassesses. One side is
    enough because a pair needs the record on both issues. The removal
    doesn't wait under the `exclusive-with` edit protocol as the spine's
    own withdrawal does (item 6): the edit has already made the record
    false. Rejected: body digests in the record, checked at the claim gate,
    because a hand-written digest goes stale on every unrelated body edit;
    and leaving the text as it was, because a stale record opens
    concurrency on missing evidence.

## Revisit When

- The pilot pair merges: compare blocked time, review wait, owner review
  hours, repeat verification, and completed slices against the chain's
  record before raising the cap or widening the pilot.
- A scope edit to an assessed unit leaves its independence record
  standing: that is the signal to add a mechanical staleness check to
  `trackercollect contracts`.
- A lease whose PR closed unmerged inside its 48 hours shows up as a false
  `missing-label`: the report reads open PRs only, so it can't see that
  release.
