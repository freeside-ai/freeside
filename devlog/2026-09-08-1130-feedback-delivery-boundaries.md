# Preflight Complete Operator Feedback Before Dispatch

The publication lane discarded the configuration needed to check whether
Return to agent feedback could reach the implementation agent. This work
repairs the preflight and dispatch gates without changing the input limit
or treating a recorded failure as authority to launch another attempt.

## Delivery Checks At Both Boundaries

The publication loop's helper engine now receives the same validator and
admission context as the main engine. It therefore includes the approved
specification, policy, complete feedback and candidate patch when checking
the rendered prompt. The admission and unknown-invocation replay paths also
recognize feedback as production work, so a retained queued intent cannot
bypass that check.

Keep the 31-KiB rendered-prompt limit. It protects the current shell-argument
transport, including quoting expansion. Increasing it or discarding part of
the candidate would trade a clear delivery refusal for a broken transport or
incomplete work. A broader transport change requires its own contract review.
Permanent pre-start refusals use existing durable failure handling;
operational read failures stay retryable. A known driver invocation retains
its recorded state and is never restarted just because preflight changed.

## Continuation After A Recorded Failure

Replaying the same command retains its outcome. The existing campaign
reattempt command can create a fresh attempt once the parent has an
authenticated final conclusion, using its approved specification and policy.
It does not carry the failed feedback, candidate patch or previous candidate
head into the new attempt. It is therefore a separate workflow choice, not
recovery of that feedback. Preserve the old command, failure and publication
when making that choice; do not restore a superseded ready card by editing
durable state.

Revisit when the prompt transport changes or a supported feedback retry
transaction is introduced. Those changes must preserve complete input and
recorded-attempt identity rather than reinterpreting old failures.
