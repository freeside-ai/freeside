# Task Lines Select An Enrollment, Model, And Effort (#2597)

Follows `2026-11-05-2314-agent-selection-digest.md` (#2596) under plan
revision 111. Admission, the alternate-agent move writer, and a table
rebuild are Tier A, so this note is mandatory.

## Decision

- **Chose a second encoding over rewriting stored lines.** A line and a line
  change are content addressed, and an admission cites a line by id, so
  rewriting a row would orphan every admission that cites it and fail the
  derivation check that re-runs on read. The migration also has no agent
  tree, so it couldn't translate a name. `freeside.task_line/v2` holds
  `selection` (enrollment, model, effort) and no agent; v1 keeps its bytes,
  its JSON shape, and its id. Every writer now writes v2. #2599 converts the
  v1 rows with the tree they were written under.
- **Rebuilt `task_lines` instead of adding columns.** 0125's CHECK ties a
  clear to a NULL agent, and SQLite can't alter a CHECK. Migration 0130
  rebuilds the table in the migrator's transaction and copies every row as
  v1, unchanged. `task_line_changes` stores each change's JSON body, which
  carries its own encoding, so it needed no change.
- **Chose a tree-only bridge in `agenttree` over resolving through the
  store.** `SelectionOfAgent`, `AgentForSelection`, and `LineAgent` read the
  committed tree alone, and a name's selection is the one
  `ReduceAgentSelection` gives, so the tuple a v2 line holds is the tuple
  admission records. Admission maps a v2 line to an agent, runs the path a
  v1 name takes, and then refuses unless the resolved selection equals the
  line's. #2599 removes the bridge when admission resolves a selection
  without a tree agent.
- **Treated names that share a definition as one agent.** The baseline tree
  writes one agent per role, so two roles that pick one model and effort on
  one enrollment get two names with one selection: the escalated specifier
  and the implementer, or the reviewer and a writer on one adapter. A fresh
  review showed that refusing every shared selection made the router, the
  move, and triage write lines admission then refused. `AgentForSelection`
  groups the agents a selection reduces to by `AgentDigest`, which leaves
  out the name. One definition is one agent: the role's lineup agent, then
  its escalated counterpart, then the first name in sort order shows for
  it, and admission's anchors accept any name of that definition
  (`LineRuns`). Two definitions with one selection, such as a repriced
  offer, still refuse, because either could run another credential path or
  price. Rejected letting tree order pick between definitions for that
  reason. Internal writers translate through `LineSelection`, which refuses
  a name whose selection maps to another definition, so no writer records a
  line admission would refuse.
- **Kept the routing policy, escalated lines, and the alternate-agent retry
  record on agent names.** The router and the move write v2 lines by
  translating the name they chose; their anchors check that a v2 line runs
  the agent the policy or record names. Moving those records to selections is
  two follow-up units: routing policy with escalated lines (Tier B), and
  the retry record (Tier A). `RecordAlternateAgentMoveLines` now takes the
  moved selection from its caller, and admission's move anchor re-checks it
  against the record's agent.
- **Kept the client wire on names until #2598.** Signet translates a wire
  name to a selection on the way in and refuses an unknown or ambiguous
  name with `ErrTaskLineAgentUnknown` (400), without repeating it. On the
  way out, the snapshot, the timeline, and the command receipts show the
  agent a v2 line maps to. Translating needs a tree, so a daemon without
  one, such as the fake driver, refuses a submission or change carrying
  lines instead of recording names it can't check. The convergence harness
  binds a tree-free bridge that gives each name a selection of its own.
- **Chose to show a v2 line the tree can't name as its
  enrollment/model/effort.** The wire has no field for a selection until
  #2598, and a routine tree edit, such as moving an agent to another
  effort, leaves old lines selecting what no agent runs. The plan didn't
  cover the outbound surfaces. The first cut failed the projection like a
  broken chain; the fresh review showed that hid every task that ran the
  old selection from the snapshot, broke the timeline for a stopped root
  (which has no tree), and failed replayed receipts. The agent field now
  carries `enrollment/model/effort`, the form `submit --task-line` takes.
  Agent names hold no slash, so the text never names an agent. Rejected an
  empty agent or a made-up name: either would show the operator something
  false. The text breaks the name pattern the API schema declares for
  `TaskLine.agent`, the timeline line's agents, and
  `TaskLineChangeRecord.agent`; the domain's timeline check accepts it, and
  no client checks the pattern on decode (the app checks names only in its
  mock server). Changing the schema is #2598's work, so the owner may veto
  this in review in favor of failing the projection.
- **Chose that a v1 head never equals a selection request.** The change
  path records what the operator chose and reads no tree, so asking for a
  selection over a v1 head records a v2 version even when it maps to the
  same agent, and is refused while the role runs. Rejected reading the tree
  there: whether the selection runs is admission's question.
- **Replaced `task-line -agent` with `-enrollment`, `-model`, and
  `-effort`, and `submit --task-line role=agent` with
  `role=enrollment/model/effort`.** An enrollment id is
  `<identity>/<client>`, so the flag splits the effort and the model from
  the right; a model id must hold no slash, which no supported harness's
  does. A recovery journal saved before #2597 still names agents, and its
  retry writes the v1 line its record backs.

## Review Outcomes

A fresh-context review of the first cut, beyond the findings above:

- **A proposal replay translates the command's agent before the replay
  check.** Declined. The proposal record stores no lines (#1641), so
  checking a replay needs the translation, and refusing a replay the tree
  can no longer translate fails closed.
- **v1 goldens were rewritten to v2.** Fixed: the router, cleared, changed,
  and move goldens keep their v1 bytes beside the v2 ones.
- **Triage computed the counterpart's selection before deciding triage was
  needed.** Fixed: it translates only when triage runs.
- **A v1 head never equals a selection request.** Kept, as decided above.
- **A model id with a slash can't be entered.** Declined: no supported
  harness has one.
- **Receipt validation compares less for v2 lines.** Kept: stored receipts
  are trusted rows (Tier B). A receipt's stored agent may differ from the
  agent its line shows on the wire after the tree changes.

## Refute-First Pass

Claims tried against the change, each with its outcome:

- **A migrated row changes id or bytes.** Disproved:
  `TestTaskLineSelectionMigrationKeepsV1Rows` seeds v1 lines and changes of
  every source, migrates, and finds the columns identical, every line
  DeepEqual to its seed, and every change backing its line.
- **An admission citing a pre-migration line no longer records or reads.**
  Disproved by the same test, which records and reads one after the
  migration. The old schema can't serve `GetTaskLine` under the new code,
  so the admission is written after migrating; the derivation check runs on
  every read either way.
- **A v2 line runs an agent other than its selection.** Disproved:
  admission compares the resolved binding's selection with the line's
  (`TestV2TaskLineAdmitsTheAgentItSelects`).
- **A selection no agent has, or two definitions share, falls back to the
  lineup or to tree order.** Disproved: both refuse as agent-not-admissible
  (`TestV2TaskLineAdmissionRefusals`), and so does a router line whose
  selection maps to an agent its policy doesn't order.
- **A writer records a line admission refuses.** Confirmed by the fresh
  review for names sharing a selection in the baseline tree. Fixed by the
  definition grouping and `LineSelection`;
  `TestV2TaskLineTwinsAreOneAgent` and
  `TestTriageEscalatesToTheImplementersSelection` admit the lines the
  router, an operator, and triage write.
- **A real enrollment id can't be entered on the command line.** Confirmed
  in self-review: the flag split on every slash. Fixed by splitting from
  the right (`TestTaskLineFlag`).
- **A moved line holds after the tree renames the agent its record moved
  the role to.** Disproved: the anchor compares the mapped name with the
  record's and refuses (`TestMovedV2LineRefusesAnotherAgent`).
- **A refusal echoes operator text.** Confirmed in signet: the tree's
  unknown-agent error quotes the name, and the bridge wrapped it. Fixed by
  dropping the tree's error; `TestChangeTaskLineTranslatesAgentNames`
  checks it. Engine refusals name the line by id, and the refusal tests
  check that no selection appears.

Revisit when #2598 moves the wire to selections (the signet bridge and the
enrollment/model/effort rendering go), when #2599 converts v1 rows and removes agent
documents (the `agenttree` bridge goes), or when the follow-up units move
the routing policy and the retry record to selections.
