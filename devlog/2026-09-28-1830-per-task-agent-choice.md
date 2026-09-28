# Choose the Agent per Task

Work unit #1587, plan revision 73. Owner decisions of 2026-09-28. Follows
revision 72 (`devlog/2026-09-28-1800-capacity-scheduling.md`), which made
account and machine capacity the scheduled resources.

## Changed Assumption

The plan already supports several providers and several accounts per
provider, and lets any agent serve any role. But the only ways to pick an
agent were the project or deployment lineup, which applies to every task,
and the alternate-agent card, which appears only after a quota, expiry, or
capacity failure. The owner wants to pick the provider when starting a task:
this task on Codex, that one on the second Claude subscription.

## Chose Task Lines

Chose a task line: a per-task, per-role agent choice that sits between the
lineup and the card. The card still overrides one attempt, the task line
overrides the lineup for one task, and the project lineup overrides the
deployment lineup. A task line selects an agent only, from agents the tree
already carries; the role's prompt stays the lineup's. Rejected:

- **Per-project choice only.** Running one task elsewhere would mean editing
  the lineup, which moves every other task in the project too.
- **Automatic routing by remaining capacity.** Freeside can't observe provider
  quota, and the plan's routing policy isn't built. Until it is, balancing
  providers stays the operator's call.
- **A per-task prompt.** A prompt is control-plane text approved through the
  tree; a task line is operator input and approves nothing.

Task lines cover the specifier, implementer, remediator, and reviewer. The
shadow reviewer and the wardless roles stay on the lineup, because their
comparisons key on the lineup line and per-task agents would split them. A
task records one line per role, since the plan never lets one role borrow
another's line; a client can still offer one "writer agent" choice that sets
three lines.

## Kept the Choice Operator-Only and Never Silent

Only an authenticated operator sets a task line, at submission or later while
no attempt of that role runs. An issue, a label, or repository content never
sets one, because choosing an agent chooses which credential runs. A line
whose agent no longer resolves fails admission and raises the ordinary card.
Falling back to the lineup was rejected: the task would run on an agent and
subscription the operator didn't pick.

Review independence stays a recorded fact, not a gate (revision 65). A task
that names one vendor for both writing and reviewing shows a same-lineage
review. Requiring a different reviewer vendor was rejected for the same
reason revision 65 gave: the operator has to keep working when one provider
is out of usage.

## Revisit When

- The operator regularly moves waiting tasks between accounts by hand: that
  is the routing work the plan's routing policy should absorb.
- A wardless role's agent needs to vary by task, for example a project whose
  tasks span languages one vendor handles poorly.
