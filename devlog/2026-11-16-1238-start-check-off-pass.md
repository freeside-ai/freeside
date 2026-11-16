# Start-Authority Check Runs Off the Engine Pass

Unit #3224 (tracker #3201) moves a task invocation's first start-authority
check out of the engine's reconcile pass. That check compares the
invocation's commit author with the repository's GitHub App bot identity,
which it resolves on GitHub, so a hung GitHub request held the one pass that
dispatches every project's work. It follows
`devlog/2026-11-15-1440-no-locks-across-network.md` (#3215), which moved the
same check in front of the stage driver's lock but left it inside the pass.

## Decisions

- **Chose an engine-side check before `Start` over a pending result from
  `Start`.** The engine runs the stage driver's `AuthenticateStart` outside
  its pass, and a later pass calls `Start` once the check has passed. A
  pending result would change the `exec.StageDriver` contract, every caller
  and test double of `Start` (about ten packages), and put a pending state
  inside the driver. With this shape the driver contract doesn't change and
  no authority decision moves.
- **Rejected a new required `exec.StageDriver` method** for the same check.
  Every engine test would then need a second pass per start. Also rejected
  an optional interface the engine finds by type assertion: a wrapper such
  as `MaterializingStageDriver` hides it without an error, so production
  would silently fall back to the in-pass check. The engine takes the check
  as an explicit option (`WithStartAuthorityCheck`), and only the production
  composition installs it.
- **The engine's check is not the gate.** `Start` still runs the same check
  before its lock, now answered from the authority's per-invocation cache,
  and the full re-check under the lock is unchanged. A passed engine check
  only decides when `Start` is called; a refused one fails the start through
  the same path a refused `Start` takes, releasing the dispatch reservation.
- **Chose a pending hold that records no reason,** as for a triage call in
  progress (#3215). The check is the daemon's own short work and nothing
  waits on an operator. A recorded reason would also reach `domain`, the API
  schema, and the app for a hold that lasts one pass in the normal case.

## Costs Accepted

- **One more pass per production start.** The pass that reaches an
  invocation starts the check; the next one starts the invocation. The pass
  interval is 100 ms by default.
- **No retry backoff.** A failing check is retried when a later pass finds
  its answer, so GitHub sees at most one call in flight per invocation, as
  often as a failing start retried before.
- **A pending start holds its admission.** Its pool slot, memory, and
  `dispatching` reservation stay taken while the check runs, up to the
  GitHub client's timeout, with no hold reason shown.
- **A refusal for another reason costs a pass.** After `Start` refuses for
  any reason, the retry runs the check again first, answered from the cache.
- **Every production start now replays its attempt.** The pass after the
  check reads the recorded admission back, so it re-runs the replay gates
  and the production delivery check (a local Inspect and materialization,
  no network call). The pool and memory gates count the invocation's own
  standing reservation as held, so they pass.
- **A closed operating gate can strand a reservation.** If a stop or a
  blocking `system_health` card closes the unattended gate while the check
  is pending, the replay holds before the engine reaches the check's answer,
  so the row stays `dispatching` and restart readiness counts it until the
  gate reopens. Before this unit only a crash between reservation and
  `Start` left that state. The window is one pass, or the GitHub timeout
  when the check hangs. Releasing the row there was rejected: a replayed
  `dispatching` row may be one an earlier process handed to the driver, and
  only a refused `Start` proves it wasn't. Running the check before the
  reservation would avoid it, but the issue contract keeps the reservation
  while the check is pending. Found by the unit's refute-first review.

## Refute-First Record

A fresh-context reviewer tried to break the change before it was committed.

- **Disproved:** a start the authority refuses can reach the driver. `Start`
  authenticates before its lock and before its duplicate check, and the
  re-check under the lock is unchanged; without the option the path is
  unchanged.
- **Disproved:** a refused check leaves different state than a refused
  `Start`, including on an `ErrDuplicateStart` replay. Both go through one
  helper.
- **Disproved:** a canceled or stale answer starts work. A check whose
  context ended stores nothing, and a stale pass still meets `Start`'s own
  check.
- **Confirmed and fixed:** the shutdown test could pass without `Run`
  joining the check, because the stalled check returned on cancellation.
  The check now ignores its context until released.
- **Confirmed and allowed:** the stranded reservation under a closed gate,
  above.

## Known Gap

After a restart the authority's cache is empty, so the first Inspect of an
invocation that was seeding or running can still call GitHub under the
driver's lock, inside the pass (`regateWithCurrentPolicy` in
`daemon/internal/exec/stage/state.go`). This unit leaves it.
Follow-up: #3268.

Revisit when the start-authority check gains a durable cache, or the
commit-author check leaves `AuthenticateStart`: either could make the
engine's check unnecessary.
