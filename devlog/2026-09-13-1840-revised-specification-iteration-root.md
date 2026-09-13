# Revised-specification campaign: iteration continuation collides with the iteration-1 root invariant

Work unit: #1083 (implementation-identity for a revised specification after a
blocked run). This note records the design finding that forced an owner
decision on the iteration/root conflict, the decision (Option A), and the
reconstruction-boundary rationale that followed from implementing it. The
decision is settled and implemented; the note freezes when PR #1341 merges.

## Finding

Plan decision **D3** ("iteration numbering continues from the prior
specification run's accepted iteration", so the fresh campaign's specification
run's first request lands at iteration `N+1`) is **not implementable as
written** without either loosening the `SpecRevisionFacts.Iteration >= 2`
contract (which D3 forbids) or a large, risky rewrite of the specification
authentication core.

The specification subsystem assumes, pervasively, that **a specification run's
root marker is at iteration 1**. `verifySpecificationChain`
(`daemon/internal/engine/specification.go`) reconstructs the entire iteration
history from `GetOutbox(specificationInvocationID(runID, 1))` and authorizes
every accepted specification output through that chain. A revision run whose
first (and possibly only) invocation is at iteration `N+1` has no iteration-1
marker, so the chain read returns `ErrNotFound` and **the revised specification
can never be accepted** — which is the unit's central acceptance criterion
("once the revised specification is accepted ... starts exactly one new
implementation run").

The assumption is not localized. At least eight non-test sites hardcode the
run's root at iteration 1: `verifySpecificationChain`,
`sameSpecificationRoot`/root reads, `acceptResearchRequests`, the
`ResolveSpecificationRunID`/`HasSpecificationDispatchMarker` helpers,
`production_workflow.go`'s intake-state probe, `loadReattemptInputs`
(`production_attempt.go`, which the plan's step 3 reuses), and the store's
`authenticateInitialAttemptSource`. The plan's risk note anticipated "three
places" (the first-marker lookup, the `MaxIterations` arithmetic, and the card
text); the real surface is a core invariant spanning the specification
authentication chain.

## Why this is an owner decision, not an implementer choice

The plan itself says: "If the count proves too entangled, stop and report
rather than loosen the contract." It also forbids loosening
`SpecRevisionFacts.Iteration >= 2` and forbids changing `docs/plan.md` §5.12.
The resolutions all touch settled decisions or a high-risk authentication
rewrite:

- **Option A — synthetic iteration-1 root.** The new spec run "adopts" the
  prior approved specification as its iteration-1 state, and the specifier's
  first real output is a revision at iteration 2. Preserves the iteration-1
  root invariant and `Iteration >= 2`, and gives accurate lineage. Cost: design
  how a run adopts a prior spec as a bare iteration-1 terminal/marker without an
  invocation, and confirm the chain reconstruction accepts it. Does not match
  D3's literal "continue from N" when the prior campaign itself had `N > 1`
  (the card would read "revision 2", not "revision N+1").
- **Option B — first-iteration-aware authentication.** Every root-marker lookup
  discovers the run's actual first iteration instead of assuming 1, and the
  `MaxIterations` budget offsets by it. Matches D3 literally. Cost: invasive,
  correctness-critical edits across ~8 authentication sites in the most
  sensitive part of the daemon; high regression risk.
- **Option C — drop card-level revision lineage for the first output.** The new
  campaign's first approval carries no `SpecRevisionFacts`; the revision lineage
  is carried only by the `RevisesRunID` link and the task timeline. The run
  starts at iteration 1 like any fresh run. Cheapest and lowest-risk, but
  contradicts D3's "the revised approval card shows the revision lineage".

## Decision (owner, 2026-09-13): Option A

The owner chose A. Tracing `verifySpecificationChain` settled its concrete
realization, which is more bounded than the "small mechanism" the options
sketch implied and needs no fabricated agent state:

- The revision campaign's specification run has **no iteration-1 marker**. It
  roots at **iteration 2**, whose request is the revision itself: inputs
  `[source, priorSpec, feedback]`, `PriorSpecArtifactID` = the blocked
  campaign's approved specification artifact (a cross-campaign reference),
  `FeedbackArtifactIDs` = `[spec-feedback-<answerCommandID>]`. The prior
  approved spec is the conceptual "revision 1" baseline; the specifier's first
  pass is "revision 2".
- `verifySpecificationChain` starts its walk at the run's first iteration (2
  for a revision run, 1 otherwise), seeds `priorSpec`/`feedback` for the
  revision root, and **authorizes that root against the `RevisesRunID` link**
  (prior spec bound by digest to the blocked attempt's `ApprovedSpecDigest`;
  feedback bound to the answer command) instead of a preceding
  `request_changes`. `verifySpecificationOutput`/`verifySpecificationApproval`
  are unchanged: the iteration-2 specification is genuinely agent-produced.
- The iteration offset is a fixed `+1` for revision runs (not the prior
  campaign's `N`), carried as an internal `first_iteration` field on the
  specification request so the `MaxIterations` budget counts the revision run's
  own iterations. `SpecRevisionFacts.Iteration` = 2 (>= 2 holds); the card
  reads "Revision 2, supersedes revision 1"; the single answer comment sits at
  iteration 1.

Rejected: **B** (make every root lookup discover a dynamic first iteration),
which rewrites the authentication core for a numbering property; and **C**
(drop on-card lineage), which is no cheaper because it still needs the
iteration-2 predecessor to carry the answer to the specifier.

Divergence from plan **D3**: the card shows "revision 2", not "revision N+1"
continuing the prior campaign's count. The prior campaign's own revision
history stays on its cards and is reachable through the task timeline (D3's
own provision), so no lineage is lost.

## What landed on the branch

The foundation every resolution needed, and which Option A builds on:

- `domain.ProductionAttempt.RevisesRunID` / `RevisionCommandID` with
  both-or-neither, initial-only validation (D2), plus goldens.
- Store gates: the write- and reconstruction-time revised-run gate
  (`gateRevisionRevisedRun`), the `campaignTask` cross-campaign task derivation,
  and `authenticateRevisionAttemptSource` + `firstSpecificationMarkerKey` for
  the seeded first request. These are dormant until an attempt sets the link.
- Engine: `SubmitSpecificationRun` refactored to the contract's
  transaction-scoped variant (`submitSpecificationRunTx` + `specificationFirstRequest`
  + `specificationRunSeed`), behaviour-preserving for the operator and
  issue-subject arms.

The rest of Option A then landed on the same branch: the
`revise_specification` dispatch, `enqueueSpecificationRevisionCampaign`, the
card-lineage derivation, the `MaxIterations` offset, signet route acceptance
and projection, and the API/app changes.

## Reconstruction boundary: why the revision gates scan, not recurse

The revision link crosses campaigns, and unlike the parent-attempt chain it is
not bounded by decreasing attempt numbers. Authenticating the revised run by
recursing into its lineage from the revision gate therefore admits a cycle: two
mutually referencing (forged) rows exhaust the stack instead of failing closed.
The store gates (`gateRevisionRevisedRun`, `authenticateRevisionAttemptSource`)
deliberately read the revised run with the non-authenticating
`productionAttemptByRun` scan and validate only the revision attempt's own
bindings (source digest; prior spec bound by digest to the revised run's
`ApprovedSpecDigest`; feedback provenance; command/item decision). Not recursing
eliminates the stack-exhaustion mode outright.

This is safe because two paths read the link, and the one with side effects
re-authenticates. The engine's `verifyRevisionRoot`, gating the start of the
new implementation run, loads the revised run through
`GetProductionAttemptByRun` (the authenticating reconstruction), so a forged
revised-run lineage is caught before any run starts. The signet run sync
(`runProjectionFactsFor` via `RevisionSpecificationRunFor`) also reads the
write-once `revises_run_id` column to project `superseded_by` onto the revised
run, without that engine gate. That projection is display-only: it trusts the
store gate that validated the link on write and reconstruction, and a forged
or corrupted row can at worst mislabel a terminal run's successor on the run
list. It starts no run and loses no data. The confidence boundary is that
every path with side effects re-authenticates; the display projection does not.

Codex flagged the non-recursive scan across review rounds 3 and 4 as "skipping
the gate rather than failing closed" and asked for recursive predecessor
authentication plus a visited-set. Declined: that re-introduces the exact
traversal the cycle fix removed, to re-authenticate a run already authenticated
on the acting path, and is defense-in-depth for a forged-row-only mode with no
reachable failing state. Two adjacent findings on this boundary were real and
were fixed: `first_iteration` is pinned to exactly `{absent, 2}` (a revision
always roots at iteration 2), and both the engine and store feedback-artifact
gates pin the full daemon-minted provenance tuple rather than a subset.

## Revisit when

A path is found that acts on the revision link with side effects beyond the
display-only `superseded_by` projection without independently authenticating
the revised run through `GetProductionAttemptByRun` (which would turn the
non-recursive store scan from defense-in-depth into a real gap), a consumer
starts acting on that projection, or a second-order revision (revising an already-revised campaign's implementation)
needs a first iteration other than 2.
