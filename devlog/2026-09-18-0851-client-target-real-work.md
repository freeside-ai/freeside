# Client-Target Mode For The Real-Work Harness (#1384)

Adds `run-real-work.sh --client-target`, which submits the three input files
only as a visibility seed and then follows and verifies the task the operator
submits from a Freeside client, named by task ID through
`real-work-session.sh select-target`. Keeps the ownership, rig-cleanup, and
supervised-restoration rules of
[the walkthrough lifecycle note](2026-09-07-1215-production-walkthrough-lifecycle.md).

## Selection Binds By Trusted Identity, Not Source Text

Chose to resolve the target from the store's task and run records over matching
the operator's source text. The harness records the operator-named task ID as a
durable, no-clobber `target.json` before it follows anything, resolves the
specification run as the first entry of the task's recorded run list
(`ReadTx.TaskRunIDs`, specification run first by ordinal), and derives the
implementation run as the single run the one-way
`domain.SpecificationRunIDMatchesImplementation` hash pairs with it. Text
matching would let a duplicate or look-alike submission silently become the
verification target; identity binding cannot.

## No Store Link Proves "Came From A Client", So Two Proxies Guard It

The store keeps no task-to-submission-origin record, so the resolver cannot
prove a task came from a client rather than from `freesided submit`. Chose two
existing signals over adding that accessor: the selected task is refused if its
runs include the seed's specification run (it is then the seed), and if it was
created before this session's daemon started (recorded as Unix nanoseconds just
before launch; the seed and any pre-existing task predate it, the client target
does not). Closing the gap fully needs a new store accessor, which is
`kind:contract` work and out of this unit's scope. Selection also refuses a task
in another project and a cancelled or stopped task (`Task.Cancellation != nil`),
and the final verifier re-checks task, project, cancellation, and the
specification/implementation pairing under `FREESIDE_REAL_RUN_TARGET_TASK_ID`.

Revisit when: #1332 lands (project discovery removes the need for a seed) or a
store accessor records submission origin (then refuse by origin, not by
creation time).

## The Seed Runs A Throwaway Specification Run; The Harness Ignores It

The harness never follows or verifies the seed, but the daemon does execute it.
Under `-operating-mode unattended`, `freesided submit` persists a pending
specification invocation and nothing gates that invocation before it generates
a spec (the spec-approval gate fires only afterward, on the spec ->
implementation transition). So the seed spends one real specification
execution, creates a spec-approval attention item the operator leaves
unapproved, and, because the writer identity has a single execution slot
(`MaxParallelExecutions == 1`), holds the client target's specification run
behind the seed's until the seed reaches its approval gate (a few minutes).

Chose to accept this rather than fence the seed: the only durable fence is
`Task.Cancellation`, set through the signet `StopTask` HTTP command with no
`freesided` CLI entry point, so fencing it would need new daemon/cmd surface
outside this unit's scope. The #1384 plan's risk section anticipated the
single-slot queue, and #1332 removes the seed entirely. The earlier
"visibility-only" framing oversold the seed as inert; the docs and this note
now describe the execution plainly. Tracked in #1405.

## Two Deviations From The Implementation Plan

- **The ">1 paired implementation runs" bind branch is a fail-closed guard, not
  a tested path.** The specification run ID is a one-way SHA-256 of a single
  implementation run ID, so two distinct implementation runs cannot pair with
  the same specification run without a hash collision. The guard stays (silently
  picking one run at a trust boundary would be wrong), but the plan's "test two
  matching runs" case is infeasible to construct and was omitted; 0 and 1
  matches are tested.
- **A cancelled target discovered during supervision is not given a new
  supervision arm.** `observe.SupervisionState` has no canceled/stopped value, so
  there is no state string to drive such an arm; adding one for a guessed value
  would be speculative. Cancellation is defended at selection time and by the
  final verifier, both tested; a cancelled target's run resolves to an existing
  terminal outcome or ends on the bounded supervision deadline, the same limit
  the plan's revision-restart risk note already accepts.

## Finding: `set -e` Leak In The Bind Helper

`real_work_bind_target` first toggled `set +e`/`set -e` around the resolver
call. Re-enabling `set -e` inside the function leaks to the caller (shell
options are global, not function-scoped), so its `return 3` tripped a caller's
`set -e`. Replaced the toggle with `rc=0; cmd || rc=$?`, which captures the exit
code without changing global options. In production the leak was contained by
the command-substitution subshell that invokes the resolver hook, but the
pattern was fragile; the fixtures now cover the pending/bound/error returns.
