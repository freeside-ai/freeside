# A Handed-Back Head With No Admitted Comment Raises a Notice

Issue #3192. Tier A by path (it exports `publish.HandBackLabelRemovalKey`),
so a mandatory note. The rest is Tier B: the notice displays untrusted
commenter logins and grants nothing.

A pull request under the hand-back label starts a hand-back cycle only once
an admitted account comments on its current head (#3160,
`2026-11-12-2127-hand-back-adopts-head.md`). Without that comment the run
waited with nothing on anyone's queue. The reconciler now raises an advisory
`system_health` notice, `hand_back_comment_missing`, and resolves it itself.

## Decision

- **A system subject, not the run.** Chose `Subject{system, daemon}` over a
  run subject because an open run-subject item with a requested decision
  reads as attention_required to run supervision, and a self-resolving
  notice must not end a supervised run (`invocation_stall.go` reasoned the
  same way).
- **One pass of grace, counted in process.** The notice is raised on the
  second consecutive pass that sees the same ready item wait on the same
  head, so a lander whose comment lands just after its label raises
  nothing. Chose a process-local map over a stored first-seen time: a
  restart only delays a notice by one pass, and storing it would add a
  record for an advisory item.
- **One notice per ready item and head, in any status.** The ID is the
  observing ready item's digest plus the head. Scoping by ready item keeps
  an older ready item of the same run from resolving the current one's
  notice; checking every status keeps an acknowledged notice from coming
  back for the same head.
- **The classifier is separate from the trigger.**
  `engine.HandBackCommentGap` reads; it runs only when
  `HandBackReentryDue` says no cycle is due, and `planHandBackReentry` is
  unchanged. It checks only what makes the notice true or false: the
  published pull request open on the head, an admitted comment, a cycle
  sealed on or running for the head, and Freeside's own label. Other
  reasons a cycle can't start (an earlier hand-back's open ending, a review
  failure, a stopped task, an unready project) have their own items or
  refusals. A task-cancellation check was tried and dropped for that
  reason.
- **Freeside's own label is told apart by the remover's record.** After a
  hand-back cycle makes the run ready again, the label stays until the
  remover takes it off. `store.HandBackReadyBinding` names that ready item,
  and the outbox record under `publish.HandBackLabelRemovalKey` says the
  remover is done; until then the label is Freeside's. Exporting the key
  is the only change to `publish`.
- **Tests where the fixtures are.** The contract placed classifier tests
  in a new `engine/hand_back_wait_test.go` using the reconciler's fixture,
  which the engine package can't reach, and the engine package has no
  published-run fixture of its own. The reconciler cases live in
  `daemoncompose/hand_back_test.go`; the cases that need a real cycle
  (Freeside's own label before and after the remover's record, and a
  cycle on the head after the commenter leaves the allowlist) live in
  `integration/hand_back_test.go`, which drives one and runs the real
  remover.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong, alongside
mutation checks that disabled each suppression branch and saw a test fail.

- **No change to cycle start: disproved by check.** The due read and write
  path are unchanged; the three trigger tests pass unchanged.
- **False notice after Freeside's own hand-back: disproved by check.** The
  remover records the removal only after GitHub confirms it, so a stale
  label read costs at most one pass, which the grace absorbs.
- **A notice that never resolves: disproved by check.** Every pass over a
  non-open ready item converges with a nil gap unless its observation
  failed; a persistent observation failure leaves the notice as it was.
- **Commenter logins: allowed.** Validated as UTF-8 with no whitespace or
  control characters, and used only in the reason.
- **A label put back on a head a cycle adopted raises nothing: allowed by
  the contract.** The cycle's triggering comment on that head stays
  admitted, and the contract raises no notice for a head with an admitted
  comment, answered or not. Same for a resolved notice when the label is
  put back on the same head. Both stalls are quiet; follow-up: #3387.

Revisit when the reconciler polls concluded ready items faster than every
15 minutes, since the grace is counted in passes, not time.
