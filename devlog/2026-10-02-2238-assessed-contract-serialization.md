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
- Follow-up: #1729 (the wave skills and the work-unit issue form still
  describe the repo-wide regime; they were outside this unit's scope).

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

9. **A PR closes the unit by the forge's closing-issue list or by the close
   keyword in its body.** The forge lists no closing issue for a PR based on
   a branch other than the default one, so the list alone reads every
   stacked PR's claim as expired once it is 48 hours old. Chose to read the
   body over reporting such a PR as ambiguous: the protocol's own test is
   whether the PR carries the keyword.
10. **The report did not reuse the merged-PR mode's tracker builder.** The
    plan named `buildContainingTrackers`. It fetches every unit of each
    containing tracker and words its ambiguities for a merged unit, so the
    report reads tracker Units sections from the open-issue inventory it
    already holds.
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

## Refute-First Pass on the Occupancy Report

The report trusts fields decoded from the forge, so an independent reviewer
tried to make it misreport before the tool was committed. An under-read of an
active claim is the error that matters: it could lead someone to clear a
label or start a conflicting contract.

Confirmed and fixed:

- **A stacked PR read as `expired`.** Item 9 above.
- **A marker comment with CRLF line endings was skipped with no ambiguity.**
  GitHub stores text typed in its web editor that way: 574 of 1,526 comments
  sampled from a public repository carry CRLF, though none of this
  repository's do yet. The parser now reads them. The fix is in the shared
  parser, so the merged-PR mode also finds CRLF markers and tracker entries
  it used to drop.
- **The ordering key named the PR-backed claim over an earlier live one.**
  It is now the earliest unreleased, unexpired claim.
- **A closing issue was matched by number**, so a PR closing a same-numbered
  issue in another repository read as a legacy claim. It is matched by node
  ID.
- **A milestone without a title decoded as unmilestoned.** It now fails loud.
- **`contracts.json` carried counts over incomplete evidence.** They are
  null when any ambiguity exists.
- **A marker from outside the trust boundary was counted.** PR review
  found this after the pass above. The repository is public, so any account
  can comment on a contract issue. An outsider's claim or reservation read
  as occupancy, and an outsider's release naming a real claim's comment ID
  read that claim as released: the under-read. The claim protocol trusts
  collaborator comments and gives no other marker a stated standing, so the
  report counts a marker only when the forge gives its author association
  as `OWNER`, `MEMBER`, or `COLLABORATOR`, and reports any other as an
  ambiguity with no verdict. Chose (agent) an ambiguity over dropping the
  marker silently, because the report shouldn't decide who may claim. The
  manual claim gate and the merged-PR mode still read a marker from any
  author; whether markers count only from collaborators is the owner's
  decision. Follow-up: #1738.
- **A close keyword quoted in inline code read as closing.** PR review
  found it: a PR body that says to write `Closes #N` kept the unit active.
  The body reader now blanks a code span that opens and closes on one line.
  It leaves a span that crosses a line break alone, because masking those
  would let a stray backtick on another line hide a real keyword, the
  under-read.
- **Four rule changes passed the tests** (the canonical-repository match, a
  legacy PR beside an expired claim, a release against a PR-backed claim, a
  malformed tracker entry). Each now fails a test.

Disproved by a check: a nil label page reaching the report (the decoder
rejects it first); the dropped `parse-collision` ambiguities hiding evidence
the report uses (only Scope parsing raises them); a change to the merged-PR
mode's artifacts on its fixture (byte-identical before and after).
PR review added one: an under-read of a legacy `Claim #N` commit, which the
report never fetches. None of the PRs open on 2026-10-03 carried one, and
the protocol allows no new claim commit, so no open PR can claim that way.

Allowed by decision:

- **A marker in a non-canonical form is not a marker.** One on the same line
  as its `Claim:`, inside a list item, or after an unclosed HTML comment adds
  no ambiguity. The protocol defines one form, and a claimant verifies its
  own saved comment.
- **Comment times sort as strings.** The forge emits UTC with a `Z` suffix.
- **A PR that arrives after its claim's 48 hours still backs the claim**
  (agent). The protocol calls that lease dead, but the forge keeps no record
  of when a PR gained its close keyword, and #1709's acceptance defines
  `expired` by the absence of a closing PR. The unit is active either way;
  only its ordering key differs, and only after a re-claim that skipped the
  new comment the protocol requires.
- **An open PR that closes the unit counts whoever opened it** (agent).
  The author check covers marker comments only. An outsider's PR can only
  add occupancy, never hide a claim, and it is visible in the PR list; the
  protocol's legacy clause names no author. #1738 carries the question.

## Revisit When

- The pilot pair merges: compare blocked time, review wait, owner review
  hours, repeat verification, and completed slices against the chain's
  record before raising the cap or widening the pilot.
- A scope edit to an assessed unit leaves its independence record
  standing: that is the signal to add a mechanical staleness check to
  `trackercollect contracts`.
- A GitHub App or bot account starts posting claims: the forge may give it
  an association outside the trusted three, and the report would read each
  of its claims as ambiguous. None posts claims today.
- A lease whose PR closed unmerged inside its 48 hours shows up as a false
  `missing-label`: the report reads open PRs only, so it can't see that
  release.
