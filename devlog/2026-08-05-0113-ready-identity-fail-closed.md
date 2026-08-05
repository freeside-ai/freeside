# Fail Closed When Ready PR Identity Diverges (#514)

Revises the foreign-identity decision in
`2026-08-04-1756-invalidate-ready-review-state.md`; that merged note remains
frozen history.

## Chose: Withdraw Readiness on Identity Divergence

Chose to atomically supersede a `ready_for_final_review` item when a successful
active-resource observation returns a repository ID or PR number different
from the item's immutable binding, over leaving the item actionable because
the foreign observation cannot prove what happened to the bound PR.

The changed assumption is that readiness is a standing claim requiring
continuous observability of its subject. Losing the daemon's only provable
route to the bound identity invalidates that claim even though it proves
nothing about the foreign object. Supersession withdraws Freeside's stale
claim; it is not a terminal assertion about either pull request. This chooses
safety over the bounded liveness cost: readiness can be re-earned through
#502 or a new run, while a false-ready claim can authorize a human action.

The item is superseded, its version increments, the `identity_changed` fact is
recorded, and its publication schedules conclude in one transaction. Existing
submission-time and persistence-time status/version re-gates therefore stale
prepared commands without a new command-side mechanism.

## Chose: Persist No Foreign Pull Fact

Chose to compare repository ID and PR number immediately after the successful
pull observation, before constructing or validating `PullMergeFact`, over
appending the suspect observation under either the bound or foreign fact key.
The resulting observation carries only the bound item identity and a readiness
invalidation whose `bound` and `observed` coordinates use
`repository_id#pr_number`. Native-review, issue, completion, and material-change
paths do not run.

At the bound-issue completion recheck, identity divergence also invalidates in
the current pass rather than returning an error for another poll interval. The
earlier exact bound-pull observation remains a valid fact, but the intervening
issue observation and completion are dropped because path identity is no
longer stable enough to authorize them.

## Refute-First Dispositions

- **Confirmed, accepted by decision, griefing/liveness:** repository or
  organization administrators, namespace reuse, GitHub routing, or a faulty
  observation channel can influence path-to-ID resolution. The blast radius is
  limited to the bound ready item and its item-derived schedules. The path
  performs no foreign mutation, never resolves the item, creates no completion
  evidence, and persists no foreign pull fact. The bounded cost is re-earnable
  readiness.
- **Confirmed, accepted by decision, transient anomaly:** one successful
  mismatching observation supersedes immediately. Retry or backoff was rejected
  because it preserves an actionable claim after positive divergence. Ordinary
  observer errors and malformed non-positive returned identity coordinates
  remain retryable without state churn. Positive coordinates are validated
  before either identity comparison, including the completion recheck. A rename
  or redirect that still returns the bound numeric repository ID and PR number
  does not invalidate; a focused test pins that non-goal.
- **Rejected by verification, foreign authority:** a mechanical sweep found
  that the first mismatch exits before fact, native-review, issue, completion,
  or material construction. Commit skips the nil pull. At the completion
  recheck, every non-identity pull field and the issue response are poisoned by
  tests; only repository ID and PR number reach the invalidation strings, and
  no foreign pull, issue, or completion is persisted.
- **Rejected by verification, same-identity regression:** old and new
  decisions are identical after repository ID and PR number match. The
  head-change, retarget, exact-close, merged-bound-issue, and base-advance
  corpus passes unchanged; the pointer used to make a foreign pull
  unrepresentable at commit is behavior-neutral on those paths. An additional
  terminal-settle row for already-superseded items was rejected as redundant:
  the commit-race test directly proves an already-superseded item wins while
  its schedules still conclude, and the base-advance scheduler tests cover its
  originating path.

## Revisit When

- #502 implements automatic re-entry and can reduce the accepted liveness cost
  without weakening immediate invalidation.
- A trustworthy identity-change observation channel can distinguish a moved
  bound PR from namespace reuse; it may enrich recovery, but must not restore a
  stale ready claim without fresh verification and review.
- The deferred execution-time exact-identity recheck before `open_pr` is
  scheduled as defense in depth.
