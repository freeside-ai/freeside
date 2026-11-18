# Failure and Crash Paths Release Their Rig Names

Issue #3095. Tier A: `internal/ward/` and `internal/exec/stage/pipeline.go`
decide when a run's runtime names leave the rig, and an early release leaves
an object that rig cleanup and stale recovery can't enumerate.

#833's rule is unchanged: a name leaves the rig only when its owner has
finished with it, no later path creates an object under it without binding
it again, and the runtime shows no object under it at release time
(`devlog/2026-11-11-1900-rig-name-release.md`). This unit adds release points
to the paths that never reached #833's closes, so a deputy hold no longer
fills the 64 KiB manifest through repeated failures.

## Decision

- **Writer names release from the stage driver, not ward.** A handoff ward
  refuses before `Journal.Begin` leaves the stage intent in `phaseRunning`,
  and recovery returns it to `phaseSeeding` and reruns the handoff under the
  same run ID without the pre-job hook binding again. Ward can't tell that
  case apart, so it can't release there. The stage driver can: once its
  intent is terminal (`phaseCommitted` or `phaseLost`), it never resumes it,
  and a new start is a duplicate. Chose a new `stage.Gate` method,
  `ReleaseFinishedHandoffs`, over exposing the journal to the driver, so ward
  keeps the journal check: it releases only a run whose record is absent or
  closed, and an open record or a failed read keeps the names.
- **The release points.**
  - `StartWithInputs` releases on every return after the pre-job hook
    started, except once the seeding intent's save was attempted: a failed
    atomic write may still land the file, and recovery resumes it without the
    hook. This includes a pre-job refusal, which the issue left out; no
    intent exists then, and the next start runs the hook again. The release
    runs under the driver's lock, so no other start of the invocation binds
    and creates in between.
  - The pipeline releases when it returns with its intent terminal, and a
    pre-journal cancellation releases once committed. A terminal write that
    failed and is retained keeps the names until the retry that commits it,
    from `Inspect`, `Collect`, or `Cancel`, releases them.
  - Stage `Reconcile` releases every terminal intent in one call at startup,
    which covers a crash between close and release and a release that
    failed.
  - Review recovery releases, in one call, every closed review intent whose
    outcome is stored. The outcome, valid or rejected, fences relaunch.
  - A failed review launch releases the names it bound when the run has no
    open intent. No outcome fence is needed: the next launch binds the same
    set before it prepares a workspace or launches, and recovery and cleanup
    bind before they act. The run gate is still held, so no launch of the run
    races the release.
- **Batch the startup releases.** Intent files and review intents are never
  deleted, so startup visits every historical run. One absence check per
  batch costs three runtime listings in total, not three per run.
- **Review recovery releases every provider's names.** A closed intent's
  provider would need its binding read to re-derive. Releasing every
  provider's names for the run is safe, since only that run uses them and the
  absence check keeps any name an object holds.

## A Stale Intent Copy Could Undo a Terminal Release

The terminal release rests on "a terminal intent is never resumed". Refute-
first found a path that broke it: `Inspect` reads a non-live intent, a
`Cancel` then commits it terminal and releases, and the recovery `Inspect`
started runs `reconcileIntent` with the stale `phaseRunning` copy. With no
journal record, that rewrote the terminal intent to `phaseSeeding` and reran
the handoff under the released names. `reconcileIntent` now re-reads the
intent under the same lock hold that registers the recovery and skips one
that is terminal. The rewrite of a terminal record predates this unit; the
fix lands here because the release made it an enumeration leak too.

## Refute-First Record

- **Ward refuses before `Begin`, after creating something:** disproved.
  `Handoff` creates no runtime object before its journal opens.
- **The pipeline's commit of a pre-journal ward refusal:** disproved as a
  release point. `commitResultLocked` refuses a failed result from
  `phaseRunning`; recovery reruns the handoff instead, under names still
  bound. The pipeline releases only on the terminal paths that exist: a
  refused seed or handoff spec, and a cancellation.
- **A host seeder alive after a pre-journal cancellation:** allowed. It
  writes a host checkout, not a runtime object under the run's names.
- **Another creator of review names without a bind:** disproved by tracing
  every `PrepareCodexReviewWorkspace`, `codexReview`, and cleanup caller.
  Only `startRequestedReview` creates, after its bind, and every cleanup and
  recovery path binds first.
- **Stale intent copy in `reconcileIntent`:** confirmed and fixed (above).

The fresh-context review found no blocker and confirmed the lock order, the
terminal-intent claim, and that no two run kinds share a run ID. Its
findings:

- **The `Gate` docs named one precondition, a terminal intent, that the
  start's release doesn't meet:** fixed. The docs now name both cases. A
  start that saved no intent releases under the driver's lock, and the next
  start runs the pre-job hook, which binds again, before it hands off.
- **A retried terminal write never released:** fixed, at the retry's commit.
- **A failed start now reads the journal and lists the runtime under the
  driver's lock:** accepted. It is bounded by the teardown timeout, and the
  pre-job hook already probes the runtime under the same lock.
- **Startup reads one journal record per historical terminal intent:**
  accepted. It matches the existing per-intent cost of startup, and an
  unchanged unbind appends no record.
- **The seeding-save test relied on a read-only directory, which root
  ignores:** fixed. It uses a directory at the intent path, as the existing
  write-failure fixtures do.

## Out of Scope

- A writer run whose intent is terminal while its journal record stays open
  keeps its names, as today.
- The one-shot helper and conformance binders (#3094).

Revisit when a stage recovery path starts calling the pre-job hook, or
review gains a second launcher outside the run gate.
