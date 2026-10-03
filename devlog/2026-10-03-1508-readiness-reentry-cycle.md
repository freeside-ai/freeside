# Readiness Re-Entry Cycle

Issue #502 is the behavior half of re-earning readiness. #496 supersedes a
`ready_for_final_review` item when the pull request's base advances or its
head changes, #1622 defines the authority a re-entry runs under, and #1706
defines what a base-advance re-entry evaluates. This unit starts the cycle,
runs it, and writes the fresh ready item. It changes `daemon/internal/engine`,
`daemon/cmd/freesided`, and `daemon/internal/publish`, and no shared package.

## Owner Decisions (2026-10-02, on #502)

1. **A base-advance re-entry evaluates the prospective merge.** The daemon
   merges the unchanged head `H` into the new base `B'` locally, verifies the
   merged tree, and has the merge reviewed against `B'`. Nothing is pushed.
   Rejected: comparing `B'` with `H` (shows every upstream commit as a
   reversion), waiting for someone to update the branch (readiness never comes
   back by itself), and having the daemon push a merge (a branch rewrite on
   every base advance). The identity is recorded in
   `2026-10-02-2255-prospective-merge-identity.md`.
2. **One unit, over the size budget.** Planning proposed splitting base
   advance from head change. The owner kept them together: both paths share
   the task shape, the cycle, the readiness step, and the restart tests, and
   #1705's `external_review` cycle rides on the same code.
3. **Findings in a re-entered cycle go to a person.** No remediation and no
   adjudicator run. Remediation on the new base would need an admission whose
   base and image are the re-entry base, and every admission takes its base
   from daemon config, which is the old base until a restart.

## Decisions

1. **The trigger runs in the transaction that supersedes the item.** Chose a
   function the two writers call (`engine.StartReadinessReentry`, from the
   base watch and the active-resource reconciler) over a lane pass that scans
   for superseded items with no successor. The store seals the authority only
   against the superseded item's invalidation fact, so the authority must be
   recorded after that write, and one transaction means no restart finds the
   item superseded with no cycle behind it. The second of two writers that
   observed one invalidation finds a newer cycle current and starts nothing.
2. **The trigger skips from reads and fails on writes.** A reason that does not
   re-enter, a completed unit, a cancelled task, a missing binding, and a
   latest review that does not cover the bound head each start nothing and
   leave the item superseded for a person, as before this unit. Each is
   decided before the first write. Any failure after that fails the caller's
   transaction, so the supersession is retried with the re-entry and never
   committed without it. Rejected: swallowing a write failure to keep the
   supersession, which leaves the run permanently stale with no cycle and no
   signal.
3. **The cycle records no candidate authorization.** The store keeps one
   authorization per repository, head, and trust profile, and a base-advance
   re-entry keeps its predecessor's head: a second authorization for `H` would
   collide with, or replace, the one that authorized publishing `H`. The
   cycle's verification result lives in an engine-private checkpoint with its
   own kind and a key that names the cycle, so a first-cycle checkpoint can
   never be read as evidence for a merge. The review gate and the ready item
   read it through an in-memory view that has no ID and is never offered to
   the publication gate. A first cycle compares its checkpoint with the
   stored authorization on reload. This cycle has none, so its reload reads
   the outcome and findings back from the verifier's report among the
   checkpoint's artifacts and refuses a row that disagrees with it.
4. **The cycle takes no publication reservation.** `publish.ClaimInvocation`
   writes a row at the key the store's in-place binding gate probes to prove
   nothing was published under the re-entry's publication ID. A reservation
   would make the cycle's own ready binding fail that gate.
5. **The admission the cycle runs under is synthesized.** The image, trust
   profile, and policy are the predecessor producer's, re-read and compared
   with the run's resolved policy. The base is the one the sealed authority
   names, in place of the base the producer was admitted at. Nothing stores
   this admission.
6. **Stop and hold are different ends.** A head the branch lacks, a head that
   does not descend from the review base, a merge conflict, a head equal to
   the base, an unclean verification, and a tree that verification refuses
   stop the cycle: a durable `publish_blocked` item names the commits (and,
   for a conflict, a bounded, quoted list of paths), the task is retired, and
   no run hold or blocked milestone is recorded. A cause a later pass can
   clear (a transport refusal, a revoked recipe, reviewer configuration)
   holds the task through the existing hold path and is retried. Rejected:
   holding on a merge conflict, which would rebuild the same conflicting
   merge on every pass. Tree-derived text in a stop reason (a conflicting
   path, a refusal's message) is quoted and cut to 256 bytes.
7. **Findings raise the review-dispute item.** The plan named
   `reserveFindingAdjudicationAttention`. That reservation exists to hand the
   finding to an adjudicator, which decision 3 forbids. The cycle raises the
   escalation the review gate already uses when no automatic path remains,
   and ends there.
8. **A head that moves mid-cycle chains in one transaction.** The cycle
   finishes on its sealed head, because #1622's gate seals the next authority
   only from a superseded ready item whose head the latest review covered.
   Before readiness it compares the live head. On a difference, one
   transaction writes the ready item already superseded with a `head_changed`
   fact as its only version, records its binding, starts the next cycle,
   supersedes an open hold item, clears the run hold, and retires the task.
   The store accepts a first version that is already superseded, so the
   discarded item is never open and no ready milestone is recorded for it. A
   force-push that removes the sealed head stops the cycle instead.
9. **A re-entered ready item does not offer return to agent.** The issue
   asks for a typed error before any fetch or replay. Withholding the action
   gives exactly that: the store rejects the command with
   `ErrActionNotOffered` before it is accepted, so the lane never sees it and
   the item stays open. Rejected: offering the action and refusing it in the
   lane. Accepting any decision supersedes the item, so one click would spend
   the re-earned readiness with no invalidation fact behind it, and a
   returned refusal would stop the publication loop on every pass. The
   feedback path keeps a one-clause guard that fails closed on a re-entry
   task, because the code after it reads a replay such a task does not have.
10. **The ready item's diff stats describe the merge and name the head.** For a
    base advance the counted change is base-to-merge, which is what the pull
    request would change on the new base. The stats still name `H`, the commit
    a person sees on the forge.
11. **A tree that verification refuses stops the cycle.** A first cycle
    verifies a tree Freeside imported, so a symlinked command entrypoint, a
    path that cannot be materialized, or an unfaithful materialization is a
    state contradiction and fails the lane. A re-entry verifies a head
    anyone with push access wrote, where the same three refusals are ordinary
    outcomes of the push. Returned as contradictions they stopped the
    publication loop for every run on every pass. They now stop the cycle
    with the refusal quoted. `ErrWorkspaceMismatch` can also come from the
    host, and the stop is accepted for that case: the item names the cause
    and a person decides. Every other verification refusal still fails the
    lane.
12. **A stop withdraws the cycle's own open ready item.** A crash after the
    ready item is written leaves the task pending. If a force-push then
    removes the sealed head, the next pass stops the cycle; without the
    withdrawal the run showed an open ready item beside the stop. The stop's
    final transaction supersedes it.

## Trust Boundary: Fetching a Head

`FetchHead` reads a branch anyone with push access can write, and the engine
trusts its verdict. The refute-first pass on it found:

Confirmed and fixed:

- Any `rev-parse` failure read as "head missing". Presence is now read only
  from exit status 1 of `rev-parse --verify --quiet`; a git process that dies
  or is cancelled stays a transport failure.
- Ancestry was checked against the fetched ref, not the observed tip. It now
  uses the tip.
- A `merge=union` line in the checkout's `info/attributes` turned a conflict
  into a clean merge. `ProspectiveMerge` refuses a checkout that has the file.
- A head equal to the base produced a one-parent commit. It is refused.
- Coverage gaps: failing-git cases for presence, ancestry, and `merge-tree`
  exit 2, and stamp and repository-binding mismatches, now have tests.

Allowed by decision:

- Unrelated histories and a branch `fsck` refuses stay retryable transport
  failures. The lane records a hold, so the cause is visible.
- Concurrent `FetchHead` calls on one checkout are not guarded: each pass owns
  its checkout, and the method documents that.

Disproved by a check: attributes carried in the tree, exit status 1 without a
conflict, a merge SHA that varies under a hostile environment, output
truncation, ref residue after the fetch, tag peeling, a token or stderr in an
error, token minting before the local gates, and option-like or glob branch
names.

Not verified: the size of a fetch is unbounded.

`ErrRemoteMissingHead` also matches `ErrGitTransport`, so a caller must test
for the missing head first. The engine does.

## Pre-Push Independent Review

A fresh-context reviewer read the full diff against the intended outcome.

Confirmed and fixed: tree-caused verification refusals wedged the lane
(decision 11); return to agent destroyed re-earned readiness (decision 9); a
stop after a crash left the ready item open (decision 12); a conflict path's
length was unbounded (decision 6).

Disproved by the reviewer's checks: evidence for one commit accepted for
another, two writers starting two cycles, a cycle evaluating a SHA other than
the sealed one, attributes or config turning a conflict clean, a token in an
error or item, an escape through workspace materialization, divergence after
a restart at each seam, a damaged task row being trusted, and first-cycle
regressions.

Not verified: the engine tests drive a transport double. The real
`publish.Transport` accepts only an https remote outside its own package, so
its checkout re-gate after the cycle's workspace steps, and the second
`FetchHead` before readiness, are covered by `internal/publish` tests and by
the first cycle's identical gate in `PushHead`, not by a re-entry through the
engine.

## Automated Review on the Pull Request

Confirmed and fixed: the reloaded checkpoint's outcome and findings were
trusted as decoded, with no second record to compare them with (decision 3).
A cycle that was held, repaired, and then ended on review findings left its
hold item and run hold open with no task behind them; the escalation now
ends both.

Declined: authenticating the checkpoint's artifact list against an edited
row. The row is write-once and no code path rewrites it, the list gates
nothing (the verdict is read from the report), and an independent record of
the list would be a store change.

Deferred: a crash after the ready item is written, followed by a pass that
holds the task, leaves the item open beside the hold with no binding and no
armed watch. The first cycle's tail has the same window, so the fix belongs
to the shared tail. Withdrawing the item on a hold was rejected: the hold is
retried, the item's ID is fixed, and supersession is final. Follow-up: #1745.

## Consequences Worth Knowing

- **A plain push after a base-advance cycle stops the run.** A head-change
  cycle reviews against the prior review's base. After a base-advance cycle
  that base is `B'`, and a later commit pushed on the old branch does not
  descend from `B'`. This follows the issue's head-change rule as written.
- **A stopped cycle does not restart when a later push fixes the cause.** The
  superseded item already has its successor, so no writer starts another.
- **A head change on a closed or merged pull request also re-enters.** The
  reconciler records `head_changed` whatever the pull state. The cycle then
  stops on a deleted branch or re-earns readiness for the merged head.
- **Every cycle spends a review round.** The cycle's round is the prior
  review's plus one, and the shared review gate applies the run's resolved
  hard limit. A base that advances often ends the run on the existing
  "Review exhausted" item. Read from the code; no test drives a run to the
  limit through re-entries.
- **A head-change cycle can be followed at once by a base-advance cycle.**
  It reviews against the prior review's base and arms the watch with that
  base. If the base branch has already moved, the watch's next tick
  supersedes the new ready item and a second full cycle runs.
- **A rebuilt merge that differs from the verified one fails loud.** The
  engine rebuilds the merge on every pass and compares it with the
  checkpoint. A difference with the same base and head is a contradiction
  (`ErrParentKeyMismatch`) and stops the lane.

## Revisit When

- A mid-cycle head move becomes common. It costs a full verification and
  review of a head that is already stale, because the store's gate needs that
  review record. Cutting the cycle short needs a store contract change.
- People push to a pull request branch after a base-advance cycle and the stop
  proves to be the usual outcome. The fix is a rule for which base a
  head-change cycle uses, which is #1622's contract.
- A git upgrade changes `merge-tree` output between a cycle's verification
  and a restart. Today that stops the lane; a re-verify path would be the
  alternative.
- Re-entering on a closed or merged pull request costs more reviews than it
  is worth. The trigger could skip those states.
- Admissions can take their base from the cycle. Then decision 3 and the
  withheld return-to-agent action can be reopened.
- Re-entries exhaust the review round limit in practice. A separate budget
  for re-entry rounds is the alternative.
