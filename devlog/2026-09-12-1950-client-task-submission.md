# Client Task Submission and the Sketch Round

Chose to let a paired client start a task through the existing command
surface, and to make the specification stage the sanity check for a rough
idea, instead of adding a conversation before a task exists. The user
decided it after noticing that the apps can decide on work but never start
it; plan revision 58 records it in Sections 5.11 and 5.14. Units: #1328 (the
`submit_task` command contract), #1330 (the composer), #1329 (the
specifier's sketch round).

## Finding

Plan revision 57 defined three intake paths (Section 5.11): a labeled
issue, a scanner proposal, and manual submission through `freesided submit`
on the daemon host. The clients were scoped as a decision surface: the
attention inbox, the run list, the discuss composer. No plan text, app
surface, API operation, or open issue gave a paired device a way to author
a task, and the Tasks rollout tracker (#1211) replaced the Runs tab with
task detail without an entry point for a new one. The gap was a scoping
decision that nobody revisited once the CLI worked, not a deliberate
exclusion.

The back-and-forth the user wanted before a task is settled already exists
on the far side of the submit button. A submission starts a specification
run, not implementation; the specifier already has a `decisions` output
form for blocking owner questions, and the daemon surfaces it as the
`agent_question` card with answer-and-retry, answer-without-retry, and
stop. What was missing was an entry point from the app and a rule that a
thin source gets questions before a specification.

## Decisions

- **A `submit_task` `ClientCommand` over a new endpoint.** Section 5.14
  names `POST /commands` as the single mutation surface for client
  decisions, with pairing, revocation, attachment upload, and the
  delivery-opened receipt as the only exceptions. Task submission is a
  consequential client mutation, so it is the second command type, and it
  inherits `command_id` idempotency, device authority, and read-your-write
  for free. The command binds to no entity, so it carries no
  `expected_entity_version` or `expected_bindings` and has no
  replacement-state rejection; the project-scoped intake key is its only
  concurrency control. Rejected: a dedicated `POST /tasks`, which would have
  duplicated those guarantees in a second place, and binding the project's
  entity version, which no API entity supplies.
- **One intake path, not a client-flavored one.** The command reuses
  `freesided submit`'s path and key (`(project_id, source_digest)`), so a
  task typed on the phone and one submitted from a file have one identity,
  one idempotency rule, and one specification workflow. The optional
  operator name stays outside the key: it applies only when the command
  creates the task, and a fetch of an existing task ignores it and returns
  the stored name, so the first name wins and the client can see that.
  The client supplies neither the resolved policy nor the publication
  metadata the CLI takes as files: the daemon resolves the project's
  configured policy at submission and composes the publication itself, and
  records both with the reserved identity, so a later configuration change
  moves no existing task. It also records a `bound_pr_merged` work-unit
  declaration with declared paths from the resolved policy, the default
  label intake already creates (`intake_reconcile.go`), so a merged PR
  completes the task and releases its WIP slot. Rejected: a separate client task kind, which
  would have made "task" mean two things; rejecting a name mismatch, which
  would break idempotent convergence over a display field; putting the name
  in the key, which would make one source two tasks; and client-supplied
  policy or publication, because a device has command authority, not policy
  authority, and a submit-time override is a revisit condition below.
- **The specification stage is the sanity check; no pre-task chat.** A
  conversation with no task underneath has no item version to bind
  decisions to, no run to attribute cost to, no ledger entry, and no
  idempotency key, and whatever it settles must become the source artifact
  anyway. Instead the task is the cheap thing and the specification the
  negotiable thing. Rejected: an advisory conversation site outside a task,
  and routing app submissions through `task_proposal` (start,
  start-with-changes, decline), which exists for scanner proposals and would
  add a fork for less benefit than the questions round.
- **The sketch round is prompt guidance, not a flag.** A source that leaves
  outcome, scope, or non-goals unresolved is a sketch, judged by the
  specifier from the source rather than by its length or section headings;
  on a sketch the specifier returns `decisions` on its first turn before
  research or a specification. This reuses the existing output form and the
  `agent_question` card, so no field, attention type, or validation changes.
  Rejected: a submit-time `sketch` flag or source kind, because the
  specifier can read the source's shape and a flag would make the operator
  classify their own idea. Rejected: a hard rule that the first turn must be
  `decisions`, because an unambiguous sentence deserves a specification
  directly. Rejected with it: defining a sketch by missing sections, which
  would have classified that sentence as a sketch and forced the rejected
  rule in by the back door. Cost bound: on a sketch, nothing is fetched
  before the operator answers.
- **Attachments on submission wait.** The discuss composer's upload is
  still "Not yet" in `app/SURFACES.md`; the submit composer takes text only
  until that path exists.

## Revisit When

- Guidance alone proves insufficient: the specifier keeps writing full
  specifications for one-line sources, or asks questions of complete
  documents. Then add the `sketch` source kind rejected above, set by the
  composer.
- The composer needs attachments; the upload path from the discuss composer
  is the prerequisite.
- #1332 lands the configured-project list in `/sync/bootstrap`. Until then
  the composer offers only the projects the client already knows from synced
  entities, and a project's first task comes from `freesided submit` on the
  host; drop that boundary when #1332 merges.
- Stopped sketches accumulate against a WIP cap: a stop ends only the
  specification run and the task keeps its slot. #1318 decides whether a
  stop before any specification records abandonment and adds the operator
  abandon action.
- Operators want to steer a task before its specification run starts (for
  example, a project-policy override at submit time). That would reopen
  the pre-task conversation question with a concrete need.
