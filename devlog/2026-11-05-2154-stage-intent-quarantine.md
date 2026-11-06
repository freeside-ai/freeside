# Stage Intent Quarantine: Isolate Restart Recovery Per Intent

Issue #386 makes restart recovery isolate each stage intent. Before it, the
first intent `listIntents` could not read or re-gate, and the first
non-retryable `reconcileIntent` error, ended the pass: every later orphan
lost its recovery attempt, and an export authority conflict or a corrupt
file stopped the daemon from starting. Intent files and journal rows are
never pruned, so the set that has to stay loadable grows for the life of a
state directory. The work is Tier A: it decides what happens when the
reconstruction re-gate refuses an intent and when a released export fails
authentication.

## Decisions

- **Quarantine the intent, not the daemon.** Chose a per-intent quarantine
  over the old refusal to start. Every intent is attempted whatever its
  directory order; only an intent directory that cannot be listed still
  fails the pass. A quarantined intent's file, export directory, and journal
  and export rows stay as found, and each opens one advisory
  `stage_intent_quarantined` item naming the file, the invocation, the class
  (unreadable, unauthenticated, export conflict), and the repair. The error
  goes only to the log, since it can carry decoder text or record content.
  Advisory, not blocking: the fault holds one invocation, and every other
  intent was adopted.
- **Report a quarantined invocation as running; never terminalize it.**
  Chose `Inspect` answering running (not live) over committing a failed or
  lost outcome. For an export conflict a terminal commit would create a
  second authority contradicting the durable export record; for an
  unreadable file there is no trusted record to commit against. Running is
  the vocabulary `Inspect` already uses for a window recovery owns, so the
  `exec.StageDriver` interface is unchanged, and an `Inspect` error would
  abort every engine pass (held-work notices, attention discussions,
  specification gates) on every interval. The cost is deliberate: the run
  stays open and keeps its usage-pool slot until the operator repairs the
  file and restarts, or abandons the task. The quarantine is in memory and
  re-derived each start, so a repair needs only a restart.
- **Inject the held classifier.** Chose a `held func(error) bool` argument
  to `Reconcile` over classifying policy refusals in the stage package. A
  held failure keeps the old behaviour (the intent stays for `Inspect`'s
  retries, no item); the classes that hold it are `ErrRecoveryRetryable`
  and `engine.MutableAdmissionPolicyRefusal`, and the stage package cannot
  import the engine. Holding a refusal that clears when policy is restored
  keeps it from being hidden behind a quarantine's running answer, and
  quarantining everything else keeps a permanent fault from being retried
  with no item.
- **Refuse a stop of a quarantined invocation.** Chose refusing `Cancel`
  (and so `CancelAndConfirm`) over letting a stop reconstruct the intent
  under its admission alone. An unreadable file or an export conflict fails
  that reconstruction anyway, but a quarantine caused only by a current-policy
  refusal the classifier does not hold would pass it, then write the intent
  and commit a canceled outcome while `Inspect` still answered running from
  the in-memory quarantine. The fresh-context review found that path. A stop
  therefore waits for the repair and restart; abandonment stays available.
  `Collect` needs no gate: the engine calls it only after `Inspect` reports a
  terminal status.
- **Name the invocation only from its own file.** The item names the decoded
  invocation only when the entry is that invocation's canonical file. A
  stale copy or a mismatched record otherwise names a healthy invocation the
  quarantine does not hold, and the decoded name is unauthenticated file
  content that must not reach item text (also found in review).

## Refute-First Findings

- **Confirmed and fixed:** a stop writing a policy-quarantined intent; the
  item naming an invocation from a non-canonical file or carrying its
  unbounded decoded name; an item keeping a stale class's text (the key now
  includes the class).
- **Disproved by a check:** the per-entry checks in `listIntents` keep the
  old order, wrapping, and text; every entry is attempted; `Inspect`
  short-circuits before any reconstruction; rewritten tests require an
  empty held and quarantined report where the old ones required a nil error.
- **Allowed by decision:** no `newDaemon`-level test, because no test drives
  `newDaemon` in Claude mode (it needs a container runtime); the converge and
  the classifier are tested directly. An item opened under the Claude driver
  resolves only at a later Claude-driver start, since only that start reads
  the intents.

## Left For Other Units

Errors that arise after startup for an intent that was not quarantined
still abort the engine pass; the engine's handling of a driver error from
`Inspect` or `Collect` is #388's. Unbounded retry of a held running intent
is #385's.

Revisit when #388 lands engine-side isolation of collection errors: a
quarantined invocation might then report its error instead of running.
