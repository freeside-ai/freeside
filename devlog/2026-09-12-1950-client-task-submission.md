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

## Implementation

Decisions the code forced during #1328, kept here because they rejected the
plan's stated approach or a reasonable alternative.

- **The engine-side submission is injected into signet, not called from it.**
  The plan had the `submit_task` handler call `engine.SubmitSpecificationRunTx`,
  `engine.ProductionPublication`, and `engine.ProductionCommitAuthor` directly.
  That is a build cycle: the engine imports signet (`signet.BlobStore`,
  `signet.ErrDigestMismatch`), so signet cannot import the engine, and the
  publication and commit-author types are engine-owned. Instead signet defines
  a `TaskSubmitter` interface returning domain types and a `WithTaskSubmitter`
  option; `engine.TaskSubmitter` implements it and runs inside the accepting
  transaction. It needs no `*engine.Engine`, so the composition builds it
  before the engine is wired. The per-project policy resolver lives with the
  engine submitter (its commit author is an engine type), so there is no
  `WithManualInitiator` on signet as the plan proposed. The submission
  primitives (`SubmissionRunID`, `SubmissionArtifact`,
  `RegisterSubmissionArtifact`, `SubmittedPathBoundary`, `DeclaredPathScope`)
  moved up from `cmd/freesided` into the engine so both `freesided submit` and
  the injected submitter share them.
- **A separate record and table, not the decision command's.** A
  `domain.Command` requires an item id and a positive item version, and the
  `commands` table's `item_id` is `NOT NULL REFERENCES attention_items`; a task
  submission has neither. It gets its own `domain.TaskSubmission`, migration
  0073's write-once `task_submission_commands`, and cross-kind `command_id`
  uniqueness enforced by each table's `Put` probing the other, so one id never
  names both a decision and a submission.
- **The envelope fields are optional on the wire, discriminated by kind.**
  `expected_entity_version` and `expected_bindings` left `ClientCommand`'s
  required set; a decision command still carries both, a `submit_task` command
  carries neither and is rejected as malformed if it does. The HTTP boundary
  probes the payload `kind` from raw JSON before the strict per-arm decode,
  because `strictjson` rejects the unknown `kind` field otherwise.
- **A reused `command_id` across kinds returns 400, not 409.** The plan said
  409. The existing decision path already maps `store.ErrImmutableConflict` to
  400 (`isCommandRequestError`); keeping 400 for the cross-kind conflict avoids
  a behavior change to decisions and a split surface. The contract calls it an
  immutable conflict without pinning the status.
- **Deployment boundary confirmed.** The injected initiator is populated from
  `cfg.IntakeInitiators`, empty on the host until the rein resolver lands, so
  client submission is refused there and works in test compositions. This
  matches the boundary the Revisit-When entry for #1332 already records.

## Review (refute-first, trust boundary)

A fresh-context adversarial pass over the returned-object trust boundary and
the idempotency/cross-kind logic found no correctness or security defect;
idempotency (all four cases), cross-kind `command_id` uniqueness (both
directions), the source-binding trust gate, error mapping, and the
`specification.go` refactor equivalence were confirmed. Dispositions of its
low findings:

- **Fixed:** `GetTaskSubmissionSnapshot` now re-runs `TaskSubmission.Validate`
  on the decoded body, so the body-only source digest and name are re-gated at
  reconstruction like the column-checked identity fields.
- **Fixed (comment):** the pre-transaction source-blob put is content-addressed
  and idempotent; a rolled-back submission leaves a harmless orphan blob, never
  a durable store effect. The device gate's "before any write" holds for every
  committed row. This matches the `freesided submit` / attachment pattern.
- **Allowed:** an idempotent replay returns the recorded record without a
  requesting-device *revocation* check. This is the decision-command semantics
  (§5.14 test 16), not new here; client `command_id`s are random UUIDs, so a
  cross-device read requires guessing one. (The replay still rejects a reused
  id whose device, project, or source differs; see the Codex pass below.)

### Codex review pass

The automated reviewer raised four P2 findings; dispositions:

- **Fixed:** `submitTask`'s command-id replay rejects a reused id whose device,
  project, or source digest differs from the recorded submission with
  `store.ErrImmutableConflict` (400) instead of silently returning the first
  task. Only the optional name is ignored, matching the decision path (where
  `PutCommand` surfaces the conflict from the rebuilt body) and the mock. The
  fast replay returned before `PutTaskSubmission` would have caught it, so the
  guard lives in the replay branch.
- **Fixed:** each command-union arm (`DecisionPayload`, `SubmitTaskPayload`,
  `CommandRecord`, `TaskSubmissionRecord`) constrains `kind` to a singleton
  enum rather than the shared `CommandKind`, so the generated types cannot
  carry a discriminator that disagrees with the arm and get mis-routed at
  decode. `CommandKind` stays as the registration seam pinned to
  `domain.AllCommandKinds`.
- **Fixed:** the mock's `materializeSubmittedTask` projects a self-consistent
  task (active lifecycle, position at the run, run and campaign membership, a
  started fact) and a run-submitted run timeline, so read-your-write on the
  submitted task's timeline resolves instead of throwing `InvalidTaskError`.
- **Declined:** re-reading the referenced task, run, and source to revalidate
  their project, membership, and digest relationships at
  `GetTaskSubmissionSnapshot`. The write-once row is produced only from
  validated, submitter-created entities, so no interface or caller path yields
  a mismatched row; only direct database tampering does, and an attacker with
  row-write access already controls the store. The returned record is a
  read-your-write reference, not a capability trust bit, so the reconstruction
  re-gate convention (publish_eligible, recipe approval) does not apply; the
  existing column/body cross-check and re-run `Validate` already fail a corrupt
  row closed.

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
