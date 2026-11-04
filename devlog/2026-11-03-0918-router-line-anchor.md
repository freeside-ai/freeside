# A Router Line Holds Only Against The Policy It Cites

#2393 adds the router: at a task's first routable admission it pins every
task-line role to one usage pool by writing `router` task lines (plan §5.4).
Admission trusts a decoded line to choose which agent, and so which
credential, runs. That makes the router arm of `anchorTaskLine` a
reconstruction trust boundary, so this note is mandatory.

## The Anchor Checks The Policy, Not The Marks

Chose to hold a router line to the routing policy revision it cites, with
six checks in `anchorRouterLine`:

- `set_by` is `router:<task_id>` for the line's own task.
- The line is the role's first version, the only one the router writes.
- The cited revision exists.
- The revision was set no later than the line.
- That revision orders the line's agent for the role, and, when the line
  passed an agent over, some role's order names that agent.
- A line the `drain_first_account` tier chose is on the pool that revision
  names as its drain-first account.

A row that passes can only run an agent the operator ordered for that role
in a revision that existed when the line was set. The refusal texts name no
agent, like the other line refusals.

Rejected rechecking the marks and readings the router acted on. Both are
passing facts: a mark clears at its reset or on an allowed reading, and a
weekly reset passes, so a recheck would refuse a correct line once they
changed, and the task would never admit. Current marks are the later-role
hold's job, not the anchor's. The provenance's own consistency (its tier
against `ranked_by` and `passed_over`) is already enforced when the line
decodes.

## Choices Where The Issue Was Open

These were chosen at planning:

- **An unordered first role leaves the task on the lineup.** The router runs
  only when the admitted role is ordered. A task first admitted on the
  lineup is never routed later, so the lineup history check reads stored
  admission records, not re-gated ones.
- **The first ranked agent passed over is cited.** The provenance names it
  and its first mark.
- **Unresolvable agents are skipped.** An agent that is absent from the
  tree, unenrolled, disabled, or fails the join is never cited and never
  chosen. A failed store read still fails the routing, so I/O errors aren't
  mistaken for "doesn't resolve".
- **The exit-run fixture seeds a reading, not a refusal mark.** A reading is
  what a collection writes and what a later collection replaces, so the
  fixture clears itself.

These were chosen in implementation:

- **The deciding role takes the chosen agent.** Other unlined roles take
  their order's first agent on the chosen pool, as the issue says, even one
  the router passed over there for its own model's marked window; that role
  then holds until the mark clears. Rejected excluding such an agent: a role
  with no other agent on the pool would fall back to the lineup and leave
  the pool. The provenance allows the line (#2516), since two agents on one
  pool can run different models.
- **With no resolvable agent, nothing is written.** The task stays on the
  lineup instead of holding on a policy that can't run.
- **Shadow reviewer admissions don't count as lineup history.** The shadow
  reviewer always runs on the lineup, so counting it would stop every
  reviewed task from being routed.
- **The later-role hold passes an unanchored line through.** Admission then
  refuses it with the typed refusal, so a bad line never looks like a quota
  wait.

## Choices In The Revision 107 Rework

Plan revision 107 replaced the fixed order with three tiers: the drain-first
account, then the soonest known weekly reset, then the policy's order
(`devlog/2026-11-03-1111-drain-soonest-reset.md`). #2516 reshaped the
provenance to match. Chose in the rework:

- **A reading is weekly when its `length` is seven days.** Rejected matching
  window names such as `seven_day`: they are provider vocabulary, and the
  record already carries a provider-neutral length. No collector sets
  `length` yet, so until the Claude collector (#1717) does, no pool has a
  known weekly reset and the policy's order decides, as revision 103 did.
- **A pool ranks by its first agent's model.** `ranked_by` holds one reading
  per pool, and two agents on one pool can run models with different weekly
  windows. The pool's first agent in the role's order names the model; of
  the weekly windows that apply to it, the soonest reset ranks the pool.
- **The drain-first account's reset ranks nothing.** The account leads by
  policy, so its reading stays out of `ranked_by`.
- **The drain-first tier records no passed-over agent.** The tier rule
  forbids one, even when the account holds two agents for the role and the
  first is marked.
- **A routed reviewer's marked window uses the review gate's capacity
  hold.** Main had since merged one hold path for a full pool or a memory
  refusal; the quota-window refusal joins it instead of keeping a parallel
  wait.

## Refute-First Findings

A fresh-context reviewer tried to refute the diff before commit.

Confirmed and fixed:

- **Every admission moved the sync revision.** The router opened a write
  even when it routed nothing, and every `Store.Write` bumps the revision,
  so a held role's retries churned sync. Chose to decide in a read and open
  a write only when there are lines, deciding again inside it.
- **A policy set between the admission instant and the router's read made
  the router write a line its own anchor refused.** The instant is fixed
  before the read, so a revision set in between (or a clock step back) was
  cited by a line set before it. That line stuck the task, because the
  router never runs twice. Chose to cite the revision in effect at the
  instant: the latest one set by then, or none.
- **The router and the later-role hold disagreed on a mark with no reset.**
  The plan treated a pool with any no-reset mark as having no free time.
  The later-role hold ignored such marks. A model mapped to two windows
  could be pinned and then held on the same pool at once. Chose the issue contract's rule over the plan's, for both: a mark
  with no reset doesn't hold, matching the quota park's resume gate, and a
  pool frees at the latest reset among its other marks.

Allowed by decision:

- **A cleared line's lineup admission keeps the task off the router.** This
  matches the contract: the task has run a task-line role on the lineup.
- **An agent whose enrollment generation carries an integrity mark is not
  skipped.** Admission refuses it, as it refuses the same agent on the
  lineup, and the operator moves the line. Skipping it would mean reading
  generation state at routing time for a credential fault, not a quota one.

Disproved by a check:

- **Only the router writes `router` lines.** Submission, proposal start, and
  line changes all hard-code their own source.
- **Concurrent first admissions can't write twice.** The final decision and
  the write share one transaction, and the store refuses a router line that
  isn't version 1.
- **The review wait reaches the hold through the real admitter.** It spends
  no retry budget, requests no review, and the admission write clears it.

A second fresh-context pass tried to refute the revision 107 rework.

- **Fixed: excluding a passed-over agent moved a role off the pool.** The
  exclusion depended on rank position, so it could also miss a marked
  agent. Removed it, as recorded above.
- **Declined: the anchor is looser than the router.** It accepts a
  `soonest_reset` or `policy_order` line on the drain-first pool, and any
  revision set before the line rather than only the one in effect then. The
  router writes neither, and the anchor guards stale policy, not tampering
  with the daemon's own database.
- **Disproved by tracing:** every branch of the pick satisfies the
  provenance's tier rules; ties keep the policy's order; the drain-first
  pool never enters `ranked_by`; no map iteration reaches the output; every
  recorded instant is UTC.

Revisit when the alternate-agent card (#869) re-pins a routed task: its
lines are no longer a role's first version, so the anchor's version check
must learn that writer.
