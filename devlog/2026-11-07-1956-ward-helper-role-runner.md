# Ward Helper-Role Runner

Issue #2776. Tier A by path (`daemon/internal/ward/`). First link of a chain
that moves every short-lived ward helper role onto one runner.

## Decision

**Chose one runner, `runtimeOps.runHelperRole` (`helper_role.go`), over
keeping each role's hand-written create-to-absence sequence.** Each role
wrote the sequence out by hand and the copies drifted: adding one check to
every role took edits to 14 to 16 files (`f9d900f29`, `39b7e700c`), and the
lineup probe went without a network attestation until #2772. This unit moves
the prompt and inputs seeders and observers, the launch-state seeder, the
config, continuity, and scratch observers, and the instruction seeder and
observer. `startStagedRole` and `removeStagedRole` are gone.

**The step order is fixed:** mark attempted, create, mark owned, inspect, the
role's allowlist check, fingerprint, start, an optional after-start step (the
seeders' input copies), wait for stop under the role's timeout,
`helperStopped`, an optional proof read and proof check, delete,
`verifyContainerAbsent`, clear the claim. The allowlist check comes before
the fingerprint, as every moved role already did (`seed.go` explains why): a
report that fails the allowlist is never bound as this run's object. The proof
check runs before the delete, so the instruction observer's journal write
still precedes it and a refused proof leaves the container to teardown. The
runner refuses a role with no allowlist check, claim, or timeout before any
runtime call.

**Chose one error-class rule over keeping each role's mix.** Every runtime
step's failure (create, inspect, fingerprint, start, stopped wait, delete) is
a `ConformanceFailure` for the role's check, built with `failw` so the cause
stays reachable through `cf.Err`. Steps a role supplies (allowlist, after
start, proof read, proof check) and the absence check return their own
errors. `helperStopped`'s `*MemoryLimitError` passes through unwrapped,
because callers detect it. The runner never sets the content-evidence mark;
only a role's proof check may.

The rule changes only the staged-volume roles: their inspect, start, and
stopped-wait failures were plain errors and are now control-plane isolation
conformance failures (12 rows of `testdata/helper-role-sequence.golden`).
That is safe because nothing outside `internal/ward` branches on
`ErrConformance`, and inside it recovery acts only on the content-evidence
mark (`recover.go`), which no changed row gains. The rejected option, keeping
plain errors where a role had them, would have meant a per-role error switch
in the runner, the drift this unit removes.

**Pinned behavior before moving it.** `TestHelperRoleSequence` was committed
against the old code first. It records the runtime and journal calls of one
successful handoff, which stayed byte-identical through the move, and for
each moved role a failure at create, pre-start inspect, allowlist, start, and
the stopped wait: error class and check, content evidence, and whether
teardown reaped everything.

## Roles Left Out

- **For later links:** the workspace seeder, base observer, writer-outcome
  observer, and credential observers (link 2); the Codex review helpers and
  the Claude enrollment seeder (link 3), which fingerprint before the
  allowlist check today; the lineup probe and one shared host-only network
  check (link 4); one handoff role table (link 5). The order and sizes are in
  the #2776 plan comment; filing them is the planner's.
- **In every link:** the agent and the exporter (teardown deletes the
  exporter, and its proof is the export archive), the long-lived Codex
  reviewer container, and the conformance suite's probes, which assert
  intermediate states on purpose.

## Refute-First Record

The old-versus-new comparison is `TestHelperRoleSequence`: written and run
against the old code, then held across each move. A fresh-context reviewer
tried to prove the change wrong.

- **A step reordered, dropped, or duplicated: disproved.** The successful
  handoff's call list is byte-identical across every move, including the
  instruction observer's journal write before its delete.
- **A claim left in a different state on failure: disproved.** Each role
  keeps its claim slot, and the runner sets `attempted`, `owned`, and
  `fingerprint` at the same points. `helper_role_test.go` checks the claim
  after a failure at every step, and the golden shows every failure reaped.
- **A memory-limit error or content evidence changed: disproved.**
  `helperStopped`'s error passes through unwrapped, the runner never marks
  evidence, and no golden row gained or lost it. Every role's `spec.Size` is
  the `hs.Size` `helperStopped` was given before.
- **Cancellation in a staged role now logs `failed`: confirmed, allowed.**
  `failw` keeps the cause off the error chain, so a `context.Canceled` from a
  staged role's stopped wait no longer reaches `launchEndFor`. That matters
  only when the joined `ErrHandoffCanceled` is absent, because the
  cancellation journal write or teardown also failed; the launch record then
  says `failed` rather than `canceled`. The instruction and state roles
  already behaved this way, and nothing else reads the difference.
- **The golden misses delete, fingerprint, copy, proof, and memory-limit
  failures: confirmed, allowed.** The issue fixes the golden's five steps;
  `helper_role_test.go` covers the rest against the runner directly.

Revisit when a role needs a step the runner lacks: add it to the runner
rather than writing that role's sequence out by hand again.
