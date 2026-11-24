# Admission Places Writers on the Cordon, Pinned per Run

Admission now places implementer and remediator attempts on the cordoned
class by default: the task's requirement, then the project's
`writer_class`, then the cordon. The specifier and clean verification stay
on the ward. Issue #3472, tracker #3462. The choices below follow the plan
comment on #3472 unless a line says otherwise; departures are the agent's
implementation decisions.

## Chose a Run-Level Pin Over Resolving Each Attempt

A run's first writer admission fixes its class. Every later writer
admission of the run, a remediation round or an operator-feedback turn,
resolves the class again and must match. Re-resolving each attempt would
let a remediation run under another containment than the candidate it
fixes, and would let re-admission (`readmitStaleAdmission`) supersede an
unstarted warded admission with a cordoned one. That supersession leaves
the warded memory reservation behind, because a cordoned admission takes
none and supersession releases nothing: the deferral #3471's review left
on this issue. With the pin, a fresh admission keeps the stale one's class
or is refused, so the case can't arise.

The pin compares placement, not names: an admission counts as cordoned
only when it names `cordoned_host_process`, and every other one counts as
warded. Test backends in attended mode carry other names, and the
admitter only ever places a writer on its ward backend or its cordon.

## Chose Refusing a Class Change Over Following or Ignoring It

A writer admission whose settings now resolve another class is refused
with `ErrWriterClassChanged` and one `execution_failure` notice on the
run, naming both classes and the two ways out: restore the setting or
stop the task. Following the change mixes containments in one run.
Ignoring it silently runs a writer where the operator said not to, the
wrong default for a setting that exists to say "this material warrants a
VM". The refusal records nothing, so restoring the setting lets the next
pass admit the attempt.

Consequence at upgrade: a run admitted warded before this change, in a
project with no `writer_class`, now resolves the cordon and waits on the
notice. The operator sets `writer_class` to warded to let it finish. A
daemon with no cordon composed holds every writer the same way until a
project sets warded; it never falls back to the ward.

A capability retry is a new run, so the pin doesn't reach it, yet its
attempt names a manifest that only the ward's proxy applies. Two guards
cover it. A cordoned writer's failure card offers no manifests, and a
manifest-bearing retry that resolves the cordon anyway (a setting changed
after a warded failure) is refused the same way, with the same notice. Before
the guard, the store refused such an admission with a derivation mismatch
that has no hold row, which ended the whole dispatch pass on every pass.

## Chose `host_open` Whatever the Policy Requests

A cordoned admission records `host_open` and skips the writer egress
policy, the capability-manifest retry, and `writerEgressRefusal`. Those
describe the ward's proxy, which a cordoned run never passes through, and
`host_open` is what the run can reach until #3477 lands step 2 of the
plan's network section. Recording the policy's requested profile would
claim an enforcement that doesn't exist.

## Chose Keeping the Selected Image on a Cordoned Admission

Image selection still runs and its image is recorded, though a cordoned
writer runs no image. Verification and rebuild compare the candidate's
image with the writer admission's, so dropping it would break those
comparisons for cordoned candidates. The ward's per-image proof read is
skipped: the cordon's proof is one configuration digest, checked under
`unattended` against the class's own floor.

## Chose Refusing Attachments at Admission

The cordoned driver can't deliver attached files and would refuse the
start anyway. Refusing at admission, through the existing undeliverable
writer-input path, gives the operator a card whose reason names the warded
setting and records no admission. Delivery is #3593.

## Departure: Invocation-Scoped Holds

The plan routes both refusals, no proven cordon and a changed class,
through `store.ErrBackendNotConformant`, whose hold-classification row
ends the whole dispatch pass. That would stall every other run's work
behind one writer's setting. Each refusal instead joins its own sentinel
(`ErrCordonNotConformant`, `ErrWriterClassChanged`) with a row placed
before the pass rows that holds only its invocation, under the same
`backend_not_conformant` reason, as `ErrImageNotProved` already does for
an image waiting on its proof.

## Refute-First Findings

Two fresh-context review rounds ran on the diff.

- **Fixed: a capability retry on the cordon stalled every dispatch pass.**
  Round 1 found it: the guards in the pin section above.
- **Fixed: two Tier A re-checks had no test that a mutation would fail.**
  Round 2 found that the start authority's class check and re-admission
  across classes were untested. Both now have one, and each was checked
  by reverting the line it guards.
- **Deferred: the pin's admission read scans every run's admissions.**
  `ListRunExecutionAdmissionRecords` filters in Go; the store fix is
  #3594.
- **Declined: a cordon re-proof between admission and its record can end
  one dispatch pass.** The store's plain conformance refusal is a pass
  refusal, and the next pass holds only the invocation. The ward has the
  same one-pass race today.
- **Kept: `attended_dev` admits a cordon with no proof.** The attended
  floor is empty for every class, as it is for the ward; the proof binds
  under `unattended` only.

Revisit when a writer role other than the implementer and the remediator
may run cordoned, or when attachments reach a cordoned writer.
