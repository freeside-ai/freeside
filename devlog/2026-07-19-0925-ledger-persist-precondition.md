---
run: manual
stage: ledger-persist-precondition
date: 2026-07-19
branch: fix/ledger-persist-precondition
---

# Make pending-command persistence a submission precondition (issue #163)

Saddle-lane P1 fix from the Wave 1 adversarial audit (#83, "Finding
remediation lanes" parallel pass B). Declared paths: `app/` plus this
note. Mandatory note: the unit changes the write-failure semantics of
the #115 persistence path (a credential-leak / reconstruction surface),
and its core choice (fail the send closed on a lost durable write)
rejects plausible alternatives.

## The gap #163 closed

#115 made the pending-command ledger persist so an unresolved command's
verbatim-resend affordance survives a relaunch (plan §5.14 sync test 4).
But that durability was observer-driven best-effort: `registerPendingCommand`
claimed the in-memory slot, fired a non-throwing `pendingCommandsObserver`
→ `SyncCoordinator.persist()` → `CacheStore.save`, and `save` swallowed
every error with `try?`. `DecisionModel.submit` sent the command
regardless. A failed write + committed command + lost response + process
exit then loses the only reusable `command_id`: relaunch has no entry to
replay and no slot to block a duplicate mint. #115's own note filed this
as observer-best-effort; #163 promotes it to a checked precondition.

## Decisions

- **Durability is a precondition of sending, not a side effect.**
  `registerPendingCommand` now claims the slot, attempts the durable
  write, and on failure rolls the slot back and refuses; `submit` sends
  only when the entry reached disk. Fail closed: an unpersisted
  `command_id` never leaves the client.
- **Each layer reports failure in its natural vocabulary.**
  `CacheStore.save` throws (idiomatic I/O failure). `persist()` catches
  and returns `Bool` (a domain "durably persisted?" signal), staying
  `@discardableResult` so the best-effort callers are untouched. The
  ledger observer returns that `Bool`. `registerPendingCommand` returns
  `PendingCommandRegistration { registered, slotOccupied, notPersisted }`.
  Rejected: a bare `Bool` from `register` (can't tell an occupied slot
  from a failed write, so `submit` can't surface the right thing — a
  slot race is a silent no-op, a persist failure is a visible error); a
  second throwing "durable-write" protocol method (two write methods
  muddy the protocol for one call site); leaving `save` best-effort
  (the bug).
- **Only the first registration is a hard gate.** Post-send transitions
  (`setPendingCommandState`, `clearPendingCommand`) keep firing the
  observer but ignore its `Bool`: they run after the command already
  left, and a lost write there only offers an idempotent verbatim resend
  on the next relaunch. The read-cache/cursor persists (`adopt`,
  `discardCache`, `observe`) likewise stay best-effort — a lost snapshot
  save still costs only one bootstrap.
- **Rollback needs no compensating write.** The atomic write leaves the
  prior file byte-for-byte intact on failure, and rollback removes
  exactly the key just inserted under the empty-slot guard, so memory
  and the untouched disk file return to the precise pre-claim state
  (other items' entries and cursors preserved on both sides).
- **A bare store with no observer still registers.** The
  no-observer-wired path (`.registered` without a durable write) is
  reachable only by a bare `InboxStore` in unit tests; production always
  runs a `DecisionModel` over a `SyncCoordinator`-owned store whose
  `init` wires the observer unconditionally, so `.registered` there
  always means the write reached disk.

## Refute-first verification (persistence / credential-adjacent path)

A fresh-context lens, given only the diff and the issue's intent, tried
to disprove three claims: (a) no `command_id` reaches `submitCommand`
without a durable on-disk entry; (b) rollback cannot strand a slot,
remove the wrong entry, or lose a committed outcome; (c) best-effort
read-cache/cursor persistence is not regressed into a spurious hard
failure. It attacked the unwired-observer path, the `persist()`
empty-state early return, the `weak self` nil case, atomic-write
disk consistency, the ignored post-send `Bool`, whether the new tests
truly prove "nothing sent", and Swift soundness.

**All three claims survived; no defect found.** Load-bearing
confirmations recorded so they are not re-litigated:
- (a) The `persist()` `cursors == nil && pending.isEmpty → discard;
  return true` shortcut is **unreachable at registration**: the entry is
  inserted and the observer fires synchronously (same `@MainActor`, no
  `await`) before `persist()` reads the ledger, so `pending` is non-empty
  and a real `cache.save` runs. `.registered` ⟺ a durable write of a
  state containing the command.
- (b) The `.atomic` write means a failed `save` leaves the prior file
  intact, so "disk untouched" is accurate; rollback restores exact
  pre-claim memory. At first registration nothing was sent, so no
  committed outcome exists to lose.
- (c) `persist()` never rethrows (it catches and returns `Bool`), so no
  throw escapes any best-effort caller; the only non-test `cache.save`
  caller is `persist()` itself. The `ConvergenceHarness` `CacheStore` is
  a parameter type, not a conformance, so the throwing signature breaks
  no call site.

**Rejected by verification (do not re-raise):** an unwired-observer
production path (none — only `DecisionDetailView` constructs a
`DecisionModel`, over a coordinator store); a false-durable via the
empty-state shortcut; a `submitCalls`-hook mismatch in the new tests
(the counter increments in the transport `send`, so it fires only on a
real request that leaves the client).

**Accepted by decision (residual, with rationale):** if a
`SyncCoordinator` were deallocated while its `store` stayed live under a
`DecisionModel`, every registration would return `.notPersisted` and
wedge submissions. Traced as **not reachable** (once `.ready(coordinator)`
is entered the coordinator is stable for the app's lifetime, and any
phase swap tears down the whole `DecisionModel`/store subtree together),
and even if reached it fails in the safe direction (never sends an
unpersisted command) — a pure liveness concern, not a correctness one.

Revisit when: a second durable client-mutation ledger joins the cache
(conversations or runs) — the fail-closed gate and the single-slot
rollback are tuned for the one-command-per-item ledger.
