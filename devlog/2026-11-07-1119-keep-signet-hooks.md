# Engine Keeps Reaching Signet Through Hooks

Work unit: #2784. Mandatory note: an owner decision on the engine-signet
layering that would otherwise exist only in chat. No code or plan text
changes; this note is the record.

## Decision: Keep the Injected Hooks

The owner decided on 2026-11-07 to keep the current direction: engine
imports signet, and signet reaches engine behavior only through hooks
that `cmd/freesided` injects with `With*` options.

Chose to keep the hooks over planning an inversion, such as moving the
operator command surface out of signet so it can call engine:

- **The layering is deliberate.** `engine/doc.go:1-3` says the engine
  composes the store, signet, and StageDriver boundaries, and none of
  them needs to know the workflow that uses it.
  `devlog/2026-09-12-1950-client-task-submission.md` chose injection over
  a direct engine call for the first such hook.
- **An inversion is a material plan change.** Plan §5 assigns signet both
  AttentionItems and synchronization. Moving the command surface out of
  signet changes a component responsibility, so it would need its own
  plan PR before any unit, and no §11 wave plans it.
- **The cost is bounded and visible.** Six of signet's 21 `With*`
  options inject engine code: `TaskStopGuard`, `TaskSubmitter`,
  `TaskLineChanger`, `RoutingPolicySetter`, `AlternateAgentRetrier`, and
  `ParkedTaskMover`. Each new operator command adds a mirror struct in
  `signet/operator_commands.go`, an option, a nil sentinel, and a
  closure in `cmd/freesided/main.go`. That is boilerplate, not a defect.
  Five of the six hooks fail closed when nil. `TaskStopGuard` is the
  exception: with no guard, a Stop commits unguarded
  (`signet/specification_stop.go:17`, `signet/task_cancellation.go:76-80`).
- **An inversion is large for what it buys.** Engine imports signet in
  25 of its 92 non-test files, so reversing the direction touches far
  more than the six hooks it would remove.

The hooks' ordering hazard is real but separate: `main.go` wires them
before the workflow they close over is assigned, so `TaskStopGuard`
skips its guard while the workflow is nil (`main.go:1141-1143`). That
belongs to the composition-root plan, #2762, not to this decision.

Not verified, and not needed for this choice: whether moving
`signet.BlobStore` and `ErrDigestMismatch` into a leaf package, the cause
the 2026-09-12 note records, would alone let signet call engine.
Engine's 25 importing files suggest it would not.

## Revisit When

Revisit when a hook causes an ordering bug in production code (a hook
called before what it closes over is set, beyond the nil case #2762
plans for), or when a plan revision splits signet's responsibilities for
another reason.
