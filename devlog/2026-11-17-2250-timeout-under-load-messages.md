# Name a Timeout Under Load in Prelaunch and Conformance Failures

In #2382's live run, an overloaded machine (load about 300 on 10 cores)
failed prelaunch's `prod_execution` check and Claude adapter conformance,
and both messages pointed at a defect that wasn't there. #3337 makes both
failures say they ran out of time. This note records the two choices the
change rests on. The work is Tier A by path (`cmd/freesided/preflight.go`,
`internal/ward/`, `internal/daemoncompose/agent_selection.go`).

## Decisions

- **Put the deadline in message text, never in the error chain.** Chose
  text over wrapping the context's error or cause with `%w` because callers
  classify runtime failures by `errors.Is(err, context.DeadlineExceeded)`
  (`classifyCodexInstructionMaterializationFailure` in
  `internal/ward/codex_review_source.go`, `internal/ward/launch_record.go`,
  and engine publication code). Today a killed runtime call's chain holds
  only the `*exec.ExitError`, so adding the deadline would move those
  failures into another class. `runPrepared` changes only the operation
  label it formats, and keeps its `%w` operands as they were. The new
  sentinels (`errProdExecutionUnanswered`, the adapter proof's wrap) add
  chain members that nothing tested before, and keep every existing one.
- **Every budget cause wraps `context.DeadlineExceeded`.** `net/http`
  returns `context.Cause(ctx)` in place of `ctx.Err()`, so a bare cause
  would have dropped the deadline class from the prod probe's error. Each
  of the four causes wraps it, so code that returns the cause keeps the
  class. The prod probe's test reads through the real control client for
  that reason.
- **Name the budget through the context's cause.** The three budgets that
  bound a conformance runtime call (`adapterProofBudget`, `HandoffTimeout`,
  and the ward suite's budget) now set a cause naming the budget and its
  length, and a runtime call that fails after its context ended prints
  `context.Cause`. Chose that over passing budget names down the call
  stack, because the cause already travels to every derived context, and
  the innermost expiring budget is the one named. A call whose context is
  still live gets no deadline wording, so a process the OS kills under
  memory pressure isn't misreported as a timeout.
- **Don't retry a timed-out adapter proof.** Chose a message telling the
  operator to restart the daemon once load drops over one retry with
  backoff, because:
  - `proveAdapterOnCandidates` already runs the proof on each substitute
    subscription, and in #2382 that second run died the same way.
  - Start conformance blocks daemon start under a proof-pass reservation,
    so a retry adds at least one more 3-minute budget per candidate to an
    already slow start.
  - Load heavy enough to outlast two 3-minute budgets is unlikely to clear
    during a short backoff.
  - A restart reruns conformance, which is the only path that resolves the
    health item (`convergeHealthItems` resolves it only on a pass).

## Refute-First Findings

- **Disproved: the redacted branch could leak through the cause.** The
  redacted `runPrepared` branch withholds stderr because it can echo a
  credential or name a foreign object, and its error reaches
  `ConformanceFailure.Reason`. It now prints `context.Cause`. Every cause
  the daemon sets is a fixed string: the three budgets,
  `errProdExecutionUnanswered`, `rig.go`'s `errRigCleanRelease` and
  `context.Canceled`, and `signal.NotifyContext`'s signal cause. No cause
  carries runtime-observed or credential text. Nothing enforces that, so
  `contextEndedOperation`'s contract requires fixed-text causes.
- **Confirmed and fixed: a bare cause dropped `DeadlineExceeded` from the
  prod probe.** The fresh-context review found that `net/http` reports the
  probe's cause, so `errors.Is(err, context.DeadlineExceeded)` went from
  true to false on a real timeout. No consumer read that class, but the
  contract keeps every `errors.Is` result. Fixed by wrapping the class in
  every cause (above).
- **Disproved: the new text could trip `isAttachmentExclusion`.** It
  classifies a start failure by "attachment", "storage device", and a VZ
  code in the message. None of the inserted wording or causes contains them.
- **Allowed: an already-done context still reports `DeadlineExceeded`.**
  `exec.Cmd.Start` returns `ctx.Err()` for a context that ended before the
  call, so that error's chain holds `context.DeadlineExceeded`, as it did
  before. The change adds only text there.

## Assumption

The #2382 kill is inferred to be the 3-minute adapter budget from the
budgets' lengths, not read from a log. If something outside Freeside killed
the start while its context was live, the message stays as unhelpful as
before, by design.

Revisit when the adapter conformance record gains a failure reason (the
health item could then carry the timeout), or when something reruns
conformance without a restart.
