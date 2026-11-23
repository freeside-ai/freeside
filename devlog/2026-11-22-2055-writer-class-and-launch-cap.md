# A Writer's Class Defaults to the Cordon, and Only Warded Is Asked For

Issue #3471, tracker #3462, gives the operator the two settings cordoned
runs need: which class a project's writers use, and how many cordoned
launches run at once. This unit stores both and enforces the cap; #3472
routes writers by the class. The choices below are the defaults the
issue's Planning Notes offered the owner, which the owner handed over
unchanged with `Handle #3471`, plus the agent's implementation choices.

## Chose Cordoned by Default, With Warded the Only Override

A writer runs on `cordoned_host_process` unless the project's
`writer_class` names `fresh_vm_read_only_volume_handoff` or the task
requires it. The task requirement accepts only the warded class, so a
task in a warded project can't move itself onto the cordon: a per-task
setting can only strengthen containment, never weaken what the project
chose.

Rejected:

- **A task setting that names either class.** It would let one task
  undo a project's choice to stay warded, and the resolver would need a
  precedence rule between the two.
- **A warded default.** Plan §5.7 makes the cordon the default for the
  implementer and remediator; the warded class is the exception.

The resolver (`domain.ResolveWriterClass`) refuses a value either gate
would refuse rather than defaulting it to the cordon, so a bad value
never picks the weaker containment. Its doc comment names #3472's writer
admission as the caller, which scopes it to writer roles and owns what a
change after a task's first writer admission does.

## Chose a Per-Task Table Over a Task Body Field

The requirement is its own daemon-internal row (`task_writer_classes`,
migration 0149), not a field on the synced task. The app neither shows
nor sets it, so it moves no server revision, and the task's record
(`freesided inspect task`) shows it. Every read re-runs
`TaskWriterClass.Validate`, so a stored row naming the cordon fails the
read instead of reaching the resolver.

## Carried the Class in the Deputy's File

Real work runs from the deputy's `settings.json`, so a stored section
alone would reach no run. The file gains `repository.writer_class` under
the same rule, and `settings import` stores it.

Revisit when the installed daemon runs every project from the stored
records (#2083): the deputy key can then go.

## Chose an Instance Cap That Must Be Set Before Any Cordoned Launch

The cap is the instance settings section `cordoned_launches`
(`max_concurrent`, at least 1), read from the store at each admission.
With no cap set, no cordoned launch starts: the launch waits under the
`cordoned_launch_cap` hold until the operator sets one.

Rejected:

- **A built-in default cap.** The spike (`docs/spikes/cordoned-writer.md`)
  gives evidence for a value on one machine, not a number that fits
  every host. A default would let cordoned runs start on a machine whose
  operator never chose how many it can carry.
- **Unlimited when unset.** A cordoned run has no VM and reserves no
  memory, so nothing else would bound it.
- **A deputy `settings.json` key.** The cap is per machine, not per
  project, and the store already carries instance sections the file
  doesn't, such as the execution pause. `settings import` tolerates it
  for that reason.

## Chose a Hold That Names No Numbers

The hold says the cap was reached or is unset, not "2 of 2". The counts
change between dispatch tries and the hold is recorded once until it
clears, so a number in it would go stale; `freesided settings show`
gives the cap.

## Kept a Separate Order for Each Resource

Launch waits gain a `resource` column, `memory` or `cordoned_launch`,
and each gate counts only waits on its own resource. A cordoned launch
waiting for a place doesn't hold back a warded launch that fits the
memory budget, and a warded launch waiting for memory doesn't hold back
a cordoned launch under the cap. The rejected alternative, one shared
order, would let either queue stall the other for a resource it never
uses.

The column carries no SQL `CHECK IN` list, following the owner rule in
`devlog/2026-11-07-1113-enum-columns-go-validation.md`; Go's `valid()`
guards it on write and on read. This departs from the plan comment,
which proposed checking it in SQL.

## Freed a Place at Export or Outcome

A cordoned launch counts from its admission until its export or outcome
is recorded, earlier than a memory reservation, which holds through an
open handoff. A finished cordoned run has no process left on the host,
so holding its place through the handoff would idle capacity. A launch
that already runs passes the gate without a new decision, as a held
memory reservation does, so lowering or clearing the cap never refuses a
supersession of a running launch.

## Pre-Push Review

One fresh-context reviewer read the whole change before push, which also
served as the refute-first pass. It found no blockers and four
non-blockers, all latent until #3472 admits a cordoned writer:

- **Fixed: the app said no action was needed.** An unset cap waits for
  the operator, so the cordoned guidance now says to set a limit when
  none is set; the hold still can't tell unset from full.
- **Fixed: an admission with no identity was gated but never counted.**
  The gate now skips it, as the memory gate does, since it takes no pool
  slot and starts no writer.
- **Deferred to #3472: a warded admission superseded by a cordoned one
  keeps its memory reservation.** The stale dispatch may still have a VM
  running, so releasing the reservation at the cordoned gate could free
  memory in use. Whether a class change may supersede an unstarted
  admission at all is #3472's to decide.
- **Declined: the cordoned gate leases waits by the admission clock.**
  The daemon's admission clock is wall time (`daemoncompose/startup.go`),
  the same clock that records a refusal, so the two agree outside a
  scenario harness.
