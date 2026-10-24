# No Attended First Run for a New Agent

Plan revision 97. Mandatory note: a safety-policy change in §5.4 Admission,
decided by the owner on 2026-10-24 on #1431 ("I don't think it should be
required"). It supersedes the attended-first-run rule the admitted-agent
contract (#894) put in §5.4 and keeps every other admission step.

## Decision

Chose to drop the attended first run over keeping it, narrowing it, or
replacing it with an advisory item. An agent that resolves, is selected,
proves, and is credentialed runs under the deployment's operating mode from
its first launch. The unattended-eligible mark beside the agent in the tree,
the attended first run it gated, and the admission snapshot's attended flag
leave the plan; the daemon enforces them until the follow-up contract unit
#2265 lands.

## Why

- **The rule applied to the wrong grain once the lineup existed.** Revision
  65 made per-role experimentation the point of the lineup: the operator is
  expected to change a role's model or effort often. A line edit is a new
  agent digest, so every such edit, a one-step effort change included,
  forced another attended run.
- **It applied unevenly.** Revision 65 gave a call no gate before first use,
  on the argument that a call lasting seconds has no attended mode to sit in
  and is bounded by its site's authority contract and fail-safe. Stages kept
  the gate, so the same new agent needed a human look in one admission class
  and not the other.
- **It blocked a feature the plan asks for.** The Wave 8 exit run's
  rehearsal (#1613, gh-imgup#125, `main` at `0ceb010d`, root
  `rehearsal-125`) ran a task line moving the specifier onto the
  implementer's agent, and admission held it `admission_policy_refused`:
  the baseline marks each writer agent only for its own role's launch, so a
  cross-role line had no mark, and the real run's tree carries the same
  marks. That is #1616's exit line 4, unmet on that build for this reason
  alone.
- **What the attended run bought is already there after the fact.** It let
  an operator see a new configuration's first run before it ran unwatched.
  The admission snapshot records the agent, the launch, the route model,
  the requested and native effort, and the selection source; the run record
  shows what the agent did. A broken or wasteful configuration is visible
  there. What bounds a bad agent is unchanged: the ward boundary, the
  hostile import, verification, review, and the human merge gate.
- **This does not make the audit a gate.** Revision 65's note rejected the
  sampled audit as a replacement gate for the attended first run, because
  an audit read afterwards gates nothing. That still holds. This decision
  drops the gate rather than replacing it: gating use on a human look costs
  more than it buys once every lineup edit is a new pair, and the look the
  operator needs is the admission snapshot and the run record, which are
  not gates and are not claimed as one.

## Rejected Options

- **Keep the rule.** Rejected for the reasons above.
- **Narrow it to adapter or harness build changes.** The adapter suite per
  build already proves a build's launch capabilities, and a model or effort
  change is the experiment itself, so the narrowed rule would gate nothing
  the suite does not and still gate the thing the lineup is for.
- **An advisory item on each new pair's first unattended run.** Under the
  lineup each effort tweak is a new pair, so the deployment would raise one
  item per tweak, and the item would name nothing the admission snapshot
  does not already carry.

## Consequences

- The plan leads the code. Until #2265 merges, the daemon still refuses an
  unmarked pair in `unattended`, and `freesided auth adopt` still emits
  marks. #2265 removes the mark type and files, the `Attended` admission
  field, the `agent_attended_mark` check in the domain and the API schema,
  and the emission; it decides how the tree loader treats a leftover mark
  file.
- #1616's exit line 4 depends on #2265: a cross-role task line is admitted
  unattended only once the mark is gone.
- An operator who wants a look before a new agent's answers count still has
  two non-gating ways: run the deployment in `attended_dev`, or run a new
  reviewer or judgment line as a shadow first (§5.13).

## Revisit When

- A second operator joins, so that one person's after-the-fact look no
  longer covers every new configuration.
- A role gains a tool or egress the ward boundary does not bound (the
  researcher's web search is the first candidate), so that a bad first run
  can reach outside the gates named above.
- A real run shows a misconfigured new agent causing harm the verification,
  review, and merge gates did not catch.

Follow-up: #2265.
