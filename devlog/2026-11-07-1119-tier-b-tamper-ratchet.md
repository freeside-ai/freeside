# Tier B Units Remove the Tamper Checks They Touch

Work unit: #2785. Mandatory note: it changes the project's safety policy
(how Tier B code sheds Tier A-style checks) and records an owner choice.
The rule lives in AGENTS.md (Assurance Tiers, Tiers and Worked
Examples); this note records why. It refines "Ratchet, Not Sweep" in
`2026-11-03-1442-assurance-tiers.md`.

## Decision: Tighten the Ratchet

The owner decided on 2026-11-07 that a unit that changes a Tier B row's
decode, or that row's tests, removes its column-versus-body cross-checks
and tamper cases instead of extending or reshaping them. A check that a
Tier A gate relies on is not Tier B and stays, so the unit traces the
read's Tier A callers first. A tamper test of a Tier A gate stays Tier A
under the Classification rule for gate tests.

The 2026-11-03 note let existing checks stay "until a unit touches its
file anyway or a cleanup unit removes it". It said a check may go; it
never said the unit must remove it.

Chose the one-sentence tightening over keeping the passive ratchet or
authorizing a bounded cleanup sweep:

- **The passive ratchet isn't draining.** Units that touch these files
  keep the checks, and some extend them: `8066c55ca` reshaped the
  `routing_policy` tamper table right after the tier rule merged.
  `errRowInconsistent` has 206 uses in 45 of the 72 non-test store files
  outside the Tier A list.
- **The cost is too small for a sweep.** Upkeep runs about 20 to 70
  lines per touch across four commits since 2026-11-03, and no bug traces
  to it. A cleanup unit would first have to classify each file, since
  some look-alike checks are Tier A by category; that review costs more
  than the checks do.
- **The unit that touches a check already has it open.** Removing it
  there costs a few lines and needs no separate classification pass
  beyond the file the unit is already in.

## The Tier A Exception

Several look-alike checks are Tier A by category and stay: the task line
forged-row check that admission runs, which AGENTS.md keeps by name,
`verifySpecificationChain` (approval binding), recommendation sources
bound to the decision surface, run-observation authentication, and the
`ReadTx` memo caches.

A check outside the Tier A list can still feed one of these. The task
line case is the example the Worked Examples now name. Admission relies
on three checks in `store/task_line.go`:

- **The chain check.** `validateTaskLineChains` (`:362`) runs on every
  read of a line's versions. `anchorRouterLine` depends on it by name
  ("the store's chain check", `engine/agent_admission.go:690-691`), and
  `RunTaskLine` reads lines through `TaskLineHeads` (`:513`).
- **The id rebuild.** `scanTaskLine` rebuilds each line row from its
  columns and requires it to resolve to its own id (`:343-347`); the
  admission re-gate `gateTaskLineSelection` (`store/execution.go:596`)
  relies on `GetTaskLine` resolving the id it cites.
- **The change-record binding.** The forged-row check reads a line's
  change record with `GetTaskLineChange` (`agent_admission.go:661`),
  which binds the decoded body to its id.

All three stay while admission's checks do. Admission reads a routing
policy's body (`agent_admission.go:792`), not its columns, so the column
comparison in `scanRoutingPolicy` (`store/routing_policy.go:91-95`) and
its 11 tamper cases (`routing_policy_test.go:117-135`) drain under the
new rule. The decode itself stays on admission's path.

Not verified, left to the unit that touches them: whether
`store/intake_occurrence.go` (admission binding) and
`store/ready_resource.go` (PR target binding) are Tier A by category. A
unit in either file classifies it first; a file doing Tier A work joins
the Tier A list instead.

## The Worked Example Was Wrong

AGENTS.md said `store/task_line.go` and `store/routing_policy.go`
"decode their rows with plain validation". Both still cross-check
columns against the body. The example now says they are Tier B and
still carry pre-tier checks, and names which of them stay. It also
said admission's forged-row check "stays under the ratchet"; once the
ratchet means removal, that phrase reads backwards, so it now says the
check is Tier A and stays.

## Revisit When

Revisit if a removed check would have caught a real bug, or if a later
count of `errRowInconsistent` outside the Tier A list shows the checks
still growing.
