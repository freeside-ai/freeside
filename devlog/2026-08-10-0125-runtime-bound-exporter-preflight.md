# Runtime Bound and Exporter Preflight (#368, #523)

## Decision

Chose one portable Bash watchdog for every Apple `container` call in
`scripts/check-agent-image.sh` over `timeout(1)`, `gtimeout`, or separate fast
and fallback paths. macOS does not ship either timeout executable, while one
background-command/watchdog implementation keeps the production and regression
paths identical. The watchdog arbitrates timeout versus completion through an
atomic state-file claim in a private temporary directory, polling at 100 ms;
that avoids both a platform-specific timeout executable and a cancellable timer
process that can outlive the call. A claimed timeout kills the watched process
group immediately rather than adding a termination grace beyond the advertised
hard wall-clock bound. Each runtime call and its descriptor relays
run in one process group, so a timeout terminates ordinary descendants and
closes checker-facing descriptors even if a writer moves to another group.
Normal completion waits for the runtime and both descriptor relays, not
arbitrary group liveness. The 120-second default is operator-
overridable from 1 to 86,400 seconds so the suite can prove hung-call refusal
in one second without unsafe arithmetic. Timeout diagnostics use a checker-
owned file descriptor so runtime stderr suppression cannot hide the exceeded
bound.

Kept the preflight in `scripts/run-real-work.sh` operator-attended and
unbounded. The probe may need to pull a digest-pinned image and the script
already expects an operator to interrupt abnormal waits; importing the
checker's unattended timeout policy here would conflate two different runtime
contracts. Chose a networkless execution probe over an OCI capability label
because it measures the actual pinned filesystem. The exhaustive external-
command list is duplicated with cross-pointers to `observerScript`,
`observerGitScript`, and `credObserverScript` rather than mechanically coupling
the scripts component to Go source parsing. Its combined output is
captured through a 64 KiB-plus-one file cap before any shell variable sees it,
matching the live ward probe's memory bound and preserving the runtime and sink
statuses separately.

The exporter probe accepts only its fixed missing-tool markers as evidence of
an image-content gap. A command-not-found exit without those markers is a
runtime launch failure, not permission to claim which tool the image lacks.

## Destructive-Path Verification

The runtime wrapper changes only how long an already-gated create, inspect,
list, or delete call may wait. Refute-first verification established these
final invariants:

- **Deletion authority is unchanged.** Foreign tokens, identity mismatches,
  ambiguous JSON, ownership revocation, and uninspectable recovery candidates
  still refuse force-delete. A timed-out delete fails closed and does not
  bypass any ownership predicate.
- **The configured limit is a hard wall-clock bound.** Atomic state polling has
  no timer child that can retain checker descriptors, and a claimed timeout
  kills the watched process group without a post-deadline grace. The hang and
  TERM-ignoring fixtures cover both main-flow inspection and cleanup deletion.
- **Owned I/O, not arbitrary daemonized work, defines completion.** The runtime,
  descriptor relays, and capped sink share the watched group, so inherited and
  detached writers cannot hold checker-facing descriptors past timeout. Normal
  completion waits for the runtime and both relays without polling group
  liveness; a same-group descendant that closes those descriptors may survive.
- **Operator interruption remains distinct from timeout.** HUP, INT, and TERM
  claim an interrupted state, forward TERM and KILL to the runtime group, clean
  watchdog state independently of leader liveness, and exit 130.
- **Delayed reaping cannot extend or falsify the bound.** Runtime completion
  does not poll `kill -0` over a group that may contain zombies. Regression
  assertions accept an observed zombie or a concurrently reaped PID as stopped,
  while a live or unobservable extant helper still fails.
- **Runtime output remains bounded and byte-preserving until validation.** JSON
  capture is capped at daemon parity. Exporter output is capped before shell
  buffering, and raw NUL bytes are rejected before marker extraction rather
  than being discarded by command substitution.
- **Exporter attribution is closed over a fixed contract.** Missing-tool
  evidence requires exit 127 and unique markers drawn from all 17 external
  executables used by the base, Git, and credential observers. Unknown markers,
  known markers with another status, and oversized or NUL-bearing output remain
  generic runtime failures.

Revisit when Apple ships a supported native timeout primitive, when
an observer's external-command set changes, or when a second image-drift class
justifies a daemon-owned capability contract under #532.
