# Publish Exec Diagnostics

Chose to prevent concurrent fork inheritance while writing publish test
executables, and expose statusless process failures, rather than retrying
commands or weakening assertions. Work contract: [#1147](https://github.com/freeside-ai/freeside/issues/1147).

## Cause And Evidence Limits

The two historical Linux failures report exit -1, which can mean either a
startup failure or signal death. Their logs omit the wrapped error, so they
cannot establish which occurred. The ETXTBSY mechanism documented in
[Go #22315](https://go.dev/issue/22315) fits both first-execution failures:
a parallel fork inherits an open script write descriptor until it execs.

A Linux scratch probe with Go 1.26.6 reproduced that mechanism in all three
unprotected trials. Holding `syscall.ForkLock` for reading across
`os.WriteFile`, including close, produced no ETXTBSY failures. Go's fork
path takes the write side, so no participating fork can inherit that
descriptor. This demonstrates the mechanism and the protection, not the
cause of the historical failures. Exact trial counts belong in the PR.

The helper covers scripts these publish tests execute. Inert exported
fixtures and hooks the gauntlet proves never run do not need the lock.
Production runners gain no retry behavior. Follow-up:
[#1152](https://github.com/freeside-ai/freeside/issues/1152).

## Diagnostic And Redaction Boundary

`TransportGitError.Error` includes its wrapped cause only when there is no
exit status (`ExitCode == -1`). Ordinary exit messages stay unchanged.
Keeping the old statusless form would hide the distinction the next
failure needs: for example, `signal: killed` versus a `fork/exec` error.
No consumer parses that old form.

An independent refute-first review traced every constructor and its current
production callers before this change was committed:

- **Confirmed:** Error strings can reach durable diagnostics, so exposing
  the cause changes a credential-sensitive boundary.
- **Disproved, token or stderr leakage through `run`:** It is the only
  token-bearing path. The token travels in the child environment; stdout
  and stderr stay in separate buffers. Startup, signal, cancellation, and
  wait errors do not include either stream or the environment. Tests cover
  a missing executable, a signal-killed stub that prints the credential
  header, and an ordinary exit, checking every stub token form.
- **Disproved, arbitrary writer leakage through `runTo`:** Its only
  production writer is `cappedMaterializationBuffer`, which returns a fixed
  limit error. Other causes are pipe, startup, read, or wait failures. This
  path has no token environment.
- **Disproved, arbitrary callback leakage through `interact`:** Its only
  production callback is `readMaterializationBlobs`. It omits raw response
  and blob bytes from errors. Its consumers return fixed validation or
  filesystem diagnostics, sometimes naming a validated tree path. This
  path also has no token environment.
- **Disproved, nil `ProcessState` after a startup error:** Go 1.26.6's
  `(*os.ProcessState).ExitCode` returns `-1` for a nil receiver. The
  missing-executable redaction test already passed on macOS and Linux before
  an added guard, so that guard was removed.

These findings justify rendering the existing causes without a new error
filter. They do not authorize future callbacks to embed remote stream bytes.

Revisit when a transport constructor gains a writer or callback, cause
errors begin retaining command output, or process creation stops
participating in `syscall.ForkLock`.
