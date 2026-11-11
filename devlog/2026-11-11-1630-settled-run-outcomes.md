# Settled Run Outcomes

Decision record for #3043: three final run outcomes, `handed_off`,
`stopped`, and `abandoned`, for a run no milestone concludes but nothing
will run again. Before this, `freesided inspect task`, follow, and the run
API reported such runs `pending` and non-final indefinitely; exit runs on
#1462 met an approved specification run and a stopped task's run that way.
The note is mandatory because the outcome enum is a contract (`api/openapi.yaml`
`RunOutcome`).

## Settlement Sits After the Authenticated Conclusion, Off the Gate Path

Chose a separate pure step, `domain.SettleRunConclusion`, applied after the
authenticated conclusion by the read surfaces only, over teaching
`domain.ConcludeRun` the new outcomes. `ConcludeRun` is milestone-only, and
the engine and the Tier A gates (publication completion, publication
reevaluation, the retry gates) read it or the authenticated conclusion built
on it. Changing it would have made this a Tier A unit and handed those gates
outcomes they never need: a retry over a closed workflow already skips the
final check, a cancellation fence refuses a stopped task's retry, and retry
parents are implementation runs, never specification runs. A domain test pins
that `ConcludeRun` never returns a settled outcome.

`signet.SettledRunConclusion` gathers the facts from records the store
already holds, so no migration or backfill is needed and older runs settle
too:

- **Hand-off:** the run's production attempt names it as the specification
  run and carries an approved specification digest (`boundImplementationRun`,
  the fact follow already reported as `implementation_bound`).
- **Stop:** the task's cancellation is `confirmed` and its target names the
  run. Confirmation asserts quiescence and fenced launches; `requested` and
  `failed_to_stop` assert neither, so they settle nothing.
- **Abandonment:** the run's workflow episode is closed.

Readers that apply it: the run API projection (`GetRun`, `ListRuns`,
`Bootstrap`), observedb's conclusion (follow, its snapshot, and `resume`), and
`inspect task`. The intake run count keeps its milestone-only rule.

## Precedence and the In-Flight Abandoned Run

A conclusion already final from milestones keeps its outcome and detail: a
`published` run whose task was later stopped stays `published`. Otherwise
`handed_off` beats `stopped`, which beats `abandoned`, matching
`Task.DisplayLifecycle`, where confirmed cancellation wins over abandonment.

Abandonment neither stops a provider execution nor certifies quiescence
(`freesided abandon`), so an abandoned run with a stage attempt whose
invocation observation is still `pending` or `running` stays pending until
that execution ends. Settlement reads the overlaid invocation statuses follow
reads; when `inspect task`'s overlay fails, its stored rows can only keep such
a run pending longer, never settle it early. A stop needs no such wait because
confirmation already asserts quiescence.

The wait counts stage attempts only, narrower than the issue's "no invocation
observation", because the engine keeps observing a closed run's stage attempts
and nothing else. A review invocation's row is written under the run but is
no stage attempt; once the episode closes, the engine only files the review's
result in the inbox and never records that row again. Counting it would leave
a task abandoned mid-review `pending` forever, the symptom this unit fixes.
The closed workflow never acts on a review result, so ignoring the row
settles nothing early that matters. A stage attempt the provider reports
unknown or lost after closure gets no new row either, so such a run still
stays pending; that rarer case is left to #3081.

A settled outcome is a reading of current records, not a stored verdict, so a
later record can replace it: a terminal milestone recorded after a stop or an
abandonment concludes the run from milestones instead, and an attempt
dispatched before the close whose first observation arrives after it moves an
`abandoned` run back to `pending` until that execution ends. Each reader shows
the latest reading, so a streaming follow that ended on `abandoned` reports
what was true when it ended.

## Follow and Resume Change

Because the settled outcomes are final, a streaming follow of such a run now
ends instead of polling. A handed-off specification run keeps the snapshot
state `implementation_bound`, so a supervisor reading `state` sees no change;
the snapshot gains `stopped` and `abandoned`. `resume` now refuses a settled
run as terminal, pointing to reattempt only for `abandoned`: a stopped task's
fence refuses a reattempt, and a handed-off run's work continues in its
implementation run.

Revisit when another run outcome is added, or when a gate needs to tell a
stopped or abandoned run from a pending one.
