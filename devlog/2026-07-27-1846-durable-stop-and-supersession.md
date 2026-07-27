---
run: manual
stage: durable-stop-and-supersession
date: 2026-07-27
branch: feat/durable-stop-supersession
---

# Durable Stop/Resume and the §4 Blocking/Supersession Rule

Work unit: #319 + #321, one serialized contract PR per the #231 spine
amendment. Scope: `api/`, `daemon/`, `app/`, and this note.

## Decisions

**The operating state is an append-only transition log, not a mode flag.**
`unattended_operation_transitions` (migration 0017) records each operator
stop/resume decision with its accepted command binding; the latest row is the
state. "Restart alone never resumes" is structural: only signet's
`resume_unattended` transaction writes a `resumed` row, and daemon startup
writes nothing. Rejected: a singleton state row (loses the operator audit
trail and needs update semantics the store's immutable-row idiom avoids), and
threading the state through `engine.AdmissionEnvironment` (construction-time
configuration is exactly what #319 exists to escape; the environment stays
frozen and the store gate reads durable state per transaction).

**Supersession is a typed field on the item, validated live, never trusted.**
`AttentionItem.BlockingSupersession` names the validated configuration class
and payload (`backup_encryption_waiver` + repository id), is legal only on
`system_health`, and is immutable once set. Blocking derives from it: open ∧
`system_health` ∧ (no condition ∨ condition fails). The stored condition is a
claim; `Supersedes` re-derives it against the transaction's live policy
through `waiverConfiguredFor`, the same helper `AdmittedUnder`'s waiver
clause uses, so the two gates share one definition of "the operator holds
this waiver" and clearing or retargeting the waiver re-blocks every notice it
covered with zero writes. Rejected: `StatusSuperseded` (terminal; the notice
must stay open and decidable), an item-ID naming convention (the failure mode
the issue names), and a daemon-private side table (the 0015 private-envelope
precedent covers implementation detail; §4 makes this part of the item's
meaning, and the spec-mirrors-domain discipline would otherwise hide a
semantic from the shared contract).

**Supersession validity is condition-versus-configuration, not
condition-versus-the-current-admission's-target.** §5.7's words are "whose
blocking state the validated waiver *configuration* supersedes": a validly
held waiver for repository N supersedes the notices carrying condition N for
any unattended admission, rather than only for admissions targeting N. The
two readings are indistinguishable in this build — every unattended
admission must carry the waiver and target its exact repository
(domain/execution.go:538-558) — so the cleaner §4 semantic won. Revisit at
#305 if a non-waived unattended admission becomes possible.

**The operating-state gate runs at admission recording and at dispatch,
never at reconstruction.** `RequireUnattendedAdmissible` is called from
`RecordExecutionAdmission` (inside the engine's single admitting `Write`,
race-free under SQLite's single writer) and from the dispatch replay branch
before a driver start; `gateAdmission`'s reconstruction re-gate deliberately
excludes it. A stop means "admit no more", not "prior admissions were
illegitimate": re-gating history would make every recorded unattended
admission unreadable (lists, exports, backup closure) the moment an operator
stops, and the engine's Run loop halts on any reconcile error, so it would
take attended work down with it. A test pins the placement.

**A recorded-but-unstarted admission is held by the stop.** The replay branch
gates the stored admission before the driver starts: dispatch of an
already-recorded attempt is still new unattended operation, and a stop
landing between the record and the start must not leak one last launch. The
load-bearing case is an engine restarted without admission configuration —
it runs no dispatch pre-check, so the in-transaction gate is the only barrier
(the refute pass proved exactly one launch leaks without it). Consequence:
acceptance can now meet a recorded attempt whose outbox intent was never
handed to the driver, so `acceptAttempt` skips attempts whose intent is
still pending — a fix the crash window between the admitting transaction
and `Start` needed before any stop existed.

**The engine holds quietly; signet carries the visibility.** While stopped,
dispatch skips pending unattended intents with no error and no failure
items; the store's typed refusals (`ErrUnattendedOperationStopped`,
`ErrBlockingSystemHealth`) map to the same quiet hold when they win the
race. The operator's evidence is the open stopped-notice or blocking item in
signet, not an engine error that would halt the loop.

**Resume rides signet as a first-class action.** Stopping concludes the
decided item, appends the transition, and raises a system-scoped
`system_health` notice offering `resume_unattended` (+ acknowledge);
resuming concludes that notice and appends `resumed`, atomically. Notice
identity derives from the accepted command id (statuses are terminal, so a
singleton id could never be reused after the first resume); a second stop
records its own transition but converges on the existing open notice, since
a duplicate would still block after the other one resumed. Rejected: a
CLI/config resume surface (a flag read at startup is indistinguishable from
restart-resumes, the exact forbidden semantic; signet is the device-gated,
idempotent, durable operator channel). The notice inherits the deciding
item's project id — items require one and 1A is effectively single-project;
a dedicated system project would be its own contract change.

**Admitting-transaction queries use extracted lookup columns, trusted for
nothing.** 0017 adds `item_type`/`status` to `attention_items` (json_extract
backfill, partial index over open rows). Waived notices accumulate one per
admission and acknowledge never resolves them, so a body-decode scan in the
hot admitting transaction was rejected. The columns only select candidates:
matched rows are fully decoded and re-gated, and a column diverging from its
canonical body is refused (`errRowInconsistent`), because a tampered column
could otherwise hide an open blocking item from the WHERE clause. The
backfill coalesces unreadable bodies to the empty default, which matches no
lookup key and still fails closed on decode.

## Verification Findings

**Refute pass (trust-boundary work), all four mutations confirmed by named
failures.** M1, `Supersedes` returning nil (trusting the stored condition):
the retargeted-notice and cleared-waiver tests fail with admissions
proceeding. M2, treating a condition-less open item as non-blocking: the
unconditional-blocker test fails. M3, dropping the `RecordExecutionAdmission`
call: the stopped-admission and both blocking tests fail. M4, dropping the
replay-branch call: initially survived, because the end-to-end test's
dispatch pre-check masked the gate — the gap became
`TestStopHoldsAReplayUnderAnUnconfiguredEngine`, under which the mutation
leaks exactly one driver start while stopped. Each mutation was reverted and
the full suite re-run green.

**Confirmed and fixed by automated review (P1): the lookup columns could
fail open by omission.** A column tampered away from `system_health`/`open`
made the row invisible to the admission query's WHERE clause, so the per-row
cross-check never ran and unattended admission proceeded past a hidden open
blocker; the original forged-column test exercised only direct reads.
`ListOpenAttentionItems` now runs a whole-table SQL divergence count
(columns versus `json_extract` of each body, COALESCE mirroring the 0017
backfill) and fails closed on any mismatch, covering both consumers of the
indexed lookup; the regression drives `RequireUnattendedAdmissible` over
both forged variants plus an honest-blocker control, and disabling the count
was mutation-checked to reopen the bypass.

**Confirmed and fixed by automated review round 2 (P1): the unconfigured
engine skipped the stop entirely for unrecorded intents.** The per-pass stop
check was keyed on a configured unattended environment, so a daemon restarted
without admission configuration ran no stop check at all, and a pending
intent whose attempt and admission were never recorded started unbound —
neither the in-transaction gate nor the replay-branch check exists on that
path. The check now treats a missing admission configuration as unknown and
fails closed (only an explicitly configured attended_dev engine skips it);
every durable window routes through the per-pass check, and the regression
plus a condition mutation prove the unconfigured fresh dispatch holds until
resume.

**Confirmed and fixed by automated review round 3 (P1): pending bookkeeping
is not proof of an unstarted driver.** The acceptance skip treated a pending
outbox intent as "never launched", but Start can succeed and the daemon die
before MarkOutboxDispatched; with a stop then recorded, dispatch held the
row and acceptance skipped it, stranding completed pre-stop work until
resume — against this unit's own rule that a stop halts new starts, never
acceptance. The driver's answer now disambiguates: acceptance always
inspects, and only unknown-and-still-pending reads as unlaunched (unknown
but dispatched stays the lost-invocation failure). The regression starts the
driver directly under the stored admission with the intent still pending and
requires the completed result to be accepted while stopped. The same round's
P2 added the blocking-supersession mirror to the app mock's validation, with
both representable halves seeded in the invariant table.

**Confirmed and fixed by automated review round 4 (P1), closing the round-2
class.** The unconfigured pre-check covered the stop but not the
blocking-health half — the second member of the "unconfigured path misses the
gate" class, proving the boundary was drawn per-condition instead of
per-gate. The predicate is now one shared function
(`RequireUnattendedOperationOpen`): the admitting transaction wraps it and
the engine's per-pass check calls it directly, so no consumer can cover one
half and miss the other; the regression holds an unrecorded intent under an
unconditional blocker until the diagnostic clears, and a stop-only mutation
of the pre-check was checked to leak the start.

**Confirmed and fixed by automated review round 5 (P1): the restored
operating state was a decoded trust bit.** A single-column tamper flipping
the latest transition from stopped to the still-valid "resumed" lifted the
safety gate, and an unbacked row (null command) reconstructed fine. The
transition's authority is now re-derived from the immutable accepted command
it must name: `AuthorizingAction` maps each state to the command action that
authorizes it, the write boundary refuses a mismatched or unbacked
transition, and every reconstruction re-runs the same binding — so the
tampered resumed row fails closed against a command that still says
stop_unattended. Consequence: a transition can only ever be recorded through
an accepted command, so the test paths now ride signet (or seed real
commands) instead of writing bare transitions. The same round's second P1
promoted the resume action into plan §4's system_health row (revision 20,
history migrated), since the plan table and the shipped contract had
diverged — the plan change is the direct subject of this unit and rides this
PR explicitly.

**Deferred at the review-loop cap (automated review round 6).** Two valid,
non-exploitable-today findings were triaged to the tracker rather than a
seventh push: binding the supersession condition to waived-admission
provenance (Follow-up: #360 — no client item-creation path exists and the
engine is the sole producer, so this is defense-in-depth against future
daemon writers), and replacing the per-admission whole-table lookup-column
integrity scan with write-time CHECK enforcement via an attention_items
rebuild (Follow-up: #361 — the scan is correct and fail-closed; the concern
is admission-path cost as open notices accumulate).

**Declined by decision (automated review P1): serializing the final gate
with the driver start.** A stop committing in the window between the
admitting transaction's commit and the external `driver.Start` can let one
already-admitted invocation launch after the stop reads as accepted. #319's
contract binds subsequent *admissions*; an admission serialized before the
stop is prior work, and stop is not abort — the launch is indistinguishable
from one that beat the stop by a second. The unbounded form of the window
(crash before start, replay later) is gated; closing the in-process
milliseconds needs a durable dispatch reservation, which belongs with the
per-run boundary-policy work (#313) or the accepting-transaction re-gate
(#316), not this unit.

**Rejected by verification: accepting completed work while stopped is not a
leak.** Acceptance re-gates through reconstruction, which the stop
deliberately does not touch; stop halts new starts, and results of work
already running are still collected.

**Found and fixed in passing (same class, pre-existing):** `acceptAttempt`
inspected recorded-but-never-started attempts and failed the loop on the
driver's unknown-invocation error; the stop-hold made the window reachable
every pass instead of only after a crash between record and start.

## Revisit When

#305 retires the 1A.2 waiver: every still-open waived notice's condition
then fails validation, making them permanently blocking with no clearing
path short of stopping unattended operation — the retirement unit must
resolve or supersede them mechanically. Also revisit the
condition-versus-configuration scope if a non-waived unattended admission
becomes possible before then.
