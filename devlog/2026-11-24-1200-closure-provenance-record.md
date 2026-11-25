# The Task Record Reads Closure Provenance From the Proposal Instance

After #2349, a published pull request's `Closes` line carries no
provenance, so the task record's `closure` object is where an operator
learns whether the daemon bound the issue itself (`verified`) or confirmed
a client-recommended source (`recommended`). The approved specification
chose the mechanism below; the agent kept it unchanged.

## Chose the Gated Instance Read Over a Checkpoint Copy

The record reads `provenance` through `store.ReadTx.GetProposalInstance`,
reaching the instance by the id the closure checkpoint already holds.
`engine.ProductionClosureAnswer` gains `InstanceID`, copied from the
validated checkpoint; nothing else in the engine changes, and the answer
stays read-only and advisory.

Rejected:

- **Copying provenance into the closure checkpoint.** It would add a new
  stored field, a second copy of a value the instance already holds, and
  a column-versus-body pair that could disagree. Checkpoints written
  before the change would have no value, so older records would need a
  fallback anyway.
- **Decoding the checkpoint in `cmd/freesided`.** The checkpoint shape is
  engine-private; duplicating it would let the record drift from the
  engine's own validation.

The gated read re-runs the store's subject and closable-source gates
against current rows. An instance a real run admitted passes it, because
`AllocateProposalInstance` ran the same gate when the checkpoint was
recorded and every row the gate reads stays fixed afterward: a project's
repository id is an immutable conflict, a run keeps its task, a resolved
policy is written once, and none of these rows are deleted. A rename keeps
the repository id, and the gate matches on the id.

## Chose to Fail the Whole Closure Section

When a proposal was made but its instance read fails, or the instance is
not a source-issue closure, `closure` is `null` and the run's errors name
`closure`. The record never shows an `origin` with provenance silently
missing. A mismatched kind is reported with `domain.ErrParentKeyMismatch`,
so it classifies as an integrity error like the checkpoint's own
validation failures.

Revisit when the store's closable-source gate can refuse an instance a
real run admitted (for example, a project's repository id changing): the
record would then blank `closure` for that run, and the section-failure
rule would need an owner choice.

## Tier A by Path, Tier B Record

The change is Tier A by path: it edits
`internal/engine/publication_closure.go`, which the
`internal/engine/publication_closure*.go` entry covers. This note, the
refute-first pass below, and the fresh-context review meet that bar. The
engine edit only exposes the instance id the validated checkpoint already
holds; it grants nothing and changes no gate. The task-record read stays
Tier B advisory work, so it keeps the scope the findings below allow: no
run-ownership check on the instance and no tamper test.

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong.

- **Confirmed and fixed:** an earlier draft of this note argued that the
  engine's own closure reconcile proves the read passes. It doesn't: that
  read runs only on the policy-approval path and degrades a gate rejection
  to no binding. The reasoning above replaces it.
- **Allowed by decision: no run-ownership check on the instance.** The
  engine's `verifyClosureInstanceRun` re-binds a checkpoint's instance to
  its run before recording an approval, because there the id grants
  authority. The record grants nothing, and only a restored or corrupted
  daemon database (trusted under the threat model) could name another
  run's instance. The task record is Tier B, and AGENTS.md's Tier B rule
  bars adding cross-checks and tamper tests there.
- **Allowed by the specification:** no test reaches the kind-mismatch
  branch, which only a tampered checkpoint could reach, and a gate refusal
  of the instance classifies as `other`, not `integrity`, as
  `newTaskRecordError` classifies every other section.
- **Disproved by checks:** that a legitimate run fails the read; that a
  caller or equality assertion over `ProductionClosureAnswer` breaks (the
  integration test compares fields); that the edited published-cycle test
  no longer tells the cycles apart (reading the successor's missing
  instance fails the closure section, which the test rejects); and that
  the table test would pass with a hard-coded provenance (it covers both
  values).
