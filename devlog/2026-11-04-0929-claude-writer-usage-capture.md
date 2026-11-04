# Claude Writer Usage Capture: A Driver Under The Drop, Nil On Doubt

Decision scope: #2543. Adopts the `turn_seeded` launch the spike chose,
[`2026-11-03-1457-claude-usage-share-spike.md`](2026-11-03-1457-claude-usage-share-spike.md).
Tier A: it changes the writer's launch command, beside the token injection.

## Decisions

- **A new delivery member, `file_v6`, over editing `file_v5`.** A
  `stage.PromptDelivery` member names a whole launch command: recovery
  rebuilds a running launch's command from its stored member, and ward
  refuses a rebuilt spec whose digest differs. Editing `file_v5` would strand
  every intent recorded under it, so `argument` through `file_v5` stay
  byte-identical (their goldens did not change) and new launches select
  `file_v6`.
- **The driver runs under the privilege drop, not as root.** The duplex
  protocol needs a process that waits for the turn's `result` before it
  writes `get_usage`, which plain shell cannot do. Chose a Node driver
  started by `setpriv` in front of the CLI over a root-side reader, so no
  root process parses agent output. The driver holds nothing the CLI does
  not: the root shell still opens the prompt file and the transcript, the
  driver's stdout is the CLI's byte for byte, and its exit status is the
  CLI's (128 plus the number for a signal, as the shell reports one).
  Every wait is bounded: a CLI that never answers `initialize` gets EOF
  after two minutes, an unanswered `get_usage` after fifteen seconds, and
  once the CLI exits the driver waits at most two seconds for a descendant
  still holding its stdout.
- **The script lives in a fresh root-owned directory,
  `/var/lib/freeside-driver` (0755, file 0444).** Rejected the launch
  directory, which is 0700 and must stay closed to the agent, and the
  workspace, which the agent can write. Rejected shipping it in the image,
  a non-goal here (no image or pin change). The root shell writes it with
  `printf '%s'` from the embedded `usage_driver.cjs` on every launch, so the
  command's digest covers the script's bytes and recovery rebuilds the same
  one.
- **The specification stage keeps `file_v5`'s command.** The specifier's
  decision arrives in the result's `structured_output` under
  `--json-schema`; a duplex launch is untested there and the usage answer
  is wanted only from writer turns. `file_v6` on a specification stage is
  `file_v5` exactly, which a test pins.
- **Conformance launches move to `file_v6` with production.**
  `ConformanceHandoff` builds its launch from `launchDelivery`, and a
  conformance pass is meant to prove the launch production runs. Its judge
  ignores the `control_response` lines, which a test pins on a duplex
  transcript.
- **Nil on doubt.** The transcript is agent-side output, so the capture is
  untrusted input. Any unreadable line, a duplicate key, a malformed or
  out-of-range answer or event, or a missing `result` gives a nil capture,
  never a stage failure. A refused, null, or unavailable answer keeps the
  turn's rate-limit events when it has any. Events keep the last one per
  window, sorted by window; more than sixteen windows or events, or a reset
  outside years 0 through 9999 (which JSON cannot encode), is doubt. The capture is validated where
  it is parsed and again by `StageResult.Validate` when a stored intent is
  decoded. It grants nothing: no reader in this unit acts on it.
- **The capture rides on every result that has a transcript.** Completed,
  blocked, failed, and rejected results all carry it, because the turn spent
  usage either way. The crash-recovery paths (pending usage, deferred
  abandonment, outcome recovery) carry none; that is "nothing captured", the
  same answer a missing `get_usage` gives.

## The 2.1.282 Gap And What Closed It

The spike's evidence was from Claude 2.1.220; the production image pins
2.1.282. Before the live turn, the pinned 2.1.282 package (the npm package
the image installs) ran the `file_v5` and `file_v6` agent lines on the host
against a loopback mock API that sent both windows' rate-limit headers, with
synthetic credentials only:

- **Same outcome.** `file_v6` exits 0 on a successful turn and 1 on a refused
  one, as `file_v5` does. Stage status depends on that exit status, so this
  was the risk that mattered.
- **No extra request.** Each launch made one `POST /v1/messages` carrying the
  prompt once, and `file_v6` tried no route `file_v5` did not
  (`HEAD /api/hello`, both).
- **The answer.** Under an OAuth token with the scope override, `get_usage`
  answered `rate_limits` with `five_hour` and `seven_day` shares from the
  turn's headers. Under an API key it answered `rate_limits_available:
  false`. A refused turn answered `rate_limits: null`. 2.1.282 adds `session`
  and `behaviors` to the answer and `unifiedWindows` to the event; the parser
  ignores them. Two of these transcripts are fixtures.

The real provider's answer on 2.1.282 is what the opt-in
`TestLiveClaudeWriterUsage` checks, with the owner's consent; its result is
recorded in the PR.

## Refute-First Review

A fresh-context reviewer tried to refute the change before push. It could
not refute these: driver tamper (the agent cannot write the script or its
directory), shell quoting of the script and the prompt, any token reaching
argv or the transcript, `NODE_OPTIONS` or other environment reaching the
driver, the argument through `file_v5` commands, exit-status fidelity,
parser panics on hostile lines, and readers that ignore the control lines.

Its findings and what became of them:

- **Blocking: a reset outside years 0 to 9999 passed `Validate` but failed
  `json.Marshal`,** so an agent-shaped answer could break persisting the
  stage result. Fixed: `Validate` bounds every reset, with tests at both
  edges.
- **The driver waited forever for an `initialize` answer.** Fixed with the
  two-minute init timeout.
- **The driver waited for stdout to close, which a lingering descendant
  holds open.** Fixed with the exit grace.
- **Events were unbounded.** Fixed: last per window and a cap of sixteen.
- **Two comments overstated the behavior** (the pipeline's missing-capture
  note and the parser's docstring). Fixed.

Residual risks, accepted:

- **A CLI that ends a turn without a `result` and stays alive** keeps the
  driver waiting until the stage deadline, as `file_v5` would wait on that
  CLI. The stage deadline still ends it, and no capture is the answer.
- **The agent runs as the CLI's UID, so it can open the CLI's
  `/proc/<pid>/fd/0` and `fd/1`** to inject stdin or forge stdout. That
  grants nothing new: the same agent can already read the token from the
  CLI's `/proc/<pid>/environ`, and a forged capture is untrusted, validated,
  and acted on by no reader in this unit.

## Departures

- **"No refresh" is checked by the credential volume's tree digest, not a
  request count.** The spike counted refresh attempts through a TLS
  observer that lives in `ward`'s tests and cannot be imported. A refresh,
  or any other auth-store write, changes the volume's tree, which
  `ObserveSetupTokenIntegrity` digests before and after the turn; a setup
  token carries no refresh token to use.

Revisit when the agent image's CLI pin moves: rerun the loopback check and
`TestLiveClaudeWriterUsage` on the new build, since the answer's shape and
the duplex exit status are the CLI's, not ours.
