# Record the Daemon Supervision Contract Gap

**Decision.** The daemon's fail-loud posture depends on a supervisor
contract that was deferred and never written, and nothing supervises
freesided at all; the owner directed that the gap be recorded and the
work ticketed. Two units are filed into the unscheduled deferral queue
with wave-7 (1B.1) affinity: a contract-definition unit (a plan §5.2
revision) and an implementation unit dependent on it. The endorsed
starting direction: convert deliberate fatal stops into durable
in-process holds, keep the supervisor policy trivially
restart-always, and watch liveness from outside the process.

## The Gap, Verified at c3ad9d0

- **Fail-loud is inert without a supervisor.** Several conditions are
  deliberately daemon-fatal so that failing loudly beats continuing
  blind: a scheduled doctor pass whose operational source errors, a
  janitor pass failure (both now routed through the §5.16 scheduler
  loop, `daemon/cmd/freesided/scheduler.go:306-307`, kept
  daemon-fatal by design, `daemon/cmd/freesided/main.go:601-607`).
  Nothing starts freesided at boot or login and nothing restarts it:
  the only launcher is the `scripts/run-real-work.sh` harness, which
  backgrounds the daemon and kills it on exit. With no one watching a
  terminal, "loud" is an indefinitely-off daemon with no signal to
  the operator.
- **A supervisor could not act on today's exits.** Every deliberate
  fatal path collapses to exit 1; a janitor stop, a doctor source
  error, and a store failure are indistinguishable to any restart
  policy. Exit 2 is no cleaner: the daemon uses it deliberately only
  for flag/usage errors, but an unrecovered Go panic also exits 2, so
  the one differentiated code is already ambiguous.
- **Graceful shutdown is unbounded by design.** `Close()` awaits
  credential-bearing session teardown with no timeout
  (`daemon/cmd/freesided/main.go`, credential-lease comment in
  `Close`); a service definition with a default stop timeout would
  SIGKILL mid-lease.
- **No death notice exists.** The only operator channel (ntfy via
  signet) is wired inside `run()`, so the daemon cannot report its own
  death; the §5.12 stall heartbeat detects a stalled run, not a dead
  process. The plan's entire supervision spec is one bullet
  (`docs/plan.md` §5.2: "A supervisor runs it under a dedicated
  user"), plus launchd installation named as elevation-helper work.
- **The debt is already on record.**
  `devlog/2026-07-30-2350-operational-command-packaging.md` kept the
  doctor source error fatal explicitly pending "the supervisor
  contract"; that contract was never written, and no wave schedules
  it. This note is that revisit condition firing.

## Endorsed Direction (for the Contract Unit to Argue)

1. **Shrink the deliberate-fatal surface toward zero, by inventory,
   not by example.** The contract classifies every writer to the
   daemon's fatal error channel, which at c3ad9d0 carries not only the
   scheduler loop (doctor and janitor) but HTTP serve errors, the
   reconcile loop, local backups, and the production-publication
   lane, several of which return runtime correctness or maintenance
   errors rather than crashing. Each classified condition becomes a
   durable stop (close unattended admission, raise an AttentionItem,
   keep the process up serving read-only state) or is explicitly
   recorded as a restart-safe exit; the doctor source-error and
   janitor-failure fatals are the worked examples, not the extent. This is louder than exiting, because the
   fail-loud rule's real goals (the operator finds out; nothing runs
   blind) are both met by a live parked daemon and neither by a dead
   process. The precedent is
   `devlog/2026-07-31-0907-complete-production-pipeline.md`: a state
   that "invited a supervisor restart loop" was reworked into an
   idempotent high-priority hold, and plan §4's `system_health` rule
   already makes a stop a durable transition a restart alone never
   reopens.
2. **Supervisor policy becomes restart-always, throttled.** This is
   sound only after item 1's inventory leaves no unclassified worker
   exit; then the remaining exits are crashes, invariant panics,
   startup failures, and the explicitly recorded restart-safe cases,
   restart-with-backoff is correct for all of them, and durable stop
   state makes an aggressive supervisor unable to hide a blind spot. A launchd
   LaunchDaemon (dedicated user, per §5.2) with `KeepAlive` fits; the
   plist lands via the §5.2 elevation helper. The stop side is a fork
   the contract must close, not a tunable: credential-lease teardown
   is unbounded by design, so any finite stop grace, however
   generous, recreates the SIGKILL-mid-lease hazard above; the
   contract either requires an effectively unlimited stop wait
   (launchd's exit timeout disabled) or first establishes a bounded,
   credential-safe teardown mechanism.
3. **Liveness is watched from outside the process.** A separate
   launchd-scheduled probe checks daemon liveness and notifies over
   ntfy on failure or on a crash-restart loop. The contract must name
   the probe target: at c3ad9d0 the daemon exposes no readiness
   endpoint (readiness is a one-time startup JSON on stdout, and the
   signet handler registers no such route), so the choice is adding a
   liveness route or probing an existing authenticated one. Plan
   §5.16 already rules that stateless process heartbeats stay plain
   tickers, so this does not touch the durable scheduler. This is the
   piece that covers the owner's stated need: the daemon staying up,
   and its death being noticed, while the operator is away from the
   host.

**Rejected alternative: an exit-code protocol.** Distinguishing
restart-safe from stop-and-summon exits via exit codes was rejected
as the primary mechanism, on the durable-state rationale rather than
expressibility: launchd's `KeepAlive` `SuccessfulExit` condition can
express a binary zero/nonzero restart policy (though nothing finer),
but a stop carried only by an exit status is recorded nowhere
durable, an unrecovered Go panic already collides with the one
differentiated code, and the project's recorded direction moves stop
semantics into durable state, where a respawn cannot erase them. The
contract unit may still assign meanings to the residual codes; it
just must not make correctness depend on them.

## Placement

Not wave 3: the 1B.0 slate (#445) is loop foundations, and plan
revision 26 concentrates the hardening drain in wave 7 (1B.1), where
the wave-3 sweep had already re-deferred the adjacent operational
units (#435, #438, #424, #428). The pair therefore entered through
the deferral protocol's unscheduled queue rather than the current
wave, and no milestone was assigned at filing, because a milestone
without a tracking-issue listing is the recorded spine-repair error
(#252). Scheduling state lives on the issues, not in this note;
earlier pickup is owner fiat.

Follow-up: #453
Follow-up: #454

Revisit when: the wave-7 (1B.1) planning sweep runs, or sooner by
owner fiat if unattended away-from-host operation is needed before
then, in which case the contract unit is the head of the pair.
