# An Exit Run's Real Writers Default to Opus 5.5 at Medium

Owner decision of 2026-11-10, made while planning #3024. It sets how an
exit run's real root gets its writer and judgment models and efforts. The
rule lives in docs/production-walkthrough.md, Choose The Real Run's
Writers.

## Decisions

- **The tracker's lineup first, then a stated default.** Chose a lineup
  the tracker's Exit section or exit-proof unit pins, falling back to one
  default the walkthrough states, over leaving the choice to whoever
  prepares the run. Wave 9's real root (#2386) had no rule to follow, so it
  copied the previous root's settings and gh-imgup#147 was written by
  `claude-haiku-5-5` at `low` without anyone choosing it. Earlier real runs
  picked their models on purpose
  (`devlog/2026-10-22-1346-rehearsal-real-models.md`); the drift came from
  copying, not from a rule.
- **Default: `claude-opus-5-5` at `medium` for every writer role and for
  judgment, with no escalated specifier.** Chose this over the fixture
  pass's Haiku at `low`, because the real run opens a real pull request on
  someone's repository and that work should be the model's real output,
  not the cheapest one that completes the workflow. Also rejected `high`
  or `max`: they add spend on every run, and the exit gates prove the
  workflow, not peak writer quality. A tracker that needs more pins it.
- **Review stays with onboarding.** `review.model` and `review.effort` are
  part of the review configuration digest the project's onboarding
  approved, so the lineup rule leaves them alone. The go-ahead still names
  them, so the owner sees the whole lineup at once.
- **The go-ahead names the lineup and preflight checks it.** Nothing
  refuses a weak model: the lineup probe refuses only a model the pinned
  CLIs don't serve. The owner's go-ahead is the one place a weak or copied
  lineup can be caught, so the exit-run skill lists each role's model and
  effort and its source there, and Preflight checks the written settings
  against what was approved.

Revisit when a newer model ships, or when a real run shows `medium` too
weak for its target or too costly to run.
