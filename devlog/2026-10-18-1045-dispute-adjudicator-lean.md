# The Review Dispute Shows The Adjudicator's Lean

A person settling a `review_dispute` chose a route for each finding without
seeing what the adjudicator proposed. The dispute fires when a goal-required
answer falls below the confidence threshold, and that demotion threw away the
route the model had proposed. Each dispute row now carries an `adjudication`
block (lean, confidence, rationale, below_threshold), and both clients show it
in the model-proposal unverified register (#2059). The block informs the pick
and never makes it.

Chose to persist the demoted answer's route as `proposed_route` on the
finding-adjudication entry over recomputing it from the raw model output.
The raw output is not stored as a typed record, and the demoted entry is the
authority the dispute already reads. `Validate` admits the field only on a
demoted entry (model producer, goal required, route `park_unknown`) and only
for a route other than `park_unknown`, so it cannot appear on an entry that
acts.

Chose `omitempty` for `proposed_route` over the explicit-null rendering the
golden convention prefers. Finding-adjudication entries sit inside a
digest-bound artifact, and an explicit `null` would change the bytes, and so
the digest, of every artifact already stored. Omission keeps every existing
artifact and golden byte-identical; a new golden pins the demoted shape.

Chose the round's latest model-authored revision as the block's source over
revision 1 or the latest revision of any producer. A discuss revision replaces
the model's answers, so revision 1 can be stale. A human revision is the
dispute's own resolution, never something the dispute could have shown, and
`LatestModelFindingAdjudication` skips it. A human revision's card is the
dispute item itself, so no model revision follows one and the read cannot
recurse into the item it is building. The skip bypasses only the decision
gate, the one check that reads that item: a skipped row still passes its
column agreement, round binding, and chain link to its predecessor, so a
corrupt human row fails closed rather than exposing an older model revision
(a review finding on PR #2067). The read enumerates the whole table and runs
those row-local checks before filtering by the decoded run and round, so a
corrupted key column cannot move a revision out of the read unchecked (a
second finding).

Chose to tolerate a row with no block over backfilling items open at the
upgrade. A row without the block carries no authority, since routes come only
from `offered_routes`, and rewriting a stored item would advance its version
under a person who may be mid-decision. The store gate accepts a nil block as
legacy and re-derives every non-nil block from the stored artifact and the
resolved policy, refusing any mismatch on write and on reconstruction. The
engine compares an existing binding through `ReviewDisputeBindingMatches`,
which ignores only a block the stored row lacks, so a legacy open item keeps
its version and a legacy resolved item is not refused.

Moved the dispatch-threshold resolver to `domain` over keeping a copy in the
engine and the store. The block's `below_threshold` must match the threshold
the engine used, and one resolver is the only way the gate's re-derivation
cannot drift from it.

The lean is never routed. `ReviewDisputeRoutes` takes no adjudication entry,
so the offered set cannot widen, and the signet gate and
`findingRoutesFromDecision` read only `offered_routes`. The clients mark the
offered route equal to the lean and preselect nothing. A remediate lean on a
location outside the declared paths cannot arise: the engine-model producer
needs `allowed` compatibility, which a pure model answer cannot mint, so the
reachable out-of-scope case is a separate-work lean with remediate not
offered, and a test covers it.

## Refute-First Findings

An independent reviewer tried to break the trust boundary. Disproved by
tracing or by test:

- **A later model revision strands an item.** Model revisions after the
  first come only from Discuss on a `finding_adjudication` item, and a round
  holds either that item or a dispute at the revision-1 ID, never both. After
  the dispute exists only human revisions follow, and the read skips them, so
  the write and every later read resolve the same revision.
- **Threshold drift between write and read.** The resolved policy is
  write-once per run.
- **A forged or misplaced block.** A block on a finding the artifact lacks
  derives to nil and is refused; a tampered stored body is refused on
  reconstruction (tested).
- **The match helper masks a mismatch.** It ignores only a block the stored
  row lacks, and works on a clone, so it never mutates the derived binding.
- **The lean becomes executable.** Every route check outside `domain` reads
  `offered_routes`.
- **An existing digest changes.** A nil `proposed_route` is omitted.

Allowed by decision: `Validate` admits `proposed_route` on a demoted-shape
entry whatever its confidence, because the threshold lives in policy, not the
entry. Only `NewDemotedModelAdjudicationEntry` sets the field, and only for an
entry below the threshold; the adjudicator builds entries through
constructors, so model output cannot supply it. A put-time threshold check
was declined as hardening for an unreachable state: the field grants no
route, so a mislabelled lean would misinform but never act.

Revisit when `DefaultDispatchThreshold` changes. A run whose policy omits the
threshold key derives `below_threshold` from that constant, so changing it
would make the store gate refuse every stored dispute item with a block; the
change would need the run's threshold recorded on the item or the policy.

Revisit when the `finding_adjudication` card needs the lean of a demoted
entry it lists. This unit kept the block to the dispute card.
