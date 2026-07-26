# Let One Failed Removal Yield To Its Siblings

Work unit: #290. This changes a destructive path and the credential-doctor
diagnostic contract, so the note and refute-first pass are mandatory. Prior
note: `devlog/2026-07-25-1126-janitor-registration-isolation.md` (#281, which
discovered and filed this unit).

## Problem

`reconcileRegistration` returned on the first failed suspend or delete. One
installation GitHub would never remove therefore starved every later removal
for the same App registration on every pass. #281 made the failure
registration-scoped, so the rest of the daemon stayed healthy while the
starvation repeated indefinitely.

A different indefinite state had no diagnosis: a registration whose removals
succeeded but whose next enumeration returned the same drift never reached a
clean pass, never gained coverage, and never recorded a fault. The credential
doctor therefore reported `janitor_inactive`, exactly as it did for a
registration waiting for its first pass.

## Decisions

- **Per-installation suspend and delete failures are accumulated while sibling
  actions continue.** A failed quarantine suspension skips that installation's
  delete, preserving suspend-before-delete, then yields to the next action. A
  failed delete likewise yields. `RunCycle` still returns the joined failures,
  and the always-on loop still records one registration fault, so this does not
  change what counts as a failure.
- **The existing attempt counter remains the removal bound.** Every action
  consumes the pass-wide budget after its audit record and before its remote
  effect, whether the effect succeeds or fails. When a prior failure and the
  bound coincide, the failure is retained and later registrations are not
  examined.
- **Shared safety failures still stop immediately.** An audit-journal failure
  remains `errJanitorUnsafe` and stops the pass. If it follows an attributable
  removal failure, both errors are returned so the earlier diagnosis is not
  erased.
- **Successful destructive progress is a separate state, not a fault.**
  `ChurningRegistrations` reports the registrations whose latest completed
  reconciliation removed installations without reaching a clean pass, plus
  the number of consecutive such passes. `CredentialDoctor` reports
  `janitor_removal_churn`; a real fault takes precedence, and a janitor without
  the optional churn port still degrades to `janitor_inactive`.
- **Skipped passes neither clear nor increment churn.** A pass-wide bound can
  stop before a previously churning registration. Its last diagnosis remains
  valid until a later clean pass, fault, or churning pass supplies a new
  outcome, or until the registration leaves the keystore.
- **Coverage requires every owner-keyed record for an App ID to finish clean.**
  Keystore enumeration is keyed by owner, so duplicate records can carry one
  registration ID. A clean duplicate reached before the bound cannot open the
  gate when another duplicate of the same ID was skipped or was reached only
  after another registration consumed the last permitted attempt.

## Rejected Alternatives

- **Treat successful removal churn as a registration fault.** Rejected because
  a completed removal is not a failure, and #290 explicitly leaves failure
  classification unchanged.
- **Stop after the first failure but rotate action ordering.** Rejected because
  it only moves starvation between installations and makes progress depend on
  remote ordering.
- **Redefine `JanitorCycle.Removed` as attempts.** Rejected in #281 and still
  wrong here: the exported counter reports completed removals, while the
  unexported attempt counter enforces the safety bound.
- **Add churn to `JanitorStatus`.** Rejected for the same reason as #281's fault
  accessor: the resolver needs only the gate methods. The doctor discovers the
  optional diagnostic port on the status value it already owns.

## Refute-First Verification

Two independent fresh-context lenses tried to disprove the change: one on the
destructive action loop and audit barrier, the other on published state,
duplicate identities, diagnostic precedence, and concurrency.

**Confirmed and fixed**

1. A failed action that tripped the bound returned through the fault branch
   before `runCycle`'s limit break, so a later registration could still be
   examined. The fault branch now stops the pass, and a two-registration test
   proves the later registration is untouched.
2. Replacing the churn slice after every pass erased a prior diagnosis when an
   earlier registration's bound caused the churning registration to be
   skipped. The latest state now persists without incrementing; a choreographed
   test pins `1 -> preserved 1 -> 2`, then a clean reset.
3. A later audit-barrier failure discarded removal failures already collected
   from earlier siblings. The unsafe error is now joined with them; the
   regression independently requires both the journal sentinel and the earlier
   API error.
4. The first clean owner record for a duplicated App ID could publish coverage
   when a middle registration exhausted the bound before the later duplicate
   record was visited. The pass now counts required and visited owner records
   per App ID and withdraws coverage for a partial visit.
5. The first draft tested only delete failure. A quarantine suspension failure
   now has its own effect-order regression: no delete follows the failed
   suspension, but a later removable sibling is deleted.
6. Churn withdrawal for duplicate App IDs was only asserted by inspection. A
   clean-plus-churning duplicate fixture now proves neither `ActiveFor` nor
   `AllowsRepository` opens.
7. Automated review found a second member of the duplicate-record class: a
   later duplicate could be reached after another registration consumed
   exactly the last attempt. Counting records as visited treated that
   bound-blocked reconciliation as complete. Coverage now requires every
   owner-keyed record to finish clean, and the duplicate regression enumerates
   both skipped and reached-but-bound-blocked cases.

Each confirmed finding was re-checked after its fix by the lens that raised it.
The regressions are mutation-sensitive to removing the corresponding branch or
state transition.

**Rejected by verification**

- No destructive effect bypasses its audit barrier; the recorder runs before
  the attempt counter and remote call, and a recorder failure prevents that
  effect.
- A failed suspension never falls through to delete the same quarantined
  installation.
- Cancellation stops later sibling processing and remains discoverable through
  the joined error.
- A partially successful registration with any attributable failure cannot
  publish coverage; fault precedence also prevents simultaneous churn
  diagnosis.
- Published churn is cloned under the janitor lock, contains scalar-only
  values, and the affected tests remain clean under the race detector.
- Duplicate registration IDs are compacted for diagnosis, but coverage is
  withdrawn by ID whenever any visited record faults or churns, or any required
  record is skipped.

## Revisit When

#289 defines retry and journal-retention policy for a removal that keeps
failing. If the daemon gains a unified operator status surface, combine faults
and churn into one atomic snapshot so a diagnostic reader cannot straddle a
pass boundary between separate accessor calls.
