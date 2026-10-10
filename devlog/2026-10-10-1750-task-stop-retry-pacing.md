# Settle a Task Stop That Can Never Be Proven

Work unit #1921, found by the Wave 8 exit run (tracker #1613). The daemon
retried a refused task stop every 100ms for the life of the state root and
logged each failure. The fix paces retries in memory and stops retrying a
stop whose proof can't succeed. #1920 owns which stops can be proven.

## Chose Working It Out at Each Start over a Durable State

The engine keeps a per-request record in memory: when the request may be
tried again, or that it's settled. A restarted daemon rebuilds the record
from two things it already has, the request's recorded `failed_to_stop`
state and the runtime adapter's answer to "can a stop of this task ever be
proven?".

Rejected a durable "can never be proven" cancellation state. It would need a
new domain state, a store migration, and an API schema change, so a
`kind:contract` unit, to save one coverage lookup per open request at
startup. The coverage record is written once and `freesided` never changes
the sync epoch while it runs, so the lookup gives the same answer for as long
as the state root exists.

The engine asks the adapter through an optional `Unprovable` hook instead of
classifying the attempt's error. The coverage refusal reaches the engine
joined with every teardown error, and only the adapter holds the coverage
record. The hook returns a reason or an error, never both: a failed epoch
read is an error and leaves the stop retryable, because a reason ends retries
for good.

## Chose Not to Retry Teardown for an Unprovable Stop

The owner's contract for #1921 lists this as a non-goal. The first attempt on
a task outside coverage still asks every known child to stop. If one of those
teardowns fails in that attempt, nothing asks again, in that process or after
a restart.

Rejected one teardown attempt per daemon process for such a request. It would
keep the endless retry, only slower, and the contract's restart criterion
says `StopRun` isn't called again. Retrying teardown alone would need the
adapter to report teardown errors apart from the coverage refusal.

## Chose One Attempt per Process for a Refused Acknowledgement

The store refuses an acknowledgement with `store.ErrCancellationBinding` when
the task's episode, run membership, or sync epoch changed after the Stop was
accepted. The request then stays `requested`, so nothing durable tells a
restarted daemon the stop was tried. The daemon settles the request in memory
and makes one more attempt after each restart. Reading the store code, none
of the three inputs changes back; that reading is the only evidence.

## Kept the Review Retry Schedule, Not Its Function

Backoff is one second doubling to 256, the schedule `reviewRetryDelay` uses.
Chose a second one-line function over calling `reviewRetryDelay`, whose
comment ties it to keeping three review-retry reconstructions in step. A
change to review pacing shouldn't move stop pacing with it.

## Refute-First Findings

An independent reviewer tried to break the change before the first commit.

- **Confirmed and fixed: join order defeated the log deduplication.** A
  stop's children finish in any order and their errors join in that order,
  so the same failures read as a new error on each attempt. The comparison
  now sorts the joined lines.
- **Confirmed and fixed: nothing pinned a failing `Unprovable` check.** A
  change that settled on the hook's error would have passed every test. A
  test now holds the stop retryable when the check fails.
- **Confirmed and fixed: a refusal at the deadline was rewritten every
  pass.** While a worker outlived its deadline, a refused acknowledgement
  was attempted again on each pass. The deadline branch now skips a settled
  request.
- **Allowed by the contract: a stop settled with no teardown request.** A
  request can be `failed_to_stop` before any child was asked to stop: the
  daemon restarts after the deadline but before the worker finishes, or the
  state root moves from a daemon with no cancellation runtime (the fake
  driver) to the Claude runtime. A restart then settles it. The restart
  criterion says `StopRun` isn't called again.
- **Allowed, unchanged: a worker past its deadline logs nothing until it
  returns.** The deadline branch wrote no line before this change either.
- **Declined: a test that `freesided` passes the hook to the engine.** No
  harness runs the Claude-mode wiring. The function the hook calls is tested
  directly, and the wiring is one closure over it.
- **Disproved by trace: a provable stop being settled.** `StopRun` and the
  hook call the same coverage function, so a reason implies `StopRun` fails.
- **Disproved by trace: a second attempt beside a running worker.** The
  attempt's entry is removed only after its worker returns.

Revisit when #1920 changes the coverage rules or the outcome recorded for a
refused stop.
