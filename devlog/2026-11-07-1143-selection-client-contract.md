# The Client Contract Carries Selections (#2598)

Follows `2026-11-06-1002-task-line-selection-tuple.md` (#2597), which moved
stored task lines to an enrollment, model, and effort but kept the wire on
agent names. This unit changes the API schema (`kind:contract`), so the
note is mandatory. It is Tier B: no Tier A path changed, and admission's
check on a line is unchanged.

## Decision

- **Chose two encodings on the wire over a tree-translated name.** A
  `TaskLine`, a timeline line, a line change, and a submission's recorded
  lines carry `agent` and `selection`, exactly one non-null: a v1 line as
  its name, a v2 line as its selection. No projection reads the tree, so a
  tree edit no longer changes what a recorded line reads as, and a daemon
  without a tree (a stopped root, the fake driver) projects every line.
  Rejected showing a v1 line as the selection its agent runs today: that
  needs the tree, and it would show a selection the line never recorded.
  #2599 converts the v1 rows; until then a client renders both. The
  `enrollment/model/effort` text #2597 put in the agent field is gone, so
  that note's open veto is moot.
- **Clients write selections only.** `submit_task`, `change_task_line`, and
  a proposal start carry `TaskLineSelection`; the schema no longer accepts
  a name from a client. Signet refuses a selection whose enrollment the
  daemon lacks, or whose recorded discovered catalog doesn't serve its
  model and effort, with a 400 that names the field and leg
  (`task_lines[0].selection.effort`) and never echoes the text. Signet and
  admission share one check (`DiscoveredModelCatalog.Serves`), so once a
  catalog is recorded they agree on the model and effort.
- **Chose to check only the store in signet, not the tree.** Until #2734,
  admission also needs one tree agent that runs the selection, and with no
  catalog recorded it limits effort to the harness's native levels. Signet
  checks neither, so a client can record a line that admission later
  refuses. Admission fails closed, the app offers only selections tree
  agents run, and the schema says so. Rejected checking the tree in
  signet: that rebuilds the bridge this unit removes, for a requirement
  #2734 retires.
- **Chose nullable discovery over refusing an enrollment with no
  catalog.** The catalog lists every enrollment with `discovery: null` when
  discovery recorded nothing, and a selection on it passes signet on shape
  alone. Codex enrollments have no discovery, and admission already
  decides those at the admitting revision. Refusing them would block every
  Codex line, and leaving them out would hide them from the picker.
- **Chose a never-null catalog over a nullable one.** Enrollments come from
  the store and exist without a tree, so the bootstrap's `agent_catalog` is
  always present. Without a tree its `agents` and `lineup` are empty and
  `lineup_revision` is null. Rejected keeping the catalog nullable: a
  client couldn't tell "no tree" from "no enrollments", and picking a
  selection needs the enrollments either way. Each tree agent and lineup
  line carries the selection it runs, so the app picks among tree agents
  and sends their selections, and it calls a choice stale when no agent
  carries it or its enrollment left.
- **A recorded catalog moves the sync revision.** The bootstrap now
  carries discovery, so `RecordDiscoveredModelCatalog` writes on `WriteTx`.
  Under `WriteInternal`, a client holding a catalog would never refetch the
  new model list. The heartbeat still compares only `lineup_revision`.
- **Deferred the routing policy, escalated lines, and the alternate-agent
  offer and its commands.** They stay on agent names. Each record still
  names an agent in the daemon, so moving its wire first would make signet
  translate an enrollment back to a name, picking a model and effort the
  operator never chose. The daemon units #2736 (routing policy over
  enrollments) and #2735 (the retry record and offers over selections)
  move the records, and each wire surface follows in its own contract unit
  (#2807, #2808). The catalog keeps `agents` and `lineup[].agent` for
  them.
- **The parked-move trust check holds only a v1 line to the agent.** The
  daemon records the moved line as the selection its tree runs the chosen
  agent with. The app's result check can't tie that selection to the name
  without trusting the catalog it holds, which may be older than the
  daemon's tree. It still checks the record's agent, the retry, and that
  every line cites the record. Rejected comparing the line with the
  catalog's selection: a tree edit between bootstrap and move would turn a
  correct result into a refused one.
- **Cache format 10 keeps the ledger.** The generated types decode
  strictly, so an old catalog would discard the whole cache, and a saved
  command whose lines or line change name an agent would drop its whole
  section. Losing a command ID risks duplicating a decision after
  relaunch. The load path drops the old catalog, which the forced
  bootstrap restores, and only the entries that name an agent, whose
  retries the daemon would refuse. A saved clear omitted the agent and
  keeps its meaning.

## Follow-ups

- Follow-up: #2807, the routing policy and escalated-line wire, after
  #2736.
- Follow-up: #2808, the alternate-agent offer, move, decision, and hold
  wire, after #2735.

Revisit when #2599 converts the v1 rows (the agent half of the wire goes),
or when the two follow-up contract units move the remaining name fields.
