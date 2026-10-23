# Rehearsals Run the Real Models at Low Effort

Owner decision of 2026-10-22, recorded because it would otherwise exist
only in chat. It replaces the exit-run rule that a rehearsal pass sets every
writer, review, and judgment role to the lowest-cost model at `low`.

## Decisions

- **Real models, `low` effort.** Chose to keep each role's real-run model
  and drop only its effort to `low`, over the lowest-cost stand-in model.
  A stand-in proves nothing about the model the real run picks: a model the
  images, the subscription, or the pinned `judgment.claude_bin` refuses
  passes the rehearsal and then fails in front of the operator. Also
  rejected: the real run's exact efforts, which would let the rehearsal
  reuse the real agent-tree commit but costs the most for little more
  proof. Effort is still part of the emitted tree, so the rehearsal keeps
  its own agent-tree commit.
- **Same task lines.** Chose to have the rehearsal submit every
  `--task-line` the real run will pass. A line the tree cannot honor
  refuses the attempt (daemon/README.md, `--task-line`), so that refusal
  belongs in the rehearsal. Two limits follow from the code. Writer roles
  on one model and effort share one agent, so `low` can merge agents the
  real tree keeps apart; a writer keeps its real effort where that would
  drop a name a line uses. And a line resolves only when its role's attempt
  is admitted, so a clean review leaves a remediator line unproven, and the
  run record says which lines the pass proved.

## Evidence

The Wave 8 / #1616 exit run's rehearsal had already departed from the old
rule: Fable at `low` for the specifier, Opus 5.5 at `high` for a remediator
named by a `--task-line` override, and `gpt-6.1-sol` at `high` for review,
to probe the models and the review image the real run used. The owner then
said the lowest-cost rule was not what they wanted.

Revisit when a real-run model is too costly to call even at `low`, or when a
model's behaviour at `low` stops predicting whether it is served at its
real effort.
