# A Failed Specification Retries as One More Iteration

Chose to retry a specification invocation that ended failed, canceled, or lost
before approval as one more iteration of the same specification run (#2315).
`freesided reattempt -task <task> -reason <text>` writes the next iteration's
request with the failed one's inputs plus a `retry_reason`, and supersedes the
failed invocation's `execution_failure` card in the same write. No run,
campaign, or task is created. Before this, a failure at the first stage left
only a second `submit`, which breaks the one-task-per-target rule (#1922).

## Rejected Options

- **A new specification run, campaign, or task.** Each one forks the task's
  identity graph for a transient failure, and a new task is exactly what the
  one-task rule forbids. The iteration chain already models a repeated
  specifier invocation with the same inputs.
- **Reusing `submit --retry-submission-id`.** It replays a saved submission
  idempotently and must keep returning the same task, so it can't also mean
  "start again".
- **Retrying automatically.** A retry spends iteration budget and can repeat a
  failure that needs a person, so it stays a deliberate act with a reason, like
  reattempt after approval.
- **Exempting the retry from `specification.max_iterations`.** The budget
  bounds specifier invocations, and a retry is one.

## How the Chain Authenticates a Retry

`verifySpecificationChain` accepts a fourth transition beside answer,
research, and request_changes. A retry is authorized only by the stored facts
of the iteration before it: a terminal with a failed, canceled, or gone status,
and that invocation's failure card concluded by supersession. The next request
must equal the previous one advanced by one iteration, carrying a non-empty
reason. Without the card check, a forged request row with a reason and the
same inputs would pass on the terminal alone. The reason is set only on a retry
iteration and cleared by every other transition, so `retry_reason` on any
other row fails the chain.

`retry_reason` is omitted when empty, which keeps every existing request row
canonical under strictjson without a migration.

## The Command Result Leaves the Implementation Aliases Empty

The implementation plan asked to fill the result's `run_id`, `invocation_id`,
and `stage_id`. `submit.go` documents those as aliases of the implementation
fields, and a retry starts no implementation run, so they stay empty and the
specification fields name the new iteration. A caller that reads `run_id`
after a retry gets nothing rather than a specification run under an
implementation name.

## The Record Reader Skips Single-Iteration Runs

`inspect task` lists retries per run. It returns an empty list without decoding
anything when a run has no second request, which is also the only case where a
retry can't exist. Seeded task-record fixtures carry request rows too thin to
decode, so decoding unconditionally would have put errors in every fixture
record.

## Refute-First Findings

A fresh-context reviewer tried to break the chain check and the command
before the first commit.

- **Confirmed and fixed:** `inspect_index.golden` still pinned record v11, and
  `TestReattemptTaskSelectsNewestCampaign` expected `-task` to fail the same
  way as `-campaign` on a campaign with no implementation run. `-task` now
  tries that campaign's unapproved specification instead, so the test states
  both refusals separately; neither selector falls back to the older
  campaign.
- **Disproved by tracing or tests:** a forged retry row without a failed
  terminal, with changed inputs, or authorized by another invocation's card or
  a card concluded for another cause; the retry branch shadowing answer,
  research, or request_changes; a refusal or replay raising the revision; two
  concurrent retries with different reasons; a budget off-by-one; and the
  record reader's shortcut hiding a retry.
- **Allowed by decision: replay covers only a pending retry.** The plan
  defines a replay as a same-reason command while the retry iteration has no
  terminal. After that iteration ends, a same-reason command is a new act:
  refused once the retry completed, or a further retry when it failed again.
  The request carries no command identity to tell a duplicate from a
  deliberate second retry, and `specification.max_iterations` bounds both.
- **Allowed by decision: every failed status is retryable.** An
  `ErrSpecificationInputUndeliverable` failure is recorded as failed, and a
  retry with the same inputs may fail the same way. Some of those causes
  depend on daemon configuration, such as the vendor instruction file's size,
  so a retry after a fix can succeed. Telling them apart would mean parsing a
  failure summary; the budget bounds a wasted retry.

Revisit when #869 adds the card's Retry action, which should call
`engine.RetrySpecificationRun` rather than a second path.
