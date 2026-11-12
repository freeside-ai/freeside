# A Notice When Every macOS Executor Slot Is Quarantined

Work unit: #3004. Mandatory note: Tier A by path (`internal/macverify/`,
`internal/engine/production_verification.go`, and
`internal/engine/production_reentry.go`). The change grants no authority; it
adds an operator notice to a path that already retried. It resolves the
residual risk in `devlog/2026-11-09-2207-verification-room-selection.md`
that an executor whose every slot is quarantined retried forever without
naming the cause.

## Decision

- **A notice, not a new hold reason.** Chose an `execution_failure` item
  under its own prefix (`host-slots-quarantined-`), the shape the
  can-never-fit memory refusal already uses, over a new run hold reason.
  Hold reasons are a domain enum, so a new one is a contract change, and
  the notice says everything an operator needs: the executor root, the
  quarantined count and slot names, that a restart returns the slots, and
  that stop is offered.
- **Keep the `publication_environment` hold beside it.** Unlike the memory
  refusal, which clears its hold because the launch no longer waits, this
  job really is in the generic environmental back-off and keeps retrying.
  The notice explains the back-off; it does not replace it.
- **A typed refusal tells busy from quarantined.** `slotPool.lease` returns
  `*macverify.NoFreeSlotError`, which matches `ErrNoFreeSlot` under
  `errors.Is` so every existing check holds, and reports `AllQuarantined`
  only when every slot is quarantined. A busy pool, with any slot leased,
  retries quietly as before, because that slot frees on its own.
- **Retire on the host room's record, at the verification site.** The
  engine wraps the host room it hands verify (`hostSlotsRoom`), which
  records whether the executor answered any command with something other
  than the all-quarantined refusal. After `verify.Verify` returns, in
  `runVerification` and `verifyReentry` (which the handoff merge check also
  uses), the notice is superseded only when such a command was recorded and
  the job did not end on that refusal.
  Retiring in the reconcile loop was rejected, and so was retiring whenever
  the job merely had a host room: a pass that fails before verifying, or in
  an image-room command that runs first, says nothing about the slots, and
  retiring on it would raise a fresh occurrence on the next pass that
  reaches the executor.
- **The refusal path does not retire it.** A withdrawn declaration under
  unattended records a failed verification without building a host room,
  so a notice raised earlier stays open until the run's next verification
  with the executor, or until the operator concludes it.

## Refute-First Findings

A fresh-context reviewer, given only the diff and the issue's intended
outcome, tried to refute the change before commit.

- **Confirmed and fixed:** the first draft retired whenever the job had a
  host room, so a transient fault in the image-room command (which runs
  first in a mixed recipe) retired the notice and the next pass opened a
  new occurrence. Fixed by the host room's record above.
- **Confirmed and fixed:** no test exercised `verifyReentry`'s retirement;
  removing that call left every test green. A readiness re-entry test now
  raises and retires the notice through it.
- **Allowed:** after a restart, a slot that still owns a process is leased,
  then refused by its prepare. That pass counts as served, so the notice is
  superseded and the job retries quietly. Restart is the cure the notice
  names, and a slot that stays dirty afterwards is #3000's concern
  (re-inspecting quarantined slots is a non-goal here).
- **Allowed:** a host command that fails a check the room runs before
  leasing (the toolchain probe, the Swift cache entry) counts as answered,
  so it retires the notice even with every slot still quarantined. A
  transient failure there costs one more notice occurrence on the next pass;
  a persistent one hides the slots notice while that other failure already
  blocks the job, and it returns once that clears. Telling the cases apart
  needs macverify to mark every pre-lease error, which this unit does not
  take on.
- **Allowed:** the retirement read is not shielded from cancellation, unlike
  the reservation release. A cancelled pass is shutdown, which the reconcile
  loop already treats as such, and a later served pass retires it.
- **Disproved by reading:** a busy pool cannot read as all-quarantined
  (quarantine removes the slot from the leased set, and configured slots are
  unique); `errors.As` reaches the refusal through every wrap between the
  room and `recordRetryableHold`; every existing `ErrNoFreeSlot` check still
  matches; the error's retry classification is unchanged; repeated passes
  converge on one open item; and a store fault on the notice still records
  the hold.

## Revisit When

- #3000 lets a quarantined slot return to service without a restart: the
  notice text and the `ErrNoFreeSlot` comment then overstate the cure.
- The engine gains real queueing across slots, which would make a busy
  executor's retries something other than the generic back-off.
