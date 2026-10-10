# Honor a Task Line at Admission Only When Its Command Backs It

Work unit #1640, plan §5.4 (Admitted Agents). Follows
`devlog/2026-10-10-1415-task-line-record.md`, which recorded task lines and
left reading them to this unit. A proposal start setting a line is #1641, the
alternate-agent card is #869, and changing a line after submission is #1601.

## Chose One Selection Point That Reads the Stored Run

Attempt admission picks a role's agent in one function: the task's line when
the run's task has one for the role, the lineup otherwise. The card (#869)
will be a third arm ahead of both, in the same function. No parameter is
reserved for it; nothing writes a card yet, and an unused argument would
claim a shape nobody has designed.

Chose reading the run and its task inside the admission read transaction
over the in-memory run the caller holds, which the planning comment
sketched. A run value with no task would read as a task with no lines, and
admission would run the lineup's agent in silence. Every stored run is bound
to a task, so the stored row cannot fail that way, and a run that reads back
without a task is refused.

A line picks the agent only. The prompt is the lineup line's, so a role with
no lineup line is still refused. The startup check and the lineup-only
resolver used by the shadow reviewer and wardless roles have no task in view
and are unchanged. A lineless run admits with the same bytes as before.

## Anchored a Line to the Command That Set It

A line's id is an unkeyed hash of its own columns, so a row nobody submitted
still decodes, and choosing an agent chooses which credential runs. Before a
line is honored, the record its `set_by` names must exist, be of the kind its
`source` claims, and have created this task at the instant the line was set:

- `submit_task`: the task submission recorded under that command id names
  this task.
- `cli_submit`: the manual submission under that `cli:` identity created a
  specification run on this task. The `cli:` prefix is required because
  `submit_task` records a manual submission too, under `client:`, and a line
  claiming the CLI must not anchor to it.
- `proposal_start`: refused. Nothing writes that source until #1641, which
  adds its arm against the decision command.
- Every line: `set_at` equals the task's creation instant, because both
  writers set a line in the transaction that creates its task.

The planning comment had the CLI arm resolve the run the manual submission
names. That is the implementation run, which is not stored until the
specification is approved, so every CLI line would have refused its
specifier. The arm resolves the specification run derived from that
identity, which the submission stores in its own transaction. A test that
reads the lines a real `freesided submit` writes found this; the fixture
that modelled the submission by hand did not.

The anchor proves a line rests on a real operator command for this task. It
does not prove the agent is the one that command sent: neither record stores
the lines, and the request digest cannot be recomputed from stored state.
Binding the agent text needs the submission record to carry its canonical
lines, which is a contract change and is deferred. Follow-up: #1939.

## Refused Instead of Falling Back

A line that names an agent the tree lacks, an agent under a disabled
identity, or that fails the anchor refuses the attempt with the ordinary
agent-not-admissible hold. The lineup's agent never runs in its place: the
operator chose a credential for this task, and running another is the
failure the line exists to prevent (plan §5.4).

## Checked Only Task Ownership at Reconstruction

The store's admission re-gate resolves the cited line by id and requires it
to belong to the admission run's task. Chose that over also matching the
line's role and agent to the binding: the binding records no role, and the
store has no tree to resolve an agent name to a digest. A superseded line
stays valid to cite, since an admission recorded before a later change
honored the line that was current then.

## Refused a Reviewer Line the Review Source Cannot Run

The review source runs one agent under one identity, fixed when the daemon
composes it, until the review admission record exists (#898). Chose refusing
a review whose task's reviewer line names any other agent over ignoring the
line: an ignored line is a recorded operator choice the system contradicts
without saying so. The refusal is a review configuration failure, the class
the source already uses when its own line stops resolving. A line that fails
the anchor is refused the same way, so a forged reviewer line cannot pass by
naming the composed agent.

The comparison is by agent name against the lineup's reviewer agent. The
planning comment sketched comparing definition digests; the tree is fixed
for the daemon's run, so within it a name resolves to one definition.

The refusal names the line by its id and the agent the source runs, never
the line's own agent. That agent is text an operator typed: a credential
pasted there can pass the name grammar, and the refusal is stored as the
review failure's reason. Chose that over repeating the name, which would
have shown a mistyped agent on the card. This applies the rule
`devlog/2026-10-10-1415-task-line-record.md` set for a refused choice to an
accepted one, at the one place this unit records a refusal's text.

## Refute-First Findings

An independent review attacked the diff before the first commit.

- **Confirmed and fixed:** the test named for unchanged lineless bytes
  compared a subset of binding fields; it now compares the whole binding
  against the lineup-only resolution and is named for what it checks.
- **Confirmed and fixed in review:** the reviewer refusal repeated the
  line's agent, and that text is stored as the review failure's reason. It
  now names the line by its id; a test plants a credential-shaped agent and
  fails if the refusal repeats it.
- **Allowed by decision:** the store re-gate accepts a cited line that is
  superseded, of another role, or unanchored (above: it has no role and no
  tree, and only a caller that bypasses the engine can record one). A broken
  chain for any role of a task refuses every ward role of that task, because
  admission reads the task's lines together; a task whose lines fail their
  own check has no trustworthy line. A transient store read during review
  admission is classed as a configuration failure, as the same read of the
  role's line already was.
- **Allowed by the issue's contract:** a reviewer line naming another agent
  is found only when the review starts, after the writer roles ran, and its
  card's recovery does not change a line. The exits are a lineup change or a
  decline until #1601 lets the operator change the line. A task submitted
  with lines while they were recorded and not read runs under them from this
  change on.
- **Disproved by a check:** changed bytes for a lineless admission (the
  working tree and the base produced the same admission id and JSON over the
  same fixture); an attempt admitted with an empty run id; a second
  selection path that launches the lineup's agent; a line anchored to
  another task's command, to a `client:` record under `cli_submit`, or to a
  CLI identity under `submit_task`; a real writer's line failing the anchor
  at a later run of its task; a refusal that skips the hold; a review launch
  that bypasses the admission hook; a review request for an unstored run; a
  writer role's refusal text reaching a record or a log (dispatch keeps only
  the hold reason it classifies the refusal to, and drops the text).
- **Not covered by a test:** a real submission driven through admission of
  its implementation run. The store binds a campaign's implementation run to
  its specification run's task, and the tests read a real submission's lines
  at the specification run.

## Revisit When

- #1601 adds the command that changes a line after submission: the anchor's
  `set_at` rule and its two arms accept only a line set when the task was
  created, so that command needs its own arm.
- #1641 writes `proposal_start` lines: its arm must hold the line against
  the decision command.
- #869 writes the alternate-agent card: it is the third arm of the selection
  point, and the store re-gate resolves a `card` record id as it does a line.
- #898 lets a review run per-task agents: the reviewer refusal becomes
  selection.
- A hold or its card starts to record a writer role's refusal text: the
  refusal for a line whose agent the tree lacks repeats that agent, and
  must name the line by its id first.
