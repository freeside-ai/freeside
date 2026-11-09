# Declare the Host Verification Class

Work unit: #2876, the first of the macOS verification executor's units
(plan §5.7, macOS Execution; the
[macOS verifier note](2026-10-22-1205-macos-verifier-executor.md)).
Mandatory note: a `kind:contract` change to domain enums carried in
admission and conformance records, and Tier A work in admission and the
conformance ceiling.

## Decision: The Unattended Minimum Is Per Class

`domain.RequiredCapabilities` now takes the backend class. Under
`unattended` the ward class keeps its configured floor plus the
networkless-export and enforced provider-egress proofs; the executor class
needs exactly `supports_credential_free_host_verification`; any other class
is refused. Chose that over a second, mode-keyed floor for host
verification, because the floor was keyed by mode alone and appended the
ward's two proofs to every unattended record. The executor exports nothing
and runs no writer, so it can never earn either proof, and a mode-keyed
floor cannot tell it from the ward. Refusing an unregistered class under
`unattended` moves a check into the re-gate that only the store's write
boundary made before: an unattended record naming a backend no class
registers now fails `AdmittedUnder` itself.

The executor class's ceiling (`ProvableCapabilities`) is the one capability,
so the store refuses a record claiming a ward proof for it, and the ward's
ceiling excludes the executor's capability, so neither class can borrow the
other's proof.

## Decision: Attended Admits the Executor's Own Declaration

Under `attended_dev` the executor has no minimum: the gate admits it with
whatever it declares, and the admission carries that declaration. Chose
that over requiring the capability in both modes, because §5.7 lets
`attended_dev` use a weaker class and report it honestly, which is how
the room (#2877) can be exercised before its conformance pass (#2879)
exists. The owner can veto this; the change would be one case in
`RequiredCapabilities` and one engine test.

## Decision: A Separate Gate, Not a Branch of Ward Admission

The executor is admitted through its own engine gate, which room selection
(#2882) calls with the run's publication task, not through `admitAttempt`.
A verification job is a room the publication workflow builds, not a ward
stage, and records no `ExecutionAdmission`. The gate admits only the
task's own verification invocation (the id its producer derives) and
refuses any invocation a writer, remediator, or specifier runs under.
`WithAdmission` refuses the executor as its backend, so a ward stage can
never land on it whatever invocation it carries.

## Decision: No Durable Record Yet

The gate returns an in-memory `HostVerificationAdmission`. Chose that over
a migration and store surface in this unit, because nothing reads such a
record yet and #2882 owns the job's evidence and restart paths, so it
decides how the admitted class and digest are recorded. A durable record
would be a new contract unit.

Revisit when #2882 places jobs on the executor: if it needs the admission
across a restart, that unit files the contract for its record.
