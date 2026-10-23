# Retire Closure Cards on Recorded Facts, Not Completion (#2218)

Work unit: #2218. Scope: `daemon/internal/engine`,
`daemon/internal/store/effect_proposal.go` with its tests, and `devlog/`.
The sweep closes human-gate cards on its own, and the adversarial review
before push changed its implementation, so the note is mandatory.

## Decisions

**Key retirement on the observers' recorded facts, over the task's
completion.** The gh-imgup#117 unit can't record completion (its merged
head differs from its bound head on a pre-#2134 binding), so a
completion-based trigger would never reach its three stale cards.
`completionSupersedes` keeps exempting `effect_proposal`; the new sweep
reads the latest `PullMergeFact` for the unit's PR and the bound issue's
latest `IssueStateFact` instead.

**Ignore a closed issue fact observed before the unit's PR binding, over
trusting the latest fact.** Issue facts are keyed by repository and issue,
not by unit, and the observers write one only after a bound unit's merge.
So when an earlier unit closed issue #N, #N was reopened, and a new unit
was bound to it, the latest fact still says closed. Trusting it would
retire the new unit's gate card at once. The closure wait would then read
Decided, and the publisher would resolve Declined and mark the PR ready
without asking anyone. Found by the fresh-context review; reachability
traced through intake's `BoundIssue`, not run end to end.

**Decide in the sweep's read, over a write that replays.** The engine
reconciles every 100ms, and a gate card can wait hours. A check inside
`store.Write` would hold SQLite's one writer slot on every pass for every
held card. Now only a unit with a settling fact opens a write.

**Read the immutable PR binding, over the effective one.** The sweep uses
only the repository id and PR number, which a successor can't change
(`effectiveWorkUnitPRBinding` refuses a mismatch). The effective read adds
successor and rebase-proof reads whose failures would log on every pass.

## Disproved in Review

- A merged or closed PR with the ready item still open does not stop the
  task durably: Hold and Declined both render `Refs #N`, so the publisher
  accepts the non-open PR.
- Admission retiring the new instance's own card can't happen: that card
  opens later in `bindClosureMerge`, and the checkpoint written in the
  admission transaction keeps a replay from admitting again.
- A closure card with no work-unit declaration can't exist:
  `ResolveProposalSubject` needs the declaration to admit.

## Revisit When

The observers start polling issue state before a merge, or key issue facts
by unit. Either change alters what a stale closed fact can mean.
