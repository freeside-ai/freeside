# Task as the Unit of Requested Work

Chose "task" as the single name for a requested piece of work, defined once
in `docs/plan.md` Section 1 (plan revision 47), over "work item", the plan's
prior term, and over merging a task's runs into one run. The user decided it
after an assessment of why work is hard to recognize in the inbox and runs
screens.

## Finding

The clients had no name for a piece of work anywhere in the stack. A run's
only labels are the daemon-derived `display_names`: the repository for the
project and either `#<issue>` or the raw `run-<sha256>` identifier for the
work unit (`daemon/internal/store/display_names.go`). The runs row is titled
by stage and round (#1111), so two pieces of work in one project read alike.
A specification run, its implementation run, and each retry attempt are
separate content-addressed runs linked only by derivation, `superseded_by`,
and the campaign fields, so one piece of work appears as several unrelated
rows. The inbox row renders a per-type title and a per-type canned sentence
and drops the item's `reason` entirely.

## Decisions

- **One term at every layer.** Task in navigation, documentation, and CLI
  language; `Task` and `task_id` in the domain and API. Rejected: keeping
  "work item" in the domain with a friendlier label in the app, because two
  names for one entity leak into every card, error, and doc.
- **"Task" over "work item".** Work item is accurate but clunky as a
  navigation label, and "work unit" already names both the coordination
  vocabulary in AGENTS.md and the per-run `WorkUnitDeclaration`, so a third
  "work" noun invites confusion. Task carries a step-sized connotation in
  some agent tooling; the plan's definition fixes its scale explicitly to the
  whole intake-to-completion lifecycle.
- **Runs stay runs; the task is a layer above them.** Run identity is
  content-addressed and approvals bind to specification digests, so
  collapsing a task's runs into one run would break the durability model.
  A campaign (Section 5.12) groups implementation attempts against one
  unchanged approved specification; a task spans campaigns as its approved
  specification changes, so the campaign is execution history rather than a
  fourth concept the clients lead with. `freesided reattempt --campaign`
  (Section 10) keeps naming one, so the term stays in the operator CLI.
- **An attention item optionally references its task.** Section 4's subject
  record gains `task_id?`; system-health and project-scoped items have none,
  which the app's row context already handles by omitting the work-unit
  segment.
- **Where the name comes from is a contract decision, not settled here.**
  Section 9 requires agent prose to render as a labeled claim, and the claim
  contract has no inline-text carrier yet. The daemon already extracts the
  approved specification's first heading as the commit subject
  (`daemon/internal/engine/commit_message.go`), which is the cheapest
  digest-bound name source; the specifier's summary is too long for a title.
  The implementing contract unit chooses the source order and the
  `DisplayNameSource` widening.

## Revisit When

- The task contract unit lands and the clients show a task name: check that
  the runs-list title decision in #1174 (stage and round as title) has moved
  to the row's second line rather than competing with the name.
- "Task" collides with a stage-level or invocation-level concept in an
  adopted harness vocabulary; the plan's definition should win, or be
  revised by its own revision.

Follow-up: #1203 (the task contract unit) and #1204 (the CLI flag and docs
prose rename).
