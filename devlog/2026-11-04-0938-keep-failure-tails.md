# Truncated Failure Output Keeps Its End

Tier A fix for #575. When a failure summary or a child process's output
passes its byte cap, Freeside now keeps the end, where the diagnosis is.
Before, every bound kept the start: ward errors mostly wrap with `%w`
first, so after the driver and engine wrap them again the specific cause
sat at the end of the string and was cut; and a noisy early verification
step could push a later failing step's output and `exit` line out of the
transcript entirely.

## Decisions

- **Chose head plus tail for failure summaries.**
  `exec.TruncateFailureSummary` keeps a 128-byte head, an elision marker,
  and 379 bytes of tail, within the unchanged 512-byte
  `exec.MaxSummaryBytes`. The head names the operation that failed; the
  tail holds the cause. Rejected: tail only, which would lose which
  operation failed when a long path or argument sits between them.
- **Chose to keep the head for agent questions.** A question leads with
  what it asks, so `exec.TruncateSummary` keeps its name and head-only
  behavior, and the question producers and re-deriving consumers
  (`engine/production_question.go`, `store/agent_question.go`) are
  untouched. Renaming it would have touched every question path for no
  behavior change; the new function's name marks the changed callers
  instead, and the `exec/stage` wrapper became `truncateFailureSummary`.
- **Chose the tail for captured output.** Verification room output, the
  verification transcript, the container CLI's reported stderr, and recipe
  extraction keep their last bytes. A failing command prints its diagnosis
  last. Calls that fail closed on truncation (`CLIRuntime.run` stdout,
  recipe extraction stdout and stderr) still fail closed, so which bytes
  they kept never mattered.
- **Chose one shared writer in `procbound`**, the owner's decision on the
  plan. `procbound.TailBuffer` replaces verify's `boundedBuffer` and ward's
  `capWriter` and `verificationOutput`. Every caller bounds a child
  process, which is what `procbound` is for. It grows to its bound and then
  overwrites in place, so a write costs its own length rather than a shift
  of the whole buffer; with 16 MiB caps and a flooding child the shift
  would copy the buffer on every pipe read. `projectimage.tailBuffer`, which
  shifts, stays as it is (issue non-goal).
- **Chose to accept both summary forms when reconstructing a prompt
  refusal.** `exec/stage` re-derives a committed prompt refusal's summary
  from the re-rendered error. An intent an older daemon committed holds
  the head-only form, which differs from the new one for an error over the
  cap. The plan assumed no prompt error passes the cap; the owner asked for
  proof or a legacy path rather than that assumption, and the proof would
  have had to hold for every future prompt error. So the check accepts
  either bound of the same error: both derive from the re-rendered error,
  so the old form authenticates the same refusal, and the stored outcome
  must still match. A test commits an over-cap refusal in the old form and
  shows it reconstructs, and that a summary derived from neither is
  refused.

## Trust

Tail-keeping lets agent, candidate, or provider output choose which bytes
survive, but under head-keeping it already chose all of them. The
transcript is evidence, not the verdict: the outcome comes from step exit
codes. The stderr `runPrepared` reports is read only for calls on
Freeside's own resource names, so the tail carries the exposure the head
did.

## Refute-First Findings

- **The bound can be passed: disproved by a check.** `TailBuffer` sizes
  its slice with explicit capacities, never `append` growth, and a
  randomized test checks length and capacity against the bound after every
  write across ring wraps.
- **Reported stderr leaks more: disproved.** Only calls that already
  reported stderr report it, still at most 512 bytes of the same stream;
  redacted calls still drain to `io.Discard`.
- **A transcript cut mid-line or mid-rune breaks a consumer: disproved.**
  The transcript is stored as `text/plain` evidence with a content digest;
  nothing parses it, and the head cut could already end mid-rune.
- **A consumer reads stderr's first line: confirmed and fixed.** The
  conformance suite's attachment-exclusion probe requires the first,
  top-level VZErrorDomain code to be 2. With the tail kept, stderr over
  512 bytes could lead with a nested code 2 and pass the probe on an
  unrelated start failure. `runPrepared` now wraps `errStderrTruncated`
  when it drops stderr's start, and the probe treats that as
  inconclusive.
- **A noisy last step evicts an earlier failing step: disproved.**
  `runRecipe` stops at the first nonzero exit, so a failing step is always
  the last one in the transcript.
- **The legacy summary form lets a forged refusal reconstruct:
  disproved.** Both accepted forms are computed from the error the current
  admission re-renders, the stored outcome must still match, and the
  intent file is the daemon's own trusted state.

Revisit when a stored summary starts gating authority by its content, or
when the legacy acceptance can be dropped because no daemon that wrote
head-only refusal summaries can still have committed intents on disk.
