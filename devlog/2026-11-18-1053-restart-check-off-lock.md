# Run a Restarted Invocation's Current-Policy Check Off the Driver Lock

After a restart, the stage driver's `Inspect` re-checked a preterminal
intent against current policy under the driver's lock, and on a
commit-author cache miss that check reaches GitHub. One stalled request then
stopped every invocation's `Inspect` and session commit, and the engine pass
with them. #3268 moves that check off the lock and out of the pass. The work
is Tier A by function: it decides when recovery may follow the
current-authority re-check, so `internal/exec/stage/driver.go` joins the
Tier A Paths list.

## Decisions

- **The background check reuses `loadIntent`, not a pre-lock
  `AuthenticateStart`.** `loadIntent` is the full current-policy
  reconstruction, so it calls `AuthenticateStart` or `AuthenticateImport`
  exactly where the gate in `state.go` does, and covers the import re-check
  of an exported intent with a current import start. A pre-lock
  `AuthenticateStart` would have copied the gate's phase rules into
  `driver.go` and missed the import check. `state.go` and `admissionauth`
  are unchanged.
- **The check is asynchronous, not synchronous before the lock.** #3215's
  rule is no lock and no engine pass across a network call. A synchronous
  check before the lock would still hold the pass, so `Inspect` reports
  running, starts the check on the recoveries wait group (so `Close` waits
  for it), and a later `Inspect` reads its answer.
- **The "checked" mark only schedules, and once.** It lets the next
  `Inspect` take the old path, which runs the full re-check under the lock
  again (now a cache hit) before any recovery. That `Inspect` consumes the
  mark when it launches recovery. A stale mark can't let a later refusal
  through, and recovery never starts on the background check's answer
  alone. The mark is one-shot because recovery can empty the cache the
  locked re-check relies on: `AuthenticateImport` uses the same
  per-invocation commit-author cache, and import forgets the entry. A
  persistent mark would put every retry of a retryably failing import back
  under the lock on a cache miss, for up to the #1181 grace. The
  fresh-context review found this.
- **Both marks and refusals are bound to the phase they checked.** A
  `Cancel` can move a restarted intent to another phase, whose gate differs
  (an exported intent with no current import start runs no start check).
  A refusal for the old phase would otherwise reach the engine once, and a
  pass for the old phase would send the locked path into a re-check the
  background check never warmed. A phase change starts a new check.
- **One map of check states, not three maps.** Each invocation's check is
  running, passed, or failed with its refusal and phase, so it can't be
  both checked and failed.
- **A live session skips the detour, and `StartWithInputs` marks nothing.**
  A session exists only after `Start` or recovery ran the full check, and
  `Reconcile` can resume a pipeline before the first `Inspect`. Detouring it
  would turn `Live: true`, which the engine reads, into a plain running
  answer. The plan also had `StartWithInputs` mark the invocation checked;
  the live skip already covers its session, and after the session ends
  with a preterminal intent (an import that forgot the cache, say), the
  detour keeps the re-check off the lock where a mark would not.
- **The intent's admission-only reconstruction, under the lock, decides
  whether to detour.** It reaches no network: `AuthenticateAdmission` reads
  the recorded admission, and the rest is store reads and local files. It
  answers unknown and terminal intents as before, so they take no detour.

## Costs Accepted

- One engine pass of delay before each recovery launch: a restarted
  invocation, or one whose recovery keeps failing retryably, recovers every
  other pass. A refusal surfaces one `Inspect` later, and on later passes it
  alternates with running (error, then a new check), as `recoveryFailures`
  does.
- After startup `Reconcile` runs the same check under the lock, the first
  `Inspect` still detours once; the check is then a cache hit.
- Residual gaps: the cache entry can drop between a passed check and the
  locked re-check one pass later (the authority's 1024-entry clear, or a
  changed repository or App binding), and then that one re-check runs under
  the lock. A live session's `Inspect` still re-checks under the lock, as
  before this change; after its import forgot the entry, that is a cache
  miss.

## Refute-First Record

Attacks run before committing, each with its result:

The agent's own checks and a fresh-context adversarial reviewer each tried
to prove the change wrong:

- Never-checked or refused invocation reaching recovery: disproved by
  `TestRestartCurrentCheckHoldsNoDriverLock` (no recovery launch while the
  check hangs) and `TestRestartCurrentCheckRefusalBlocksRecovery` (refusal
  returned with its `authenticate current intent` chain, no launch). The
  reviewer traced every `startRecovery` call and found none without a
  passed mark and a successful locked `loadIntent`.
- Stale "checked" mark letting a later refusal pass: disproved by reading;
  the locked `loadIntent` runs on the `Inspect` that uses the mark.
- Mark outliving the cache, so a retried import re-checks under the lock:
  confirmed by the reviewer; fixed by the one-shot mark, and
  `TestRecoveryAttemptRechecksOffTheLock` pins it.
- Stale refusal after a phase change: confirmed by the reviewer; fixed by
  binding checks to their phase, and
  `TestRestartCurrentCheckRefusalIsBoundToItsPhase` pins it.
- `currentChecks` growing without bound: allowed. With no mark from
  `StartWithInputs`, entries come only from detours, and a recovery
  launch, a returned refusal, or a terminal `Inspect` removes them.
- `loadIntentAdmission` reaching the network: disproved by reading; it
  reads the store, the ward journal, and local files.
- The new tests passing without the fix: disproved by mutation. Each of
  the detour, the mark's consumption, the phase binding, and the
  ended-context rule, when removed, fails its test.
- Data races on the new map and the unlocked `loadIntent`: none under
  `go test -race` of the stage package.

## Revisit When

- `loadIntentAdmission` gains a network call; the locked detour decision
  would then reintroduce the stall.
- `loadIntent` starts writing state; the background check would then need
  the lock again.

Follow-up: #3381. Startup `Reconcile` still runs the check under the lock
before the engine loop, and a GitHub failure there quarantines the intent
instead of holding it.
