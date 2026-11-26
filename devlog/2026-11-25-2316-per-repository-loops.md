# Per-Repository Loops and a Project-Image Build Limit

Issue #3210. The daemon's repository-wide loops now run one per served
repository or project, and project-image builds run up to the
`image_builds.max_concurrent` instance setting (one when unset) instead of
one per daemon. Tier A by path: `projectimage/apple.go`,
`projectimage/builder_vm.go`, and `daemoncompose/claude_driver.go`.
`daemoncompose/project_loops.go` joins the
Tier A list, because it picks the registry set the rebuild gate bounds each
repository's upkeep with. The owner handed the whole unit to one session
("Don't split"), so the plan's three parts land as one PR.

## Builds Take Turns on One Builder VM

Chose a host file lease (`builder.lock`, beside the registry leases) that a
build holds from the builder's start until its stop is proved, over letting
concurrent builds share the builder. Apple `container` runs one builder per
host user, so two builds that each start and stop "their" builder stop each
other's. Before this change the daemon's one build gate kept that from
happening as a side effect; with a limit above one the gate no longer does,
so the turn is its own lock. The lease is the registry leases' flock loop,
taken by every build that manages the builder, which is every daemon and
deputy build of the user. An `onboard` build or a hand-run `container
build` leaves the VM alone and takes no turn, as it ignored the gate, so a
daemon build's stop can still end its builder; that hazard predates this
change.

Recovery takes the turn only around its builder stop, so a restart's cleanup
never ends a build running beside it.

## Who Waits for the Turn

Chose a busy answer per caller (`projectimage.WithTurnBusy`) over one
blocking wait. Upkeep waits, and its one-hour bound pauses while it waits,
so a build queued behind another's builder step is never refused for time it
did not spend. The publication lane gives up and leaves the task queued,
the same answer it gives a full gate, because the lane never waits on
another build.

## Memory

Each build keeps its own memory reservation, so the budget, not the limit
alone, bounds how many run at once. A build runs one VM at a time (recipe
read, builder, probes), and only the turn's holder runs a builder, so
concurrent builds never exceed their reservations together. The lane builds
under its verification reservation; the builder runs at
`projectimage.BuilderSize`, the verification default, and policy can raise
but never lower the verification size, so that reservation still covers it.

## The Limit Is Read at Start

`ImageBuildLimit` is read once when the daemon composes, like every
section but `cordoned_launches`, so `settings set` reports that it takes
effect at the next start. A live limit would need the gate to shrink under
held slots, which nothing needs yet. The deputy's settings file has no key
for it, so the deputy builds one at a time.

## One Repository's Passes Still Take Turns

Chose a per-repository pointer turn in `ImageUpkeep` over relying on the
gate. At limit one the gate serialized a repository's tip pass and its
scheduled pass; at limit two both could build the same image. The second
pass now waits for the first and re-checks the pointer it advanced.

## Per-Repository Upkeep and Cards

One `ImageUpkeep` and one trigger worker run per repository, sharing the
lane's `ProjectImageRebuild` and so the gate and the turn. The host-fault
card is filed under the repository's project instead of the scheduler's
system project, so one repository's healthy pass clears only its own card.
A card filed under a project no served repository's card goes under (the
system project, where an earlier daemon filed the one card, or a project the
daemon no longer serves) has nothing to conclude it, so the first
convergence after each start concludes those, as readiness cards for an
unevaluated project are.

A manual-submission project with no `projects` binding yet joins the first
loop, as every project did when the daemon served one repository; the
store's daemon-origin gate still refuses its work if it later binds
elsewhere.

## Registry-Set Refusal Narrowed to One Repository

This revises `devlog/2026-10-27-1248-image-upkeep-job.md`, which refused to
start when any two manual-submission projects declared different registry
sets "because one repository has one pointer". That reason holds per
repository, so the refusal now applies only to projects on the same
repository; projects on different repositories may declare different sets.

## Active-Resource Pass per Project

Each project's ready items and holds, and each repository's tip watch, run
on their own `keyedrun` key, and each pass runs on its own goroutine, so a
stalled GitHub call holds up only its own project or repository. The four
process-local maps moved into per-project state, which only that project's
run touches. A project's run lists the items again instead of reading its
pass's list, because its previous run can outlive the pass that listed
them, and a convergence over a version that run replaced would stop the
loop as stale. A wake for a new ready item starts a pass at once, and a pass
already running at the wake starts one more when it returns, so an item in
a project the woken pass skipped still engages the operator cadence without
waiting out an interval.

The hand-back label is #3659's (#3209): each observation reads the run's
project's label in its own transaction, and the remover takes off only that
label. This unit first built its own label per project, a map composed at
start from each repository's setting; when #3659 landed first, the rebase
dropped that map for #3659's lookup, which reads the current record and so
follows a live edit. What this unit adds is only that the lookup runs on
the project's own run.

## Rebase onto the Per-Project Gates

#3659 also removed the lane's one repository and made a stored tip that
names another repository or branch `domain.ErrRepositoryIdentityMismatch`
instead of input unavailable. The rebase keeps that answer:

- **Conformance seed.** A proof pass over another repository's image holds
  as input unavailable when that repository has no stored tip, and as an
  identity mismatch when its stored tip names another repository, the same
  answers the configured repository's tip gives. Every caller of a proof
  pass records a failure and backs off the same way under either error, so
  the identity error changes only what the log says, not whether a pass
  fails closed.
- **Pending resolution.** A re-entry and the image selector ask the upkeep
  of the run's admitted repository whether a resolution is pending, since
  the lane no longer has one repository of its own.

Revisit when Apple `container` supports more than one builder per user.
