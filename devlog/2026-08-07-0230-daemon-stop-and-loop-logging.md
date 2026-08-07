# Daemon Stop Semantics and Loop Logging (#544 + #543)

Combined work unit: one branch, one PR, closing both issues. The owner's
implementation plan (identical comment on both issues) argued the
combination, since both rewire the same composition surface in
`cmd/freesided/main.go` and complete each other: #544's new failure modes
are only operator-visible through #543's records, and #543's loggers land
on the loops #544 makes stoppable.

## Decisions

**Chose a `procbound` helper over a fourth and fifth hand-rolled copy of
the pattern.** `internal/verify/procroom.go` worked out WaitDelay +
Setpgid + process-group-SIGKILL-on-cancel + post-wait reap under a
refute-first pass; `internal/verify/gitrunner.go` carries a partial second
copy. Nine more sites needed it, which is past the project's ~3-use bar.
The helper is internal plumbing, not a shared contract surface (owner's
plan); both issues declare no affected contract.

**Left `internal/verify`'s two sites alone**, per the owner's explicit
plan decision, even though `gitrunner.go` is only partially bounded (it
sets WaitDelay but no process group, so a lingering descendant is
unblocked-from but never reaped). Converging them belongs with #566, which
consolidates those same runners. The enumeration ratchet names both as
exemptions with that reason, so the gap is recorded where the next reader
of that code will see it rather than only here.

**Chose a source-walking ratchet test over a one-time grep.** The plan
called for a grep sweep to confirm the enumeration was complete. A grep
does not survive the session that runs it, and the defect is invisible on
inspection: assigning a `bytes.Buffer` to `Stdout` is the ordinary way to
capture output and carries no hint that `Wait` now blocks on a pipe. The
test asserts every function building an `exec.Cmd` also names the helper,
with three exemptions that each carry a reason and fail when stale. Its
limit is stated at the test: it proves a function *names* Bind or Run, not
that it used them correctly, so it catches a forgotten site and not a
misused helper.

**Chose "failed step" over "room error" for a ward verification command
whose pipes outlive its process.** `exec.ErrWaitDelay` maps to
`ExitCode: -1`, matching `internal/verify/procroom.go`'s classification
and its stated reason: a container that exited cleanly while something
still holds the output pipe is not a clean verification, and must not read
as passed. Both consumers fail closed on it — `Run` returns the failed
step, `ReadRecipe` rejects the non-zero exit before the digest check, so a
truncated recipe read cannot reach authentication.

**Kept the credential-bearing session teardown unbounded, reversing a
finite budget this unit first implemented.** The first implementation gave
`daemon.Close` a per-phase budget derived from ward's `TeardownTimeout`,
which is what #544's acceptance list asks for. That is wrong against the
architecture: plan §5.2 (revision 27, decider: user) closes the stop-wait
fork on the unlimited side, and says so in terms that name this exact
change — "any finite grace recreates SIGKILL-mid-lease, and a bounded
credential-safe teardown is deferred hardening, not a tunable" — with the
supervisor's exit timeout effectively unlimited to match.

**#544's acceptance list therefore conflicts with plan §5.2, and the plan
governs.** An issue's acceptance criteria are not a gated plan revision, so
the criterion "`daemon.Close` runs under a finite budget derived from
ward's `TeardownTimeout`" cannot be satisfied as written. Surfaced on the
issue and in the PR for the owner to arbitrate; the alternative resolution
(revise §5.2 to permit a bounded credential-safe teardown) is a material
plan change and its own gated unit, not something to fold in here.

What #544's objective actually needs, and what this unit delivers instead:
the teardown no longer hangs *by accident*, because every subprocess site
it runs through is individually bounded, and a stop is no longer
inescapable, because restoring signal disposition before teardown means a
second SIGTERM ends the process. The distinction §5.2 draws is preserved:
nothing abandons a lease on a timer, and abandoning one is a deliberate
human act.

Credit: the automated reviewer caught this, citing §5.2 against the diff. I
had not read §5.2 before implementing the budget, and the finding is a
straight hit.

The HTTP-shutdown phase keeps its pre-existing five-second bound. It holds
no lease: abandoning a slow in-flight request loses nothing durable.

**Chose text `key=value` over JSON** for the handler, taking the plan's
offered default rather than its veto: the LaunchAgent captures stderr
straight to a file the operator greps, and a line they can read beats one
that needs a tool.

## Verification Findings

**Rejected by verification, so it is not re-raised:** `gitnet.go`'s
`run` reads `cmd.ProcessState.ExitCode()` without the `!= nil` guard its
two sibling functions carry, which looks like a nil dereference when
`Start` fails (a context canceled before start, on exactly this PR's
shutdown path). It is not: `(*os.ProcessState).ExitCode` guards its own
nil receiver and returns -1. Confirmed by direct probe, not by reading.
The sibling guards are redundancy, not a fix for a live defect.

**Confirmed by review, and the reason the helper is not a pure
extraction:** returning a raw `ESRCH` from the cancellation callback
misclassifies a *successful* command as a failed one. `os/exec` treats only
`os.ErrProcessDone` as a benign `Cancel` result and wraps anything else as
`exec: canceling Cmd: ...`, which `Wait` returns; when a shutdown races a
command's own exit the group is already gone and the group kill returns
`ESRCH`. `Bind` maps it to `os.ErrProcessDone`. The code this package was
extracted from carries the same defect
(`internal/verify/procroom.go:107-112`), where it surfaces as the room
claiming it could not execute a step that ran to completion; filed as #584
rather than fixed in passing, since `verify` is outside this unit's
declared scope.

**Confirmed:** Apple container's helper processes (`container-apiserver`,
`container-runtime-linux`, the plugin binaries) all run with PPID 1 under
launchd, never as children of a CLI invocation. Putting the CLI in its own
process group therefore cannot reach a running container, which was the
plan's stated risk for `runtime_cli.go`.

**Confirmed, with its limit stated:** no credential reaches a log record.
`publish.Secret` redacts through `Format` (every verb), `String`,
`GoString`, and `MarshalText`, and the new test feeds one through the
three shapes slog renders by different paths: inside an error, as an
attribute value, and as a struct field. Separately, every non-test
`Reveal()` call site is an intended consumer — an `Authorization` header,
a PEM decode, a basic-auth encoding, a keystore write, or an emptiness
check — and not one feeds a formatting call, an error, or a logger. The
limit: this is a point-in-time audit of call sites, not a mechanical
boundary. A future `Reveal()` into an error would not fail any test. The
`Secret` type is the durable control; the audit only shows nothing
currently bypasses it.

**The refute pass on the credential surface was self-run, not
independent.** This session ran without delegated reviewers, so the
adversarial lens the high-assurance profile asks for on a credential-leak
surface came from the same context that wrote the code. The automated PR
reviewer is the first genuinely independent pass.

## Scope Taken Beyond the Issues

The enumeration is nine sites, not the issue's five: `publish/gitnet.go`
has three transport sites rather than one (two of which drive their own
pipes through `StdoutPipe`, where `WaitDelay`'s timer has not started when
the read blocks, so only the group kill unblocks them), and a sweep found
`readTailscaleIPs` in `cmd/freesided` reaching the same pipes through
`cmd.Output`.

One defect was found by the new tests rather than named by either issue:
the active-resource loop returned its store's "context canceled" from a
pass interrupted by SIGTERM, and `main` hands that to the channel `Wait`
treats as fatal. A normal stop therefore had a coin-flip chance of exiting
non-zero and reading to a supervisor as a crash. Fixed to match the engine
and scheduler loops, which already read cancellation as shutdown. It is in
#544's subject (the daemon cannot be reliably stopped) though not in its
acceptance list.

## Known Deviation From the Plan

Active-resource records carry `subsystem` and the error text, not separate
`repo`/`number` keys. The loop's failure seam is a `[]error`, so item
identity reaches the boundary only inside the wrapped message (which does
name the item). Promoting it to structured keys needs a typed failure
value, which would touch that package's large test surface for a
presentation gain; not taken here.

Revisit when: #566 consolidates the git plumbing runners, which is the
moment to converge `internal/verify`'s two hand-rolled copies onto
`procbound` and drop two of the ratchet's three exemptions.
