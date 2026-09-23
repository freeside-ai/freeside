# Daemon Control Routing

Work unit #1510. The private control socket is a same-user authority surface:
the daemon, rather than another `freesided` process, owns the live database and
its blob store. A command attempts the per-database flock before selecting a
path. Holding that lock across a direct open prevents a daemon from starting
between selection and use; failure to obtain it requires the socket or an
explicit maintenance refusal. Opening the file after a socket failure would
defeat the exclusive-locking prerequisite in #1502.

## Chose Database-Scoped Unix Control Over Direct Live Opens

Kept the existing peer-UID check and bound every request to the canonical
database path. The socket address is advertised beside that path, so the
existing `-db` flags identify the daemon without new operator configuration.
This keeps operational control local to the daemon's OS user and outside the
paired-device network API. A same-user process is already inside the local
host authority boundary; the socket does not claim to protect against one.
Canonical paths identify the database, not its sidecars. Doctor borrows the
daemon's existing blob store so a symlinked database does not create another
blob directory under the target path.

The daemon borrows its open store for handlers, while a direct command owns
and closes the store it opened under the lock. Observation transports the
authenticated snapshot fields hidden from the public JSON view in a separate
wire shape, keeping the command's printed snapshot unchanged.

## Preserve Request-Scoped Recipe Authority

The owner approved extending this unit into the store's read boundary after
review showed that borrowing its full recipe policy could authenticate evidence
the snapshot caller had not approved. A transaction-local recipe scope now
intersects the caller's requested recipes with the daemon's approvals before
any record reconstruction. Empty requests fail closed for configured recipes;
the compiled effect-proposal recipe remains approved, as it is on direct opens.
The daemon's immutable policy and other transactions are unchanged.

Checking only the final snapshot would miss evidence consumed by nested store
gates. Reopening the live database with narrower options would defeat the
control-socket design. Regression checks exercise recipe B under requests for
A, B, both, and neither through socket and direct reads, plus attempted policy
expansion and isolation from ordinary daemon reads.

Control requests inherit the caller's cancellation and deadline. A fixed
30-second client timeout would regress Doctor scans whose cost grows with the
checkpoint's artifact set; deterministic tests cover longer completion and
caller cancellation.

## Keep Doctor Health Local To The Request

The owner also approved the narrow store and operations seams needed to keep
Doctor's checkpoint reconstruction under the requested recipe policy. A daemon
approving A and B can otherwise report B evidence healthy for a caller asking
only for A, potentially resolving an artifact-closure blocker. The scoped
evaluator intersects those approvals and retains the compiled effect-proposal
recipe when explicitly requested, matching direct checkpoint scans.

The evaluator reuses the daemon's existing `LocalBackupFiles`, including its
generation lease and live-closure-gap state. Creating another file set would
lose that state; calling the startup constructor would mutate producer policy.
The store instead evaluates an injected source against its coherent live-state
snapshot and validates the returned health before Doctor can converge items.
Ordinary daemon health retains its original source.

The focused refute-first pass reproduced recipe B evidence under requests for
A and B on both socket and direct paths, including a symlinked database. A-only
reconstruction fails closed with the existing provenance error and leaves an
existing closure blocker open; B reports healthy closure. Tests also establish
that malformed source results fail validation, request maps cannot broaden or
mutate daemon policy, the same file set retains its lease, and an incomplete
live closure remains unhealthy even when the prior checkpoint is valid.

## Refute-First Findings

Independent review found that relative database paths would be interpreted in
the daemon's working directory, dangling database symlinks could advertise the
wrong address, flag-shaped user values could be rewritten while forwarding,
and remote error strings would lose the command's `errors.Is` meaning. The
control client now sends a canonical path, canonicalization follows dangling
leaf symlinks like the lock, forwarded command flags preserve flag-shaped
values while keeping the final database selection parseable, and typed error
kinds preserve the command-relevant sentinels. The review found no additional
reachable defect in submission validation, lock release, or resource cleanup.

The control request budget covers the aggregate prepared submission, including
base64 bytes, repeated canonical policy keys, and derived declaration paths.
The prior 20 MiB limit rejected a valid legacy production replay: the same
near-limit input files succeeded offline. The regression proves that difference
against the old transport with a Go source overlay, and proves identical direct
and socket results under the derived 40 MiB request budget. It includes JSON
HTML escaping and a near-limit numeric declaration. New specification creation
has a separate 1 MiB queue-contract limit; the legacy replay is the reachable
case requiring this transport correction. The response limit stays unchanged.

## Revisit When

- Operational control crosses OS users or moves onto a network transport.
- Exclusive database locking in #1502 changes the direct-store fallback or
  calls for a broader offline maintenance protocol.
