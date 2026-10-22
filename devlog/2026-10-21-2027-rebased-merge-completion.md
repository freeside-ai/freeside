# Rebased-Merge Completion

Issue #2129. A work unit whose published pull request a lander rebased and
merged now completes. The `head_changed` re-entry proves the rebase and
records the verdict; completion accepts a merge of a proven head.

## Decisions

- **Prove the rebase rather than review the new head or carry proofs to
  it.** Planning chose this on 2026-10-21 and implementation kept it. A
  proof needs three things. The head descends from the reviewed base. `git
  range-diff` pairs every reviewed commit as `=`. The head's tree equals
  the prospective merge the review covered. Together they say the merged
  content is the reviewed content, which is all completion needs. A new
  review would spend a round on content already judged, and it could not
  finish before the lander merges. Carrying verification and review proofs
  to the new head would make it ready again, a wider trust-boundary change
  that buys nothing for completion.
- **Readiness stays bound to the reviewed head.** A proof moves only the
  head the completion evaluator compares (`EffectiveWorkUnitPRBinding`). It
  writes no ready item and admits no remediation. The ready and held
  bindings keep the reviewed head, and `WorkUnitBindingCovers` lets a
  consumer accept the pair (reviewed ready head, proven effective head). The
  consumers are the capture pass, the reconciler, the task record, and the
  sync boundary's completion check.
- **The proven bit is re-gated on every read.** A stored `Proven` row moves
  the head only while all of these hold: its reviewed pair is still the
  latest review record's, its reviewed head is still the published head,
  and it names the unit's pull request. A later review or publication
  silently retires an old proof; nothing deletes it.
- **`range-diff` alone is not a proof.** A fresh-context review found two
  blind spots, both reachable by anyone who can push to the branch.
  `range-diff` walks without merge commits, so a merge commit could add
  content it never lists, and it prints "Binary files differ" for any
  binary change, so a rebase could change a binary file and still pair as
  `=`. The final tree check catches neither when a later commit restores
  the reviewed tree. The proof now refuses a rebased range with a merge
  commit, requires the pairs in reviewed order, and compares each pair's
  per-path contents and modes wherever both start from the same contents.
  A path the base changed is left to `range-diff` and the tree check,
  since its contents legitimately differ after a rebase.
- **`=` only; a reflowed message is unproven.** `range-diff` marks a reworded
  message `!`, the same as a changed patch, and the proof does not try to
  tell them apart. A lander that rewrites messages gets a review, which is
  the safe failure.
- **A failed fetch is not a verdict.** When the remote no longer serves
  either head, the cycle records nothing and reviews the head as before. A
  refusal is recorded, so a merge of that head forecloses at once instead
  of waiting.
- **The foreclosure waits for the cycle, bounded by the cycle.** A merge
  seen before the `head_changed` cycle for exactly that head has a verdict
  would otherwise foreclose a head the cycle could prove. The reconciler
  holds the foreclosure while that cycle has no proof row and its task is
  undispatched. The cycle proves first and then ends quietly on a recorded
  merge, so the wait normally lasts one pass. A cycle that retires without
  a verdict releases the hold. A pass that keeps failing keeps the hold, by
  choice: that failure is loud on every lane pass, and foreclosing under it
  would race the verdict the cycle has yet to reach. A time bound would
  trade that race for a timer nothing else in completion uses.
- **An optional transport seam, not new required methods.** The issue named
  `FetchPullHead` and `ProveRebase` on `engine.PublicationTransport`. They
  live on `engine.RebaseProofTransport`, asserted by interface, as
  `ProspectiveMergeOnto` and `UpdateHead` already are. A transport without
  the seam reviews the head as before, which is the fail-safe path, and the
  many publication fakes need no stub that could prove by accident. The
  production `GitPublicationTransport` is asserted to implement it.

## Findings

- **GitHub serves the heads a lander leaves.** `refs/pull/<n>/head` persists
  after the head branch is deleted, and a commit a force-push replaced stays
  fetchable by SHA. So the cycle can fetch both heads after the lander
  deletes the branch. The transport fetches the new head through the pull
  ref and requires it to be an ancestor of that ref's tip. If GitHub stops
  serving the reviewed head by SHA, the fetch fails as a class
  (`ErrReviewedHeadUnavailable`), and the unit is reviewed or foreclosed,
  never wrongly completed.

Revisit when a lander in use rewrites commit messages, or when GitHub stops
serving replaced commits by SHA.
