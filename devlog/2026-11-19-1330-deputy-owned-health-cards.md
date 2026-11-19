# Clear Deputy-Owned Health Cards With One Deputy Start

**Chose to have prelaunch mark the cards a deputy start closes and exit-root
start the deputy once for them, over teaching prelaunch to close or
re-evaluate them, because prelaunch is a read-only reader of the database and
can't rerun the checks that decide these cards.** Issue #3439, from the #2382
exit run (friction log F1, F22, F26, F36).

## The Cards

Three card families close only when the daemon reruns its own start-up
checks: `deputy_preflight_failed` (the deputy's self-check),
`execution_not_configured` (the driverless daemon), and a writer role's
`agent_selection_inactive` (`CheckWriterRoles`). Their only client action,
Acknowledge, leaves them open, yet prelaunch told the operator to resolve
them in a paired client, and exit-root started the deputy only when the
settings changed, so a root holding one looped until someone deleted
`composed_settings` by hand.

- **Classified by diagnostic code, not id prefix.** The
  execution-configuration prefix also carries `app_authority_in_use`,
  `rig_lease_present`, and `execution_paused`, which a deputy start leaves
  open. Agent-selection cards are classified by the role cause in the id,
  because a retired identity's card waits on the operator to adopt or retire
  it.
- **Rejected: prelaunch re-evaluating the cards.** F36's role card came from
  an adapter conformance proof that load killed (#3337). Only a start reruns
  that proof, and prelaunch must stay read-only (it can't close a card), so a
  prelaunch-side check could at best predict, not clear.
- **Rejected: a new `ExecutionState` member.** It would touch the Tier A App
  authority gate for no gain over card text that says what closes the card.
- **Bounded to one extra start.** exit-root starts the deputy once per
  invocation for these cards and stops naming them when they survive it, so
  the convergence loop never spins on a card a start can't clear.
- **Open cards keep their old text.** The converge functions never rewrite an
  open item's reason, so only cards filed after this change carry the new
  wording; the next passing start closes the old ones anyway.

## The Lease-End Rule

**Chose to abandon the deputy's rig lease only when a run that served with
the Claude driver ends in error, over abandoning whenever the daemon was
started, because the abandon exists to report an interrupted task run, and
release safety never depended on it.**

Before, `runDeputy` marked the daemon as run before calling `run`, so a
startup failure (the real root's log shows ward conformance failing after
"composition passed") abandoned the lease, and the next start filed a
`deputy_run_interrupted` card though no task ran. A driverless run (a failed
self-check) runs no agent either. Both now take the clean release, which
still deletes the recorded containers, proves the rig clear, and abandons
the lease when that proof fails. What release deletes or proves is
unchanged.

## Refute-First Findings

- **A startup failure leaves a resumed session's resources behind:**
  disproved. `daemoncompose.Start` closes the sessions it resumed before it
  returns the error (`closeStartupSessions`), and the clean release's proof
  abandons the lease if anything recorded remains.
- **Something reads the abandoned lease's recovery beyond the card:**
  disproved. `acquireDeputyLease` returns the recovered manifest only to
  `fileDeputyInterruptedCard`; recovery itself runs the same cleanup whether
  or not a task ran.
- **A driverless run starts containers the clean path misses:** disproved by
  construction. Without the driver no agent or proof runs, and any container
  the manifest records is deleted and proven absent before release.
- **A startup failure can follow task work:** confirmed, and allowed.
  `daemoncompose.Start` starts the engine loops before its last steps
  (minting the pairing code, publishing readiness), which can still fail.
  A task stage the loops resumed in that window is torn down with the
  sessions before `run` returns, and the clean release's proof still gates
  the lease, so nothing is orphaned; only the advisory interrupted card is
  lost, and the task resumes or fails on its own records. The contract
  chose the clean path for every `run` failure.
- **A writer-role card a start can't clear is marked as closing on one:**
  confirmed, and allowed by the contract. A role refused for a missing
  enrollment gets `closes_on_deputy_start: true` by its id. exit-root's one
  start then fails `CheckWriterRoles`, the deputy exits before serving, and
  exit-root stops with the deputy's log, so the cost is one bounded start.
- **A failed admission-gate read still triggers exit-root's start:**
  confirmed, and allowed. The read leaves `operator_stop` false, so the
  items qualify; prelaunch's own fix names the gate error, and exit-root's
  start is bounded to one.

## Remaining Cause

A deputy that exit-root's 60-second `kill -KILL` fallback ends runs no
deferred release, so it still leaves the lease and the next start files the
interrupted card. That path is real but not the one the exit run's logs
confirm.

Revisit when the deputy gains a supervisor that restarts it, since then a
start-up failure could be retried without the operator, and when prelaunch
is allowed to write the database.
