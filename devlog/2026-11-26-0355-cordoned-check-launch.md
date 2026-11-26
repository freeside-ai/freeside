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
  commit because it is seeded from one. A request that won't read as this
  run's also refuses with `ErrNotExportable`, so a caller treats it as
  permanent rather than retrying.
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
- **Disproved: the `Recover` refactor changed writer recovery.** Compared
  against the old function state by state: the same states, lost reasons,
  export-record shortcut, and error passthrough.

## Revisit When

- #3627 finds a recipe command needs its own stall limit; a check uses the
  configured one, so a long, silent test run ends stalled.
- #3627 finds a check needs the verifier's exact tree with no `.git`; a
  check runs in the seeder's checkout.
