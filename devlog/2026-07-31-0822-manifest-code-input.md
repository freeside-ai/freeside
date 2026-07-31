# Keep Manifest Conversion Codes Out of Process Arguments

Work unit: #412. Scope: `daemon/` and `devlog/`.

## Decision

Chose an explicit `-registration-code-stdin` mode over a code-valued flag or
environment variable because the one-time manifest code is credential-equivalent
until GitHub consumes it. Standard input keeps the value out of both the
`freesided` process argument vector and copy-paste command history, while a
protected file or secret-manager descriptor gives non-interactive automation a
direct input channel. Reader failures are intentionally reported without their
underlying error because a custom reader can include recently read secret bytes
in that error. The in-memory value uses the publish package's redacting secret
type and is revealed only at the existing registrar call.

Rejected adding the registrar's browser callback listener to packaged setup.
The Phase 1A operational-command decision deliberately kept callback transport
out of scope and made replay the authority-independent recovery path. Adding a
listener here would widen the command's network and lifecycle contract when a
bounded one-line standard-input seam solves the leak directly.

## Recovery Boundary

The conversion code remains transient. Setup reads at most 4096 bytes, accepts
one line, and passes it to the existing redacted registrar exchange without
writing it to JSON or durable state. The registrar still atomically stores the
converted key with its pending-authority marker before authority initialization.
If that later step is interrupted, the next setup run resumes from the exact
marked registration without reading or retaining the already-consumed code.

## Verification

The regression boundary exercises a real child process blocked on standard
input, inspects its live process command with `ps`, completes the conversion,
and checks the inert marker is absent from the process listing, standard output,
standard error, returned errors, and JSON. The existing interruption test now
delivers the code through standard input and proves recovery performs only one
conversion before finalizing the pending authority.

The refute-first pass confirmed and fixed two leak variants. Go's boolean flag
parser rendered the supplied value when the stdin flag used joined
`-flag=value` syntax, so setup now rejects every joined form with a static error
before flag parsing. An inherited `set -x` also rendered the expanded shell
variable in the interactive recipe, so the documented sequence runs inside a
tracing-disabled subshell. The same pass rejected leaks through correct stdin
usage, successful and interrupted output, reader errors, JSON, or durable files.

Accepted boundary: the code exists transiently in process memory and the stdin
stream, and GitHub necessarily receives it at the conversion endpoint. A
producer that keeps stdin open after writing also keeps setup waiting for EOF.
Those are not argv, environment, history, logging, or persistence channels.

Revisit when setup gains a first-class callback transport or must accept a
manifest-code format larger than GitHub's current bounded one-line response.
