# Stop, Not Retry, on a History Refusal

Work unit: #3166. Source: finding 2 of
`devlog/2026-11-12-1730-verify-command-history.md` (#3154).

The engine now treats `verify.ErrHistoryUnavailable` as a refusal a retry
meets again. `productionVerificationStateContradiction` names it, so neither
verification call site wraps it as retryable, and `unverifiableTreeCause`
names it, so a re-entry or handoff ends its cycle on it with a reason a person
can read. Before this, both call sites wrapped it as retryable, and every
retry met the same commits.

## Where the Refusal Is Reachable

The verifier refuses a history request when the head and the base it is
verified against share more than one merge base. Planning corrected the
#3154 note's reading of where that can happen:

- **Handoff merge check: reachable.** The handoff verifies the head against
  the moved target tip (`production_handoff.go` sets the cycle's base to
  `check.TipSHA`), and the head need not descend from that tip. A pushed head
  that merged a commit the tip also merged, where that commit and the
  admitted base are unrelated, gives two merge bases.
- **Re-entry: not reachable today.** `reconcileReentryTask` stops any head
  that doesn't descend from the admitted base before it verifies, and
  `verifyReentry` verifies against that same base. When the base is an
  ancestor of the head, it is their only merge base.
- **First cycle: not reachable.** The importer builds the candidate on the
  admitted base, so the base is the only merge base.

## Decisions

- **Chose to stop the lane on a first cycle over ending the cycle there.**
  A first-cycle refusal can only mean Freeside's own state is wrong, the same
  reasoning that already stops the lane on `verify.ErrMalformedTree`. Ending
  the cycle would present a Freeside fault as the candidate's.
- **Classified the re-entry path although it is unreachable.** Both call
  sites share `productionVerificationStateContradiction` and
  `unverifiableTreeCause`, so naming the refusal once covers both. If a later
  change verifies re-entry against a moved base, the path becomes reachable
  and already ends the cycle rather than retrying or stopping the lane.
- **Kept a failed `git merge-base` a lane error.** That refusal wraps
  `verify.ErrHistoryUnavailable` and `verify.ErrGitPlumbing`. Git failing is
  not the head's doing, so `unverifiableTreeCause` excludes it; it was already
  non-retryable through `ErrGitPlumbing`, so no transient failure becomes
  newly terminal.
- **Kept the tree wording for the `.git` path refusal.** That refusal wraps
  both `verify.ErrHistoryUnavailable` and `verify.ErrMalformedTree`, and the
  path is a tree problem, so the malformed-tree case is checked first.
- **Reused the handoff's existing account.** `handoffStopAccount` ends a
  handoff on any cause `unverifiableTreeCause` names, so the handoff blocks
  with `Handoff merge cannot be verified:` and the history cause without its
  own change. Its closing advice ("change the pull request so the merged tree
  can be verified") fits loosely: the fix is to rebase or rework the head's
  history.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **Mis-wrapping or caller order: disproved.** A failed `git merge-base`
  wraps a git error whose class is `verify.ErrGitPlumbing` for every failure,
  cancellation included, so the exclusion holds. At re-entry,
  `reentryUnverifiableTreeReason` is the first classifier after
  `verifyReentry`; at a handoff, the checks that run before
  `unverifiableTreeCause` match other error types only.
- **Reachability: confirmed** as stated above. Fetches are not shallow, so
  the merge-base count can't change between attempts.
- **New terminal transient failure: disproved.** The only newly
  non-retryable shape is the merge-base count, a fixed property of the
  commit graph for a given head and base.
- **"The base" read as the admitted base at a handoff: confirmed, fixed.**
  The cause now names "the commit it is verified against", which is the tip
  at a handoff and the admitted base at re-entry.
- **Wording assumes "more than one" while the verifier also refuses zero
  merge bases: declined.** Git exits non-zero when there is no merge base,
  so that case arrives as the `verify.ErrGitPlumbing` refusal and never
  reaches this wording.
- **Tests call the classifiers, not the callers: allowed.** This matches
  the existing tests for every other stop cause; a reorder in a caller would
  not be caught, as for those causes today.

Revisit when the verifier learns to give history across several merge bases,
or when re-entry verifies against a base other than the one its descent check
uses.
