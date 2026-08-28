# Wave 7 Split Into Four Outcome Waves (Plan Revision 41)

**Work unit:** plan revision splitting the §11 Wave 7 row into waves 7
through 10. **Origin:** four independent split proposals assessed against
the live tracker on 2026-08-27, after Wave 6's last unit (#844) merged.

## Decision

Chose four outcome waves over the single row and over six. Wave 7 proves the
decision surface closes and reads from the phone. Wave 8 proves unattended
operation, diagnosis, recovery, and re-entry of published-PR activity. Wave 9
proves one agent vocabulary and a second real provider, and may split into
9a (contracts) and 9b (adapters) at planning. Wave 10 is the initiative view.
1B.1 spans waves 7 through 9, as 1B.0 spanned 3 through 6; 1B.2 is wave 10.

The count comes from two constraints, not from how the outcomes read:

- Contract work serializes repo-wide (`docs/coordination.md`: claiming a
  `kind:contract` unit blocks on every other open one). 34 open contract
  units carry `deferral`, and the decision cluster (#917 through #924,
  #936, #942, #724, #892, #901) is one chain. Splitting one chain across
  more waves buys no concurrency.
- Every wave boundary costs a fresh-context audit over the whole repository,
  not the wave's delta, and its findings must close or defer before the next
  wave plans. Wave 5's audit produced five fix units.

So: the fewest boundaries that still give each wave a coherent exit proof.

## Why the Row Could Not Stand

The Wave 7 row carried about fifteen workstreams across unrelated trust
boundaries: operational closure, the Codex execution tail, admitted-agent
adapters, provider diversity, the egress registry, and the whole revision-40
decision-surface closure. "Wave 7 complete" would have proved nothing in
particular. The 1B.1 prose named only three of those workstreams, so the
table and the internal-exit definition had already drifted apart. Revision
40's recorded fallback, slipping the telemetry contracts (#924) to the next
wave, was a one-unit remedy for a phase-sized row.

The row was also stale: #401 gate 3 and #405 are closed, the admitted-agent
contract #894 is merged, and #868 declares `starts-after` #406, which the
row placed one wave apart.

## Rejected Alternatives

- **Six waves** (a separate decision wave and comprehension wave, and a
  separate initiative wave later). Both extra boundaries cut the same serial
  contract chain: two more audits for zero concurrency. Its "move the
  initiative view to a later wave" was the existing 1B.2 slot renumbered.
- **"Exactly four."** Wave 9 bundles a second contract chain (#898, #899,
  #900, #979, plus #397, #873, #406, and #869, all `kind:contract`) with the
  full Codex stack and pi. Review bandwidth bound at both the Wave 5 and
  Wave 6 closeouts. The plan marks the wave split-eligible and leaves the
  count to planning, once the chain length is measured; a realized split
  renumbers the halves as waves by plan revision, because §11's resolver
  matches only numbered wave trackers.
- **Providers before operations.** Deferring the Codex driver to wave 9
  extends single-provider exposure; §1B calls that driver a capacity hedge
  against single-provider stalls (§14). The user chose operations first:
  unattended daily operation is the exit claim, and the adapter is worth
  building once against the settled vocabulary rather than retrofitting.
  #866 and #867 have no open prerequisite and may start earlier by fiat.
- **Revising #868's dependency field** to keep it in an earlier wave. An
  account probe over provider profiles needs a registered provider that
  exposes account identity, which is Codex (#406), not the Claude floor. The
  unit moved; the field stands.
- **Filing the client gaps as unbound deferrals.** The queue already held
  122. Each gap became a `kind:contract` unit bound to a row: #979, #980,
  #981, #982.
- **Keeping the drain clause open-ended.** "Sweep-eligible open deferrals
  enumerated at planning" is what made the row unbounded. Each row now names
  its clusters, and the long tail is stated not to drain in 1B.

## Findings

- No `listDevices` operation exists in `api/openapi.yaml`; only `pairDevice`
  and `revokeDevice`. The health endpoint deliberately carries no
  operational state. Neither agent facts (harness, model, cost owner,
  isolation class) nor readiness waivers with granting authority reach the
  sync contract. All four client gaps are therefore contract work, not
  app-only work.
- #869 adds a new action ("retry with another provider"), so its move to
  wave 9 does not leave a Phase 1 action pending against wave 7's exit
  proof.
- #723 is not a contract unit and has no #900 dependency: summaries stay
  stage-agent-sourced with no daemon-inference call, so #900 (whether daemon
  judgment roles consume lineups) is decided before any utility agent
  exists, not inverted by it.
- pi stays elaboration-only (#895 excludes implementation and review by its
  own terms).

## Supersedes

Revision 40's revisit clause naming a one-unit slip of #924 as the remedy
for Wave 7 exceeding review bandwidth. The slip survives as wave 7's
first-to-slip note; it is no longer the only remedy.

## Revisit When

- Wave 7 planning measures the decision chain and finds it still exceeds
  review bandwidth after the #924 slip: split wave 7 the way wave 9 is
  marked, rather than widening fronts.
- Claude stalls or is rate-limited during waves 7 or 8 long enough to idle
  the real backlog: pull the Codex tail forward by fiat; the hedge argument
  then outweighs the build-once argument.
- The Wave 6 audit files units that belong to none of the four rows: the
  spine places them at wave 7 planning, and a cluster that fits no row is a
  signal the split is wrong, not a reason to widen a row.
