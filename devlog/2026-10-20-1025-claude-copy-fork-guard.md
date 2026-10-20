# Guard the Private Claude Executable Across Its Whole Write

Chose `syscall.ForkLock.RLock` across destination open, verified copy, and
close, as specified in [#2031](https://github.com/freeside-ai/freeside/issues/2031).
A concurrent Go fork can otherwise inherit the private executable's write
descriptor. Closing the parent's descriptor does not close the child's,
and Linux can refuse execution with `ETXTBSY` until that child execs.
The production driver and the opt-in native CLI probe share the helper.

## Tradeoff

Chose prevention over a short lock or bounded exec retries. A short lock
leaves part of the writable lifetime exposed; retries can expire while an
inheriting child still holds the descriptor. The existing content pin and
private copy remain necessary to prevent an updater from substituting a
different CLI between verification and execution.

The guard delays every participating daemon fork while staging runs. Copy
I/O is not context-cancelable and has no hard time bound. Keeping the guard
inside the staging helper releases it before any launch or wait, which
would otherwise need the same lock's write side. Go 1.26.9's Unix fork path
takes that write side; the Linux implementation gives waiting readers a
turn. Raw or cgo forks outside that protocol are not covered.

## Evidence

The fixed-count regression reproduced the race through serial `Complete`
calls alongside concurrent Go forks on Linux arm64, kernel 6.18.15, with
Go 1.26.9. Removing only the production guard in a scratch build made the
same test fail repeatedly. A separate scratch diagnostic confirmed
`ETXTBSY` on the private `freeside-judgment-*/claude` executable; the public
error remains unchanged. Repeated guarded runs passed. This is Linux
evidence, not a claim that the race was reproduced on macOS.

## Refute-First Findings

- **Disproved, a descriptor can escape the guard:** The helper acquires
  the lock before exclusive destination creation and closes the destination
  after every returned copy result, including a failed pin. It joins copy
  and close errors, so neither error permits launch. An open failure
  returns through the deferred unlock. Close-error handling was traced,
  not fault-injected.
- **Disproved, the helper recursively locks or launches under the guard:**
  Independent review checked Go 1.26.9's supported Unix open paths, which
  use `O_CLOEXEC` without taking `ForkLock`. Both callers return from the
  helper before constructing and running the command.
- **Disproved, refusal leaks input or retains the slot or scratch:** The
  public call still returns the content-free `errCompletion`, with no
  response and quiescence true before launch. Tests cover missing,
  non-executable, and changed-pin sources, no launch on refusal, empty
  scratch afterwards, and a successful later call. An existing destination
  stays unchanged, and a later launch proves that failed open unlocked.
  Cleanup still precedes slot release.
- **Disproved, extraction changes staging outcomes:** A scratch comparison
  against the original staging block exercised 512 generated cases,
  including valid copies, changed pins, missing and invalid sources,
  existing destinations, and missing destination parents. Refusal
  decisions, destination bytes, existence, and permissions matched.
- **Allowed by the issue's explicit decision, unrelated forks can wait:**
  The full writable lifetime must be covered. This is a production-wide
  latency cost, not a claim that all process creation is now bounded.

Revisit when measured CLI copy latency disrupts daemon operation, the
configured binary moves to slow storage, the Go fork-lock protocol changes,
or a raw/cgo fork path is introduced. A cache or different staging design
would need a separate assessment of executable identity and cleanup.
