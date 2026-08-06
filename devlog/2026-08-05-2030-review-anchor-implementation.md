# Re-anchoring the §7 Review Stage Pre-Publication (#527)

**What.** The production publication engine now runs the required §7
review between verification and publication:
`implement -> verify -> review -> clean: publish`. The review gate moved
out of `completePublishedTask` (post-publication) into `reconcileTask`,
directly after the authorization gate and before any branch push or PR
creation. Implements the anchor resolved by plan revision 28
([2026-08-05-1746-review-anchor-pre-publication.md], PR #526); that note
carries the anchor rationale and revisit-when condition and is not
restated here.

## Decisions (from the issue plan, unchanged in shape during implementation)

1. **Base advance after a clean review is owned by the publisher's
   exact-base gate, not a review dispute.** Pre-publication nothing on
   the forge can go stale, and the publisher already refuses a moved
   base (`ErrTargetBaseAdvanced -> productionBlockBaseAdvanced`). The
   gate-side `observeBase`/`observePull` staleness checks
   (`reviewPassStaleReason`) and their config/struct plumbing were
   deleted. A base advance is now an ordinary blocked task, not an
   `AttentionReviewDispute`.

2. **A published run reaches readiness only through a clean,
   candidate-bound review record; it fails closed otherwise.**
   `completePublishedTask` no longer re-runs the full review loop; it
   calls `assertReviewedCandidate`, which fails closed unless the latest
   review record is clean, bound to the exact base+head, produced under
   the current reviewer configuration and instruction authority, and
   authoritative over any failure round. A run published under the
   retired post-publication order (no clean record) blocks for manual
   operator disposition. Expected live population: zero (run 482, the
   only production run, ended `publish_blocked` with no PR).

3. **Escalation before publication carries no PR.**
   `completeReviewEscalationTask` dropped its `publish.Result` param;
   the terminal outcome's `prNumber` stays 0. The durable AttentionItem
   (`PRHeadSHA = task.HeadSHA`, a valid pre-publication head SHA) is the
   operator surface.

## Rejected / not-done

- Commits 1 and 2 from the plan were merged into one: dropping
  `published` from `reconcileReviewGate` forces removing
  `reviewPassStaleReason` in the same change, so the seam between them
  was artificial.
- No new "afterReview" crash seam was added; the record-replay
  crash-recovery test uses a transient publishing-push failure as the
  crash between the review pass and publication.

## Refute-first verification (returned-object trust boundary)

A fresh-context adversarial reviewer was tasked to disprove six break
hypotheses. Outcome: no blocker; all six rejected-by-verification.

- **Rejected (could not be substantiated):**
  1. Publish/push without a clean candidate-bound review. Every path
     from authorization to `PublishExecutionAfterGateAndFinalize`/
     `PushHead` is gated; the two crash-recovery fast-paths route
     through `assertReviewedCandidate`, and the pushed head equals the
     asserted head.
  2. Crash-recovery divergence. Replay is deterministic
     (`composeReviewInstructions` is stable for a fixed base), so a
     recovered pass replays the clean record without a new round and
     re-derives readiness idempotently.
  3. `assertReviewedCandidate` too weak/strong. Its
     `failure.Round >= record.Round` block is the exact complement of
     the gate's own pass condition; "too strong on recomposed
     instructions" is unrealizable for a same-base recovery.
  4. Dead code / broken invariants from the `reviewPassStaleReason`/
     `observeBase`/`observePull` removal: grep-clean; the retained
     `observeBaseTip`/`observePull` serve the scheduler's §5.18
     merge-capture and base-advance watch, a different consumer.
  5. Enum switch non-exhaustive: all three members handled; a future
     member is caught by the `exhaustive` linter.
  6. Escalation terminal path: `prNumber` 0 matches the
     `completeBlockedTask` convention; `result.lastPR` is set only when
     `prNumber > 0`, so no consumer breaks; idempotent via the shared
     `recordCompletedTerminal`/`finishTask` helpers.
- **Accepted-by-decision:** decisions 1-3 above (base-advance owned by
  the publisher gate; fail-closed readiness re-gate; PR-less
  escalation), confirmed load-bearing and sound by the review.
- **Minor findings, both addressed in the same change (not deferred):**
  a direct forge-count assertion on the true-`StatusPending` branch
  (`TestProductionPendingReviewPublishesNothing`), and a comment-
  precision fix so the fail-closed doc does not read as if a fully-ready
  old-order run (which does carry a clean record) would block.

### Codex round 1 (rejected-by-verification)

Codex raised two P1 findings on the crash-recovery readiness path; an
independent refute pass tried to construct a shipping bug on each and
found none. Both declined; no code change.

1. *Recheck the live base before restoring readiness* (recovery path
   into `completePublishedTask`). Asks to re-add the exact live-base
   check decision 1 removed. Post-publication base advance is owned by
   the §5.16 base-advance watch, which binds `admission.Base.BaseSHA`
   and re-arms idempotently on re-entry through the `readyExists`
   branch; the PutItem->arm window is transient and self-healing (no
   terminalizing return between them). Changing already-published-PR
   invalidation is an explicit #527 non-goal, and re-adding the check
   re-couples what decision 1 decoupled.
2. *Revalidate persisted review-request authority on recovery*
   (`assertReviewedCandidate`). The readiness path trusts the immutable,
   body-digest-authenticated `ReviewRecord` (written only after
   `VerifyRequestAuthority` and `Verify` passed pre-publication),
   disjoint from the request journal, and re-checks every binding field
   it needs (configuration digest, instruction digest, base, head;
   recipe approval re-gated separately in `completePublishedTask`).
   `VerifyRequestAuthority` uniquely guards journal-driven review
   *relaunch*, which this path never performs. Per decision 2's
   returned-object-trust-boundary design.

### Codex round 2 (confirmed-and-fixed)

One P2 finding, confirmed and fixed: *Preserve the checkout when review
completes inline* (`ensureReviewWorkspace`). The gate moved the
publication checkout into the durable review workspace (`os.Rename`) and
deleted it after a clean result; on a synchronous review source that
completes inline within the same pass, the subsequent `PushHead` then
consumed a moved-and-deleted checkout. Not reachable with the production
async Codex source (its `Inspect` returns pending right after
`RequestReview`, ending the pass before publication), so it is a latent
regression this re-anchoring introduced on the synchronous-source path,
masked by the integration transport whose `PushHead` never read
`checkout.Dir()`.

Fix: `ensureReviewWorkspace` now *copies* the checkout into the workspace
(staged temp dir + atomic rename; `os.CopyFS` preserves exec bits and
symlinks) instead of moving it, so the publication checkout survives at
its original path for `PushHead`. The async path is unchanged (its
scratch checkout is still cleaned by the pass defer while the durable
copy persists). The integration transport's `PushHead` now stats
`checkout.Dir()`, modelling the real transport, so every single-pass
publish test is regression coverage (verified red without the fix,
green with it).

Record correction: the pre-commit refute pass's implicit assumption
that the publication checkout is available at `PushHead` held only for
*async* sources (a later pass re-fetches a fresh checkout); the copy fix
makes it hold for synchronous inline completion too.

### Codex round 3 (confirmed-and-fixed)

One P1 finding, confirmed and fixed: *Convert review mismatches into a
durable hold*. `assertReviewedCandidate`'s fail-closed path returned
`ErrParentKeyMismatch`, which is non-retryable, so the reconciler joined
and returned it and `runReconcileLoop` exited the whole
production-publication lane: the promised decision-2 manual disposition
was never created and every other queued publication stopped. A lane-
fatal error, not the operator-visible block decision 2 specifies.

Fix: the re-gate stays fail-closed (predicate and its field checks
unchanged, current-config-digest included), but the failure is now task-
scoped. On `ErrParentKeyMismatch`, `completePublishedTask` holds the
single run instead of returning the error, so reconcile returns nil, the
lane keeps advancing, an operator-visible AttentionItem is created, and
readiness is never silently derived. A store read failure from
`latestReviewState` still propagates for retry (only the predicate
mismatch blocks). (Round 4 below settled the exact disposition mechanism;
this round's fix was the lane-crash → task-scoped block.)

### Codex round 4 (confirmed-and-fixed)

Two findings on the round-3 fix, both valid; one consolidated round with
a rising bar (further adjacent non-blockers deferred to a follow-up).

**Finding 5 (P1) — recovery re-gate was weaker than the gate it replaced.**
`assertReviewedCandidate` compared the record's `ConfigurationDigest`
only against the daemon's *current* `reviewConfigurationDigest`, never
against the trust profile's approval of it. Unlike the pre-publication
gate (old lines ~1657/~1696), it omitted `Review.Mode ==
ReviewFreesideInvoked` and `profile.Review.ConfigDigest ==
reviewConfigurationDigest`. So a published run whose reviewer config was
approved at publish time but later un-approved by the trust profile could
restore readiness on recovery, where a fresh review would be blocked.
Fixed by enumerating every axis the pre-publication clean-pass acceptance
checked and closing all of them in `assertReviewedCandidate` (the two
profile axes were the only omissions; base/head, instruction digest,
current-config digest, clean outcome, and authoritative-over-failures
were already present). Red/green test on the profile-approval axis.

**Finding 6 (P2) — disposition made consistent with the recipe re-gate;
PR-retention deferred.** The round-3 terminal `completeBlockedTask` +
new `productionBlockReviewRecordMissing`/`domain.HoldReviewRecordMissing`
were **retired**. The fail-closed disposition now uses the *same*
`holdBlockedTask` mechanism as the sibling recipe-approval re-gate at
`completePublishedTask`, reusing the existing `domain.HoldTrustBlocked`
cause (no new wire/contract enum member). Determination: the recipe
re-gate (verified) also produces a generic hold that discards the
`published` result and carries no PR number / Open-PR action / lifecycle
watches, so the **PR-orphaning gap is pre-existing and shared across
axes, not introduced by #527** — my round-3 terminal choice was the only
#527-specific inconsistency. Retaining the published-PR binding on these
holds is deferred to a follow-up issue (cross-axis, out of #527 scope).
The hold is recoverable when the reviewer configuration is restored (the
config-drift case); an old-order run with no record stays held until the
operator dispositions it.

## Revisit when

The anchor's own revisit-when governs (see the revision-28 note).

Decision-2 config-digest strictness: `assertReviewedCandidate` requires
the record's `ConfigurationDigest` to equal, and (round 4) the trust
profile to approve, the *current* reviewer configuration. A legitimately
published run whose reviewer config drifts before its readiness-recovery
pass therefore also fails closed to the hold. This is safe (fail-closed,
operator-visible, never silent readiness) but arguably over-strict versus
"the review was valid at publication time." Revisit as a possible
decision-2 refinement if that drift case shows up operationally; not
changed here.

Published-PR context on a fail-closed hold: the recipe- and review-axis
re-gates hold an already-published run without retaining the PR number,
Open-PR action, or lifecycle watches (pre-existing across axes). A
follow-up issue tracks enriching these holds so the operator can navigate
to the live PR and merge/close/base observation continues.
