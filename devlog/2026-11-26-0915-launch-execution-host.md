# Record the Host a Launch Runs On

Issue #3641. A contract unit, Tier A: the `StartSpec` field decides which
machine a launch and its credential go to. Source note:
`devlog/2026-11-25-0151-cordoned-runs-on-hosts.md`; the host record it builds
on: `devlog/2026-11-26-0457-execution-host-contract.md`.

## Decision

An execution admission names the host its launch runs on, the start spec
carries it to the stage driver, and a `cordoned_launch_cap` hold names each
host the waiting launch was considered for and what closes it. Every launch
names the daemon's own host, `daemon`, until admission chooses among hosts
(#2166). The stage driver refuses a new cordoned start that names no host,
and any start that names another host, because no other host has a stage
gate yet (#3625).

The four choices in the issue's Planning Notes stand as planned: one column
on `execution_admissions` (not on `launch_reservations` or `launch_waits`);
a stored launch reads as the daemon's host in the column, the admission's
reader, and its facts; no cap set reads as `cordoned_count_full` with a limit
of 0; and the hold names hosts by id only.

## Choices and What They Rejected

- **A new encoding version, used only when the host is set,** over adding the
  field to the current encoding. An admission is content-addressed, so a
  field in the existing encoding would change every stored id. Version 9
  carries the host; an admission without one still encodes as before, and a
  stored body keeps its id and bytes. A body that names a host under an
  older version fails its id check.
- **`Host()` reads an unset host as the daemon's,** over leaving it empty.
  Every launch before this unit ran on the daemon's host, so this states a
  fact about the record, not a guess from current state. The facts and the
  store column use the reader; review facts stay null because a review names
  no host.
- **`StartSpecFromAdmission` copies the raw field, not `Host()`,** over
  copying the reading. Start authority compares a journaled spec with the
  admission's render exactly (`internal/admissionauth/authority.go`), so an
  intent journaled before this unit must still match. A reading would turn
  its empty field into `daemon` and refuse it.
- **The driver refuses by the raw field, and only a new start,** over
  refusing at admission or on recovery. The refusal comes after the
  duplicate-start lookup and before any probe, bind, or save, so a replay
  still reports a duplicate, a pre-unit intent still recovers, and a refused
  start leaves nothing behind. A warded start with no host still starts:
  the ward runs only on the daemon's host.
- **A start refused for its host holds only itself,** over leaving it a
  bare `ErrUnsupportedStart`. The refusal also wraps
  `exec.ErrStartHostUnsupported`, which the engine classifies as a
  `backend_not_conformant` hold on that invocation. An unclassified start
  error ends the dispatch pass, so one stranded attempt would hold back
  every intent queued behind it, with nothing but a log to say why.
- **A full host's refusal still counts the launches ahead,** as the memory
  gate does, over reporting 0. Otherwise the fact would claim nothing waits
  ahead of a launch that is behind older ones.
- **The host column has no foreign key,** over `REFERENCES
  execution_hosts(id)`. SQLite refuses `ADD COLUMN` with a foreign key and a
  non-null default, and the column is derived from the body, which the store
  reads as the authority.
- **The hold's fact is optional beside its reason,** over required. A hold
  recorded before this unit has no fact and still decodes. The fact is Tier
  B: validated on write and on read, with no tamper tests.

## Refute-First Findings

- **Confirmed and accepted: an unstarted pre-unit cordoned admission in
  attended mode is refused after the update.** An admission recorded by the
  old build and never started has no host, and the driver refuses it. In
  unattended mode the cordon's configuration digest covers the daemon binary,
  so the update makes `readmitStaleAdmission` supersede it with an admission
  that names the host. Attended mode skips that check, so the attempt holds
  under `backend_not_conformant` until the operator stops the task. The
  window is a crash between recording and starting, followed by an update;
  a capped launch is never recorded, so it isn't in it. The issue requires
  the refusal, so it stays.
- **Confirmed and fixed: a bare refusal would stall dispatch.** The
  fresh-context review traced `ErrUnsupportedStart` to no hold class, so
  the pass ended at the stranded attempt every time. The host sentinel and
  its hold row fix it (`TestHoldClassificationGolden`).
- **Confirmed and fixed: a full host reported no launches ahead.** The cap
  returned before counting them (`TestCordonedLaunchCapGrantsInTaskSubmissionOrder`).
- **Disproved by a test: a replay of a refused-host start reports the
  refusal instead of a duplicate.** The host check runs after the intent
  lookup (`TestCordonedStartRefusals`).
- **Disproved by a test: stored admissions change id.**
  `TestAdmissionExecutionHostEncoding` decodes a stored version 8 body, and
  every existing admission golden is unchanged.
- **Disproved by a test: a supersede leaves a stale host column.**
  `TestExecutionAdmissionHostColumn` supersedes in both directions.

Revisit when admission places a launch on a host other than the daemon's
(#2166) or a remote host gets a stage gate (#3625): both replace the
daemon-only refusal.
