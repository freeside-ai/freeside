# Ward Handoff Helper Roles on the Runner

Issue #2867. Tier A by path (`daemon/internal/ward/`). Second link of the
chain `devlog/2026-11-07-1956-ward-helper-role-runner.md` started.

## Decision

**Moved the handoff's remaining helper roles onto `runHelperRole`.** The
workspace seeder and base observer (`seed.go`), the writer-outcome observer
(`writer_outcome.go`), and the credential observers behind
`observeCredentialProof` (`handoff.go`) now run through the runner. Their
hand-written create-to-absence sequences are gone. The four functions keep
their signatures, so recovery, the credential volume readers, and the Codex
review workspace need no change. The runner needed no new step.

Three roles needed a choice the runner doesn't make for them:

- **Base comparison after the runner, not in `checkProof`.** The
  declared-versus-observed base comparison still runs after the observer is
  deleted and proven absent. Moving it into `checkProof` would have been
  simpler, but a mismatch would then leave the observer for teardown and
  change the claim state on that path.
- **Writer wrapping keyed on `proofChecked`.** `writerObservationError` wraps
  a failure with `ErrWriterFailed` only after `checkProof` accepted the
  status, because only the delete and absence proof can fail after that.
  Wrapping every runner error was rejected: a failed journal write and a
  failed evidence capture already carry `ErrWriterFailed`, so they would
  join it twice.
- **Credential label carries the image.** The label is
  `credential observer container from image <image>`, so the create reason
  keeps the text it always had. The cost is that the inspect, start,
  stopped-wait, and delete reasons now name the image too. The image is
  digest-pinned configuration, not a secret. A per-step label in the runner
  was rejected as the per-role drift the runner exists to remove.

**Only reason wording changed.** Inspect, stopped-wait, and delete reasons
now use the role label (`inspect seeder container before execution`,
`delete stopped writer-outcome observer`). No test or other package matches
the old strings.

## Evidence

- **Golden:** `TestHelperRoleSequence` gained 25 failure rows (the five
  roles at create, inspect, allowlist, start, and the stopped wait). They
  were committed against the old code first, and no row changed through the
  move. The successful-handoff call list is byte-identical.
- **Writer wrapping:** `TestObserveWriterOutcomeWrapsCleanupFailures` pins
  the delete and absence failures at status 0 and 7, and the failed journal
  write. It passes on the old and the new code.

## Refute-First Record

The old-versus-new comparison was a session-only test. It drove each moved
function directly with a failure injected at create, pre-start inspect,
allowlist, fingerprint, start, stopped wait, memory-limit kill, proof read,
delete, and absence proof. It also covered the base mismatch, writer
statuses 0 and 7, the failed journal write, and the gate and token-read
credential observers: 88 cases. For each case it recorded the claim's
`attempted`, `owned`, and `fingerprint`, the error's check, `ErrWriterFailed`
and how many times the text names it, an unwrapped `*MemoryLimitError`, the
content-evidence mark, the writer's journal flags and failure status, and
whether the credential proof and token were zero. Old and new differed only
in reason wording. A fresh-context reviewer also tried to prove the change
wrong.

- **A claim left in a different state on failure: disproved.** The
  comparison matched at every step, and the reviewer traced each role's slot
  through the runner.
- **`*MemoryLimitError` wrapped: disproved.** It returns before the proof
  read, so `proofChecked` is false and the writer returns it unwrapped.
- **`ErrWriterFailed` joined twice or dropped: disproved.** The comparison and
  the new test cover the delete, absence, and journal paths.
- **A token returned with an error: disproved.** Every runner error goes
  through `fail`, which returns zero values. The closure's captured token is
  dropped.
- **Base comparison moved before the delete: disproved.** It runs only after
  `runHelperRole` returns nil.
- **Size, name, or context drift: disproved.** All four spec builders set
  `Name` to the old name and `Size` to `hs.Size`. `checkProof` uses the same
  context the old code did.
