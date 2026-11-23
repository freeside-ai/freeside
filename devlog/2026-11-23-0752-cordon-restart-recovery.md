# A Cordoned Launch Runs Under Its Own Supervisor and Recovers From Files

A cordoned launch now outlives the daemon: a supervisor process runs it,
and a restarted daemon adopts the run by its ID with `Recover`, cancels it
with `RequestCancellation`, and removes it with `ReleaseFinished`. Nothing
calls these yet; the stage driver port wraps them. Issue #3468, tracker
#3462. The choices below follow the plan comment on #3468 unless a line
says otherwise; departures are the agent's implementation decisions.

## Chose a Supervisor Over Adopting the Bare Processes

Before this unit the daemon was the parent of the canary and the agent,
and it held the pipes for their output and the prompt. A launch could not
survive a restart, and could not be adopted even if it did:

- **Only a parent learns an exit status.** A restarted daemon isn't the
  agent's parent, and `Export` refuses anything but a recorded clean exit,
  so an adopted run could never export.
- **The output and prompt ran through the daemon.** When the daemon
  exited, the agent's next write failed and the CLI died; a daemon that
  died mid-copy left the agent a truncated prompt.

So the daemon starts `freesided cordon supervise <runs-root> <run-id>`
from its own executable, outside the rule file, in a session of its own
with no environment. The supervisor is the canary's and the agent's
parent, copies their output to files, watches the stall limit, runs the
run-end proof, and writes the end record. The rule file already denies a
launch signaling a non-member and reaching `ctl/`, so the agent can
neither kill its supervisor nor forge its records. The prompt moved from
a pipe to `ctl/prompt`, which the agent reads as stdin.

Rejected:

- **Re-parenting or tracing the agent from the new daemon.** macOS has no
  subreaper, and an exit status read any other way (a wrapper that writes
  it to the worktree) would come from inside the launch, which is
  untrusted.
- **Treating a vanished agent as a clean exit.** It would export a tree
  whose writer may have been killed mid-change.

## Chose One Claim File to Decide Who Owns a Run

The supervisor and recovery both create `ctl/supervisor.json` with
`atomicfile.WriteFileNoReplace`; whoever creates it first owns the run.
A supervisor's claim names its pid and start time; recovery's names
none. A supervisor that finds recovery's claim starts nothing, so a
supervisor the old daemon started just before it died can't run a launch
recovery already ended. Cancellation uses the same ordering:
`RequestCancellation` writes `ctl/cancel.json` before it reads the claim,
and a supervisor reads the marker after it claims, so one of the two
always sees the request. The marker outranks a clean exit wherever an end
record is read, so a canceled run never exports.

A process is signaled or waited on only while its pid has the start time
its claim records, so a pid reused after a reboot or a long outage reads
as a supervisor that is gone.

## Chose `lost` Over Exporting a Run With No Witness

A run whose supervisor died without an end record has no trustworthy
exit status, so recovery runs the proof with a fresh canary under the
run's rule file (it reads as a member as the original did), ends every
survivor, and records `EndedByLost`. A lost run never exports; the caller
re-runs it. A failed proof returns a `BlockedError`, as a launch does.

Departure: a run with no claim, or with recovery's claim, ends lost
without the proof. The plan asked for the proof there too. Every
supervisor claims before it starts anything, so nothing of such a run
ever ran, and a canary started only to prove that would be the one
process the run ever had.

## Chose Files Under the Run Over the Store

Every record lives in the run's `ctl/` directory and is read strictly,
refusing one that names another run. No record names a path: each path
follows from `RunsRoot` and the run ID. Keeping recovery's state beside
the run lets a restarted daemon recover from the run ID alone, the same
input the driver re-supplies from its durable admission, and keeps this
unit out of the store and its migrations. `RunsRoot` is under a protected
root, so a launch can't write these records.

## Other Departures

- **Probe launches run through the supervisor too.** They now leave an
  end record with no base commit, and `Export` refuses such a record, so
  a probe launch still can't be exported.
- **The supervisor's exit status carries why it wrote no end record.**
  The end record, not the status, says how a launch ended; the status
  distinguishes a blocked proof (naming the ending), an unproven host,
  and a plain failure, so `Launch` returns the same errors as before.
- **Canceling `Recover`'s context stops the wait and leaves the run
  running.** Only `RequestCancellation` cancels a run, so a daemon
  shutting down never cancels the runs it is recovering. `Launch` still
  turns its context's cancellation into `RequestCancellation`.

## Risks Not Closed Here

- **launchd's process-group kill is escaped, not tested under the real
  job.** The LaunchAgent doesn't set `AbandonProcessGroup`; a new session
  leaves the daemon's group, and the darwin restart test shows a SIGKILLed
  parent leaves the supervisor running, but no test ran under launchd. If
  launchd ends the supervisor anyway, the fix is in the plist.
- **The supervisor runs from the daemon's path, not a digest-checked
  copy.** An in-place overwrite of the executable could crash a running
  supervisor; the run would end lost and re-run. After an update by
  rename and before the daemon restarts, a new launch's supervisor is the
  new build, which the current conformance record doesn't name. Accepted
  here, since the next daemon start proves the class again under the
  build it runs. The probe's digest-checked copy (`suite.go`) is the fix if
  that changes.
- **Shutdown order is the caller's.** Canceling a `Launch`'s context
  cancels the run for good, so a daemon shutting down must call `Detach`
  first. The driver port wires that order.
- **Version skew.** A supervisor from an older build writes the records a
  newer one reads, so the `v1` formats stay readable; a change needs a
  versioned read.
- **A tree the agent made undeletable** makes `ReleaseFinished` report an
  error and leave the directory.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong before commit.
It confirmed the claim race, the marker ordering, strict decoding, the
pid-and-start-time checks, and release's paths. Its findings, by outcome:

- **Fixed:** a failed proof after the agent failed to start exited as a
  plain failure, not blocked; recovery returned an error when the
  supervisor wrote its end just as recovery found it gone; and a failed
  cancellation request let `Launch` return with the run alive. Each has a
  test that fails without the fix.
- **Documented for the driver port:** release keeps a run with no end
  record until `Recover` gives it one; `Export`'s permanent refusals reach
  `Recover`'s caller unchanged for it to classify; `Detach` comes before
  canceling a launch; and a cancellation after the export leaves the
  export standing.
- **Accepted:** the supervisor runs from the daemon's path, not a
  digest-checked copy (Risks Not Closed Here).
- **Declined:** `Launch` returning a `lost` ending needs `Recover` and
  `Launch` running on one run at once, which no caller does.

Revisit when the stage driver port lands with a shape that needs recovery
to take other inputs, or when a launch must survive a host reboot.
