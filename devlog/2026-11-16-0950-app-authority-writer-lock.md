# App Authority Writer Lock and the Recorded Execution Pause

Unit #3204 (tracker #3201) makes the daemon enforce that the GitHub App
authority has one holder at a time, and adds `freesided execution pause` and
`execution resume` so an executing `prod` can hand that authority to a
deputy run without stopping its service. The issue body carries the
contract; this note keeps the decisions that rejected a reasonable option
and the record it revises.

## Record This Revises

**"Two sides, no locks"** in `2026-10-20-1013-prod-runs-driverless.md`.
That note accepted a gap: `prod` checked for a rig lease before it opened its
database, and a deputy read a missing control advertisement as "`prod` is
not running", so a `prod` between its check and its advertisement was
invisible to a deputy that took the lease in between. The gap was safe only
because the installed LaunchAgent passed no driver. Once `prod` executes
under `KeepAlive` (#2083) it reaches the gap on every restart, so the
assumption no longer holds. The two start-time checks stay; a lock now sits
beneath them.

## Decisions

**A kernel lock held for the authority's whole use, over sharper start-time
checks.** Every process that opens `prod`'s App state directory first takes
a non-blocking exclusive `flock` on `app-authority-writer.lock` there, and
holds it until it exits or closes the authority. A check followed by an open
is a race however carefully the check is placed; a lock held across the use
is not. `flock` also dies with its holder, so a killed holder leaves nothing
to recover, unlike the rig lease. The lock is a parameter of the keystore
and authority store constructors rather than a package global, so a
production path that opens the authority without it fails to compile.

**A fixed order on each side.** `prod` reads the pause, takes the lock, then
looks for a rig lease manifest; a deputy takes the lease, then the lock.
However two starts interleave, one sees the other: a `prod` that took the
lock first refuses the deputy's lock, and a deputy that took the lease first
leaves a manifest that `prod` finds after it locks. Both may decline at the
same moment, which is the safe side.

**`prod` degrades instead of exiting.** A `prod` that is paused, finds the
lock held, or finds a manifest releases any lock it took and serves without
a driver, with a blocking `system_health` card that names why and the
command that restarts execution. The old exit with status 2 was rejected:
under `KeepAlive` launchd restarts at once, so an exit under a lease
crash-loops for the whole deputy run and takes the board and the phone
with it. A daemon hand-started with `-prod-app-authority` is not under
`KeepAlive` and still exits with status 2 when the lock is held.

**The pause is a recorded settings section that closes new work, not an
operator-stop transition.** An operator stop must cite an accepted client
command (`requireTransitionCommand`), which a CLI does not have. The pause
lives in the instance settings' `execution_pause` section, so it survives a
restart, and every site that refuses new background work under the update
drain also refuses under the pause, with the paused tasks showing the
system-health hold. The pause is not the update drain, which stays reserved
for swapping builds, so a new build's start never lifts a pause.

**The pause card is advisory, not blocking as the issue said.** A blocking
`system_health` card closes the unattended operating gate, which stops
dispatch of an attempt admitted before the pause as well as new ones. That
attempt would then hold its slot, restart readiness would never reach zero,
and the pause would never release. New work is already closed by the
section itself; once `prod` restarts driverless, its `execution_paused` card
blocks.

**The forgotten-pause check lives in the pause waiter, not the doctor
pass.** The plan put it in the doctor pass, but that pass runs only in a
daemon with a driver, and a paused `prod` never has one. The waiter runs in
every `prod` started with a driver, so it sees a released pause; after an
hour with no rig lease manifest it raises an advisory
`execution_pause_unused` card.

**Resume restarts only a `prod` without execution.** Resume restarts a
driverless `prod` so it retries its start order, but an executing `prod`
whose requested pause has not released keeps running: a restart there would
interrupt the very work the pause was waiting on.

**The pause command waits for a `prod` without execution.** The `prod` that
released the pause still answers, and may still hold the writer lock, while
it tears down for its restart. So `execution pause` returns only once a
`prod` that does not execute reports the release, and a deputy started next
finds the lock free.

## Refute-First Record

A fresh-context reviewer tried to refute the branch before push. It found
no path that lets two writers use the App authority at once. Its other
findings, and a full-suite failure, were settled this way:

- **Confirmed and fixed.** Resume restarted an executing `prod`. The pause
  command returned while the old `prod` might still hold the lock. The
  move script's comment overstated what its lock covers. A hold comment
  named a blocking card that does not exist. One test checked an
  in-memory field, not the kernel lock. Nothing showed a failed start
  releasing the lock.
- **Confirmed by the full suite and fixed at the root.** A `Close` racing
  another goroutine's fork left the lock held, because the forked child
  shares the open file description until it execs. `Close` now unlocks
  explicitly, which releases the lock for every sharer.
- **Allowed by decision.** A `prod` that started beside a short lock
  holder stays driverless until resume, with a card that says so; a retry
  is follow-up #3246. A waiter error is a durable stop, as the plan
  specifies, so it fails loudly. The move script locks prod's directory,
  not the source; a deputy using the source is refused by the rig manifest
  check. No test drives a configured `prod` or the deputy's own start,
  since a test cannot compose a driver; the gate and lock tests cover both
  orders.

## Remaining Gap

The lock is close-on-exec, so no child process keeps it past its exec. A
publish's git child can therefore outlive a `prod` killed mid-push for a
moment after the lock is freed, and a deputy could take the lock while it
runs. Publication recovery already reconciles a push whose outcome the
daemon did not record, so this gap was left open rather than closed by
making children hold the lock.

Revisit when a process other than `prod`, a deputy run, and the operator
commands needs the App authority, or when `prod` gains a second process.
