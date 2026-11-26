# Run a Recipe Command Under the Cordon With No Token

Issue #3626. Tier A (runner containment: `internal/cordon/`). It gives
plan revision 137's cordoned self-check (#3627) a room: one recipe command
run as the host's run account under every writer denial, with no
credential and no agent, its end proven as a writer's is.

## Decision

`internal/cordon` gains a third launch kind beside the writer and the
conformance probe. `Backend.Check` runs one command's argv in a fresh
worktree seeded at the named commit, under the writer's rule file, with the
probe's launch environment and stdin on `/dev/null`. `RecoverCheck` finds
how one ended after a restart. The supervisor request records the kind,
and the kind decides what the program gets and whether the run exports.

## Why

- **The kind lives in the request record, not in booleans or the end
  record.** Chose a `kind` field in `ctl/request.json` (`writer`, `check`,
  `probe`) over the old `prompt` and `secret` booleans, because a boolean
  pair says what the program gets but not what the run is, and export
  needs the second. The supervisor derives the prompt, the token pipe, and
  the output routing from the kind with one dispatching switch, so a new
  kind can't compile without a decision on each. The request is in `ctl/`,
  which no launch can reach, so it is the daemon's own word.
- **Export refuses by kind.** Chose `kind != writer` over the old
  empty-`BaseSHA` test, which a check would pass: a check carries a base
  commit because it is seeded from one. A missing request, or one that
  reads but isn't this run's, also refuses with `ErrNotExportable`, so a
  caller treats it as permanent rather than retrying; a failed read
  doesn't, so a retry can still export a clean run.
- **No older reader.** The record is private to the package and its
  supervisor, the same binary, and an update drains every run first. A v1
  or v2 request a forced update leaves fails closed: `Export` refuses it
  and the run needs an operator. Accepting an older request as a writer's
  would be a small reader change if that proves too strict.
- **No token by construction, checked twice.** `CheckRequest` has no token
  field, and the supervisor refuses a check request while it holds a pipe
  on descriptor 3, before it claims the run, so it starts nothing. A macOS
  test shows a real check's command sees no pipe on descriptor 3 and no
  `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`.
- **Output is one stream with a tail bound.** A check's stdout and stderr
  both go to `ctl/transcript`, matching `verify.StepResult.Output`'s one
  stream; `ctl/stderr` stays, empty, so the run's layout doesn't depend on
  its kind. The result carries the last `MaxOutputBytes`, since a failing
  test run's useful lines are at its end.
- **Recovery shares the writer's loop.** `Recover`'s wait, claim, and
  end-lost loop moved into one helper that both `Recover` and
  `RecoverCheck` call, so the four interrupted states have one
  implementation. `RecoverCheck` never exports and refuses a run whose
  request is another kind's.
- **No evidence files.** The recipe's only capture mode is `none`, and
  `verify.StepResult` has no field for a file, so a check returns its
  output alone; the first capture mode adds files.
- **The end-to-end probe is a test, not a step of `Suite.Full`.** Full has
  no repository to seed a check from, and a check's rule file is
  byte-identical to a writer's (tested), so the writer denials Full proves
  hold for a check. A macOS test runs a set of them from inside a real
  check.

## Behind the Host Launcher

#3688 (#3623) merged first and put cordoned launches behind a host
launcher, a LaunchAgent the daemon reaches over a Unix socket. The
landing batch handed this PR back, and the owner chose to rework it here
rather than split the protocol half into its own unit. Three decisions
follow, the first two the owner's.

- **The protocol carries a check.** `internal/cordonhost` gains a
  `check` frame and a `recover_check` frame, served on the launcher's
  backend, and its client gains `Check` and `RecoverCheck`. A check
  crosses as a launch does without the token: its request, then its seed
  as a tar, and it keeps running when its client leaves, ending only on a
  cancel frame. Its end carries the tail of the output in its one frame,
  so the bound is capped at `MaxCheckOutputBytes` (the failure
  transcript's 4 MiB) on both sides; the launcher refuses a larger bound
  before it reads the seed. Rejected: returning the output as a file the
  client fetches, which needs a second operation and a scratch copy for a
  tail that fits one frame. `stage.CordonGate` and the daemon's
  `cordonLauncher` don't gain `Check`, because nothing calls it yet;
  #3627 adds it there with its consumer. The two-seam contract suite's
  `seam` interface does carry both, so neither seam can drop one.
- **A check takes a writer's approval refusal.** `Check` calls
  `approvalRefusal` before its pre-job probe, whatever `RequireProof`
  says, as `Launch` does: macOS charges screen-recording and accessibility
  to the program that starts runs, and a check's command is such a
  process under the same rules, so the approval would reach it.
- **A check seeds from its request's seeder.** `CheckRequest.Seeder`
  mirrors `LaunchRequest.Seeder` with `Launch`'s rule exactly: a non-nil
  request seeder replaces the backend's, and the backend's own stays the
  in-process fallback, which `New` already requires. The launcher sets it
  to the staged tree, so its `NoForgeSeeder` is never asked.

## Rebased Onto the Streamed Writer and the Host Export

The landing batch handed this PR back a second time: #3704 had moved the
supervisor request to v2 with `prompt` and `secret` flags (a writer's
stdin streamed from `ctl/prompt` and the inbox), and #3710 returned a
clean run's export over the launcher's protocol. The owner asked for the
PR to be made mergeable; these choices follow.

- **The kind replaces the flags.** Every launch on `main` set `prompt`
  and `secret` together, true for a writer and false for a probe, so they
  were the kind spelled as two bits. The kind keeps the same one
  dispatching switch: a writer gets #3704's streamed input and its
  turn-end watch, and the token's pipe; a check gets `/dev/null` and one
  output stream; a probe gets neither. Rejected: keeping the flags beside
  the kind, which leaves combinations no kind means for the supervisor to
  refuse.
- **The request moves to v3.** v2 now names two shapes, `main`'s flags
  and this PR's kind, so a request's version alone couldn't say which. A
  v2 request of either shape fails `check` and, at `Export`, refuses, as
  a v1 one does.
- **A check stays out of the export stream.** The `check` and
  `recover_check` frames sit beside #3710's `export` frame; a check reply
  never carries an export, and the client's check-end test refuses one
  that does. #3710 retired `authenticate_released_export`, so the check
  frames don't list it.

## Refute-First Findings

Recorded per `docs/agent-workflow.md` §refute-first.

- **Confirmed and fixed: a check's argv could set the sandbox's
  parameters.** `confinedArgv` put the program right after the `-D`
  parameters, and sandbox-exec reads options up to the first non-option,
  a later `-D` overriding an earlier one. A recipe argv starting
  `-D HOME=/nonexistent` read the owner's secret stand-in, and one setting
  `LAUNCH_TAG` returned an exited end while the command's detached children
  lived. Recipe validation accepts a leading `-`. `confinedArgv` now puts
  `--` before the program, for every kind, as `internal/macverify`'s
  launcher already does; `TestHostCheckCannotSetTheSandboxsParameters`
  fails without it.
- **Confirmed and fixed: two pipes don't make one ordered stream.** A
  check's stdout and stderr were copied by two goroutines, so a real
  command's alternating lines came back out of order and the tail could
  drop the last error. A check's stderr now shares stdout's pipe;
  `TestHostCheckKeepsItsStreamsInOrder` fails without it.
- **Disproved: a check or probe run can export.** Mutating `Export`'s kind
  test to always pass makes `TestOnlyAWritersRunExports` and the probe
  launch's export check fail. A v1 or unreadable request also refuses.
- **Disproved: a token reaches a check.** Check passes no secret, the
  supervisor refuses a non-writer holding a pipe on descriptor 3 before it
  claims the run, and a real check sees no pipe there and no token
  variable. Each refusal direction has a test that fails when it is
  removed.
- **Disproved: a check reaches a credential or an export through the
  protocol.** The `check` frame has no token field and strict decoding
  refuses an unknown one; the server builds a `CheckRequest`, which has
  none, and the supervisor still refuses a non-writer holding descriptor
  3. `RecoverCheck` runs no export, and an `Export`, served or local, or
  a writer's `Recover` on a check's run refuses at `Export`'s kind gate,
  which the launcher's backend runs before any byte crosses; the client
  also refuses a check end that carries an export.
- **Disproved: a refusal changes on the wire.** Every error `Check` and
  `RecoverCheck` return is one `encodeError` already codes: the approval
  and probe failures (`*ConformanceFailure`), a detach, a blocked or
  unproven end, `ErrNoRun`, and a failed record; the rest cross as
  internal text and stay errors. The served approval refusal and a
  recovery of no run are tested at both seams.
- **Confirmed and fixed: a failed read refused an export for good.**
  `Export` wrapped every error reading the request in `ErrNotExportable`,
  which `Recover`'s caller reads as a refusal that will repeat, so a
  failed open or read (a descriptor limit, say) would have dropped a
  clean writer's export. Only a missing request, or one that reads but
  isn't this run's, now refuses; the `unreadable request` case of
  `TestExportRefusesALaunchWithoutACleanProvenEnd` fails without it.
  Found by the fresh-context review.
- **Disproved: `RecoverCheck` over the protocol can report an end it
  doesn't have.** A dropped connection or a canceled context returns an
  error, never a result, and leaves the run running; the client refuses
  a reply with no check end, a lost end outside a recovery or without
  its reason, and output past the bound it asked for.
- **Disproved: the client's shared start path changed a launch.** Old
  and new `Client.Launch` ran against the same recording launcher over
  300 generated requests across four reply shapes (clean, failed with a
  transcript, refused, connection closed); the bytes on the wire, the
  results, and the errors were identical.
- **Confirmed and fixed: a test that hung on a missing guard.** Removing
  the launcher's bound check made the raw-frame test wait forever on a
  seed it never sends; the test now has a deadline, so the mutation
  fails it.
- **Disproved: the `Recover` refactor changed writer recovery.** Compared
  against the old function state by state: the same states, lost reasons,
  export-record shortcut, and error passthrough.

## Revisit When

- #3627 finds a recipe command needs its own stall limit; a check uses the
  configured one, so a long, silent test run ends stalled.
- #3627 finds a check needs the verifier's exact tree with no `.git`; a
  check runs in the seeder's checkout.
