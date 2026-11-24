# The Stage Driver Launches a Cordoned Writer Through a Second Port

The Claude stage driver now routes a start by `exec.StartSpec.RunnerClass`:
an empty class runs in the ward as before, `cordoned_host_process` runs
through a new `stage.CordonGate` port that the composition adapts to the
cordon backend, and any other value is refused before an intent is saved.
Nothing admits a cordoned run until #3472, so no running behavior changes.
Issue #3584, tracker #3462. The choices below follow the plan comment on
#3584 unless a line says otherwise; departures are the agent's
implementation decisions.

## Chose a Second Port Over Reshaping the Ward's

Ward's `Handoff` runs the writer and exports in one call, authenticated by
a handoff journal. The cordon's `Launch` blocks until the run's end is
proven, `Export` is a second call, and its recovery reads files under the
run. Folding both into `HandoffSpec` would have changed the warded
journal's bytes, whose digest the ward re-checks on recovery (#3465's plan
finding, which the issue adopted). So the cordon
gets its own port and types, the provider gets an optional
`CordonLaunchProvider`, and the driver branches at each point the two
differ, then shares everything after the release: import, the export
record, the outcome, and failure evidence. The warded goldens and tests
pass unchanged.

Rejected: treating any non-empty class as cordoned. A journaled spec's
class isn't validated against the enum on its own, so the driver matches
`cordoned_host_process` exactly and refuses the rest.

## Chose Detach Before Any Cancellation, Enforced in the Adapter

A canceled cordon `Launch` context cancels its run for good. Before this
unit, `daemonshell.Shell.Close` canceled the process context before it
closed sessions, and the driver's lifetime was that context, so a restart
would have canceled every cordoned writer. Now:

- **The driver's lifetime ends after detach.** With a cordon, the lifetime
  is detached from the process context and ends only after an `AfterFunc`
  has run `Detach`.
- **The gate closes first in the session group,** so `Shell.Close` detaches
  before `Driver.Close` runs, whichever path gets there first.
- **`Driver.Close` requests no cancellation of a cordoned run.**
- **Departure: a detaching flag guards each launch's context.** The plan
  relied on order alone. The backend's wait selects between the context
  and its detach signal at random when both are ready, so a cancellation
  racing a detach could still reach the run. The adapter gives each
  `Launch` its own context and passes the caller's cancellation through
  only while the gate isn't detaching. A token read that a shutdown cuts
  short also returns detached (refute-first round 1).

## Chose Durable Records Over the Cordon's Bytes Once an Import Decided

The cordon authenticates an export by hashing its directory's members
against `ctl/export.json`. Ward compares the path with a journal row that
outlives the directory. The driver removes the export directory once the
outcome is durable and calls `ReleaseFinished`, which removes the run, yet
the stage regate re-authenticates an intent's `Export` on every load.
Without a change, a successful cordoned run could never commit or be read
(refute-first round 1, finding 1). The driver now authenticates a cordoned
export this way:

- **A terminal intent** is authenticated by its durable export record or
  outcome, which the regate already checks, with no call to the cordon.
  Nothing reads or removes a terminal intent's export directory. So its
  history stays readable once its run is released, and after the cordon is
  unconfigured; the composition passes the runs root, resolved as the
  cordon's configuration resolves it, whether or not a cordon is composed
  (round 2, finding 3).
- **Any other intent** still needs the cordon's check, with the path bound
  to `<RunsRoot>/<runID>/export` first. A refusal stands unless the
  directory is gone, leaving nothing to read, or a durable export record
  already exists, which the regate binds to this export next. The second
  case covers a crash partway through the removal after a finished import
  (round 2, finding 2).

Rejected:

- **Clearing `Export` on a terminal intent.** It changes what every
  committed intent records and what recovery reads, for both classes.
- **Keeping the cordon's record outside the run.** That needs new durable
  state in the cordon, outside this unit's scope, to restate a fact the
  store already holds.

## Chose to Leave a Live Run's Recovery to Inspect

A cordoned run outlives a restart, and the cordon's `Recover` waits for a
live supervisor to end the run. Startup's `Reconcile` pass runs each
intent's recovery inline, so it would have waited for the whole writer
before the daemon started (round 2, finding 1); ward never hit this
because its recovery tears a live writer down. `Reconcile` now holds a
running cordoned intent as retryable, and the attempt's next Inspect
recovers it in `startRecovery`, off the caller. Canceling that recovery's
context stops the wait and leaves the run alive.

## Other Decisions and Departures

- **The pre-job probe holds; the launch proves again.** `PreJob` runs the
  suite's `ProbeBeforeJob`, so a class whose proof doesn't stand holds an
  attempt (`exec.ErrPreJobRefused`) instead of failing it, and `Launch`
  runs with `RequireProof`, which closes the gap between the two.
- **Departure: the proof is required when the daemon or the admission is
  unattended.** The plan named the admission's mode. Either alone requires
  it, so a mode change between admission and launch never drops the proof.
- **The token comes from the admission's own generation.** The adapter
  reads the durable admission, refuses one that names another backend or
  binds no generation, and reads that generation's setup token.
- **Departure: failure evidence uses `ward.FailureEvidence`.** The plan
  sketched a raw transcript. The adapter keeps a failed run's transcript
  and stderr only when bounded, valid UTF-8, and passed by the export
  credential scan, as ward does, so the driver's existing evidence path
  serves both classes.
- **Departure: a launch error after the run started is left to
  recovery.** The plan failed every launch error inline. A started run
  may still be alive under its supervisor, so the driver records nothing
  and lets recovery adopt it; only a launch refused before its run
  directory existed fails inline, with a fixed summary.
- **Attachments are refused, not delivered.** The cordon has no input
  mount; delivering them needs new cordon surface.
- **The cordon's CLI settings match the warded writer's.** The cordon
  refuses a second `--settings`, so its fixed settings now also turn off
  commit attribution and `agents-md@builtin`.
- **The regate ties egress to class.** A cordoned intent must carry
  `host_open`; a warded one keeps the two profiles ward enforces.

## Risks Not Closed Here

- **Only the operator's host instructions reach a cordoned writer.** They
  go in argv through `--append-system-prompt`, capped at 256 KiB under
  macOS's 1 MiB argument limit, and are visible to the account in `ps`.
  The repository `CLAUDE.md` files ward composes from the trusted base
  aren't delivered; that needs an instructions file the launch can read.
  Follow-up: #3588.
- **A seed failure inside `Launch` ends the run lost.** The cordon claims
  the run directory before it seeds and refuses an existing one, so a
  transient seed failure can't relaunch; recovery ends it lost and the
  engine allocates another attempt. Acceptable for a dormant port; #3472's
  live run should watch for it.
- **No usage readings and no live transcript** for a cordoned run.

## Refute-First Findings

Three rounds of fresh-context review tried to prove the change wrong before
push; the third found no blocker. All confirmed the class switch, the refusals before save, the
egress regate, the export path rule, the session-group order, the launch
context guard, and the token's generation. Findings, by outcome:

- **Fixed (blocking, round 1):** a cordoned export failed authentication
  once its directory was removed, so a successful run could never commit
  or be read. The fake cordon accepted every export, which hid it; it now
  authenticates and releases as the real backend does, and the happy path
  and recovery tests fail without the fix.
- **Fixed (round 1):** a shutdown during the token read recorded a
  canceled outcome for an attempt that never started.
- **Fixed (round 2):** startup recovery waited on a live cordoned writer;
  a crash partway through removing a finished export quarantined the
  intent; and a daemon without the cordon could not read a cordoned
  intent's history. Each has a test that fails without its fix.
- **Fixed (round 2, docs):** `CordonExportRefusal` claimed its reason
  never names an agent-chosen path, but a scan refusal names the item, as
  ward's does; and `CordonEnd.Failure` said stderr follows only a nonzero
  exit, but a stall keeps it too.
- **Declined:** a failure-transcript read or scan error keeps the run
  retrying through recovery rather than recording unavailable evidence.
  Ward's capture returns the same errors for retry
  (`internal/ward/failure_evidence.go`), and the adapter matches it.
- **Accepted:** a crash partway through removing an export on the
  definitive-rejection path, before any durable record, leaves a partial
  directory the cordon refuses, so the intent is quarantined for a person.
  The window is the removal itself, and quarantine fails closed.
- **Fixed (round 3):** releasing a retried commit's names had made the
  warded release depend on reading the intent back; the warded release is
  unconditional again, as before this unit. A comment claimed a seeding
  intent with no cordon composed stays rerunnable; it fails, as a warded
  handoff spec refusal does, and the comment now says so.
- **Deferred (round 3):** a stop that races a clean exit records failed,
  where recovery would record canceled: the cordon's end record reads
  canceled once its cancellation marker exists, so `Export` refuses the
  run as not exportable. Nothing is exported either way. Telling the two
  apart needs a typed error from the cordon, outside this unit's scope;
  follow-up: #3589.
- **Declined (round 3):** a test pinning the session group's close order.
  The adapter's detach guard and the lifetime's end after detach each have
  a test, and either alone keeps a shutdown's cancellation from the run.

Revisit when #3472 admits a cordoned run and its live runs show how often
a seed failure ends a run lost, or when the repository's instructions
must reach a cordoned writer.
