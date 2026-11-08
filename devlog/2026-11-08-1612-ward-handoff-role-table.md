# Ward Handoff Role Table

Issue #2870. Tier A by path (`daemon/internal/ward/`). Follows the helper-role
runner chain proposed on #2776
(`devlog/2026-11-07-1956-ward-helper-role-runner.md`).

## Decision

**Chose one ordered table, `handoffRoles` (`handoff_roles.go`), over the
hand-kept role lists.** Ten places named the handoff roles by hand: the name
struct, the runtime namespace functions, teardown's claim lists, and
recovery's attempted-claim marking and adoption reap. A role added to the
handoff and missed in one of them would leave its object standing after a run
or a restart, some of them credential-bearing. Each row now names its
`wardname` role, kind, spec feature, recovery reap place, and accessors for
its `handoffNames` field and `runState` claim, and every list reads the rows.
A reflection test fails when a claim or name field has no row or more than
one, or when a row pairs one role's claim with another role's name.

**Chose to carry recovery's reap order as a second column over making
recovery follow teardown's order.** Teardown reaps the prompt and inputs
helpers first and the exporter last; recovery's adoption reaps the exporter,
the post-writer credential observer, and the writer observer first and the
pre-writer credential observer last. Nothing found depends on recovery's
order: each reap is independent and `verifyNoTokenStrayContainers` runs only
after all of them. The contract kept the order anyway, so a refactor would not
also change crash-recovery behavior. One column is cheap; dropping it is a
visible golden change.

**Chose a spec-feature column over per-list membership flags.** Which lists
include a role follows from the feature that creates it: every handoff,
`AuthStoreLease`, the Claude clean launch state, a prompt file, or operator
inputs. `RuntimeResourceNamesFor` takes every row, the conformance suite's
synthetic handoffs take the "always" rows, and recovery marks a row attempted
when its feature is "always" or the leased credential store, or the spec
enables it. Recovery marks the credential observers unconditionally, as it
did before: an extra claim only costs teardown an inspection, since teardown
reaps on ownership evidence alone.

**Left `wardname.RigShapes` separate.** It is the rig cleanup namespace's own
definition, a separate authority, and `rig_name_coverage_test.go` already
checks ward's name lists against it. Deriving it from this table would make
the cleanup namespace trust ward's own list instead of checking it.

## Refute-First Record

The old-versus-new comparison is `testdata/handoff-role-order.golden`, written
against the hand-kept lists in the first commit and unchanged by the later
ones. It records teardown's deletes with all 25 objects standing and every
claim attempted, recovery's 16 adoption reaps on a writer-complete record, the
deletes a canceled recovery's teardown makes for four cumulative feature sets
(which shows the claims it marked attempted), and the sorted name sets of both
exported namespace functions.

Findings, each confirmed, disproved by a check, or allowed:

- **The golden can't tell a leased spec from an unleased one.** Allowed: the
  credential observers are marked unconditionally in both versions, so no
  feature set changes them.
- **The feature sets are cumulative, not independent.** Allowed: a prompt file
  or operator inputs without the Claude clean launch state fails spec
  validation, so no reachable spec enables them alone. `enabledBy` uses the
  same predicates the old conditions did.
- **The order of names inside each exported class changed.** Allowed by the
  contract: `daemonlock.BindRigRuntimeResources` sorts them, and
  `deputy_test.go` takes `Volumes[0]` only to get some volume name.

A fresh-context reviewer, prompted to refute the diff, found no behavior
difference: claims are copied and referenced exactly where the old lists
copied and referenced them, and the golden passes against the old code. Its
findings:

- **Nothing stopped the agent row from gaining a recovery reap place.**
  Confirmed by a mutation that every test passed. The plain adoption reap
  would skip `reapRecoveredAgent`'s memory-limit-kill journaling, so the rows
  test now refuses a reap place on the agent.
- **The golden can't tell an "always" row from a leased-store row in
  recovery.** Allowed: both are marked unconditionally, and a row moved
  between the two changes the conformance suite's name set, which the golden
  pins.
- **`enabledBy`'s leased-store branch never runs from recovery.** Allowed:
  it is the exhaustive dispatch the enum convention requires, and its answer
  is correct for any later caller.

## Revisit When

The owner lets recovery reap in teardown's order (drop the `recoveryReap`
column), or a role needs a list its spec feature doesn't decide.
