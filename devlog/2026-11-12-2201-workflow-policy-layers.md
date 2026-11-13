# Workflow Policy Layers in the Store

Issue #3145 moves the gates and the follow-up filing caps out of the
manual submission config file and into two store layers, set from the app's
Workflow pane. This note records the choices the unit made where the plan
comment left room or where the code argued for a different shape.

## Decisions

- **Chose an append-only revision table over a current-value table,**
  because a revision is what a stale `expected_revision` compares against
  and what the pane cites as "set by, at". The current layers are the
  newest row per scope, project, and key; a cleared layer is a row with a
  null value. The table is add-only (migration 0138), so the unit stays
  Tier B on the store side.
- **Chose one policy revision number over a revision per key.** Every
  change advances it, so a client's `expected_revision` refuses a change
  prepared against any older state of any key. A finer revision would let
  two devices change different keys without a conflict, but the pane saves
  one control at a time and a 409 costs one retry, so the coarser rule is
  simpler to reason about.
- **Chose to resolve at task creation over resolving at run start.** The
  paired-client submission and label intake overlay the layers on the
  project's file keys (`WorkflowPolicyLayers.Overlay`) and store the result
  as the run's resolved policy, with `project`, `general`, or `preset` as
  each key's provenance. A run already created keeps its policy, and the
  run identity digest covers the overlaid keys, so two tasks created under
  different layers get different identities.
- **Chose to reuse the stored keys on intake crash recovery over
  re-resolving.** When intake finds a resolved policy already stored for
  the specification run, it takes the exposed keys from there. Re-resolving
  could pick up a change made between the crash and the restart and produce
  a run whose identity no longer matches its stored policy.
- **Chose to drop an exposed key from the file with a warning over
  refusing to start.** A config written before this unit still loads; the
  store layers win and the operator sees which key was ignored. Label
  intake's config gets no warning: its keys are overlaid the same way, so a
  stale file value never takes effect there either.
- **Chose canonical value strings over typed values on the wire.** A gate
  is exactly `true` or `false` and a cap a positive decimal that survives
  an integer round trip. The readers already parse strings from the
  resolved policy, so canonical strings keep one representation from the
  command to the run.

## Departures From the Plan Comment

- No `WorkflowPolicySource` enum. It would have one member; `set_by`
  (`device:<id>`) already says who set a layer.
- The receipt echoes the request and carries the policy revision after the
  change, not a whole policy object. The bootstrap is the one place the
  layers are projected.
- The snapshot's `revision` is an integer from 0 (nothing ever set), not an
  entity version, whose minimum is 1.
- Project membership is checked once, in signet, which holds the served
  project list. The engine setter trusts its caller on that point.

## Finding: The 409 Arms Decode by Order

The 409 body's `CommandRejection` arms carry no discriminator, and the
generated Swift client takes the first arm that decodes. The rejection's
field is named `current_workflow_policy` so it differs from routing's
`current_policy`, but that alone did not separate them: `current_policy` is
required and nullable, and the generated client decodes a missing nullable
field as null. A workflow refusal therefore decoded as a routing refusal
and the pane treated it as unanswered. The workflow arm now precedes the
routing arm, and the schema's description states the rule: a required
nullable field counts as absent when ordering arms. A test decodes a
workflow refusal and checks its arm.

## Refute-First Findings

A fresh-context reviewer tried to break the command gate, the store's
revision order, resolution, intake recovery, and the app's pending change.

- **Disproved:**
  - **Validation before writes.** Every refusal (shape, unserved
    project, stale revision) happens before the append, and a stale
    revision's 409 reads its projection inside the rolled-back
    transaction.
  - **Replay.** A replay returns the original receipt before the unserved
    check runs, and a reused command id from another device or with
    another payload is refused.
  - **One writer per revision.** Two writers can't take one revision: the
    immediate transaction lock serializes them and the primary key
    backstops it.
  - **No exposed key from the file.** On neither path does one reach a run.
  - **Identity matches the stored policy.** The run identity digest
    equals the stored policy's digest.
  - **App lifecycle.** The pending change is persisted before send and
    retried only in its own sync epoch.
- **Allowed by decision, flagged for the owner: an upgrade can loosen a
  gate.** A file that set `gates.source_issue_closure` to `true`, or a cap
  below its default, loses that value: the loader drops the key and the
  layers start empty, so the default (autonomous, caps 10, 5, 2) applies
  until someone sets it in the pane. The issue contract chose
  drop-and-warn. Plan §3.2's "a typo never removes a person" covers a
  value that doesn't parse, not a key moved out of the file. The
  warning now carries the dropped value and says to set it in the pane.
  The alternative is to seed each project's layer from its file once, on
  the first start with empty layers; that is a daemon write at startup
  the contract didn't ask for, so the choice stays with the owner.
- **Fixed:** the pane offered Use general on a key this app can't decode,
  which the daemon would refuse; it no longer does.
- **Latent, left:** the daemon serves the project list from the manual
  submission file, so a label-intake project absent from it would get no
  project section and only the general layer. No intake initiator is
  configured in production yet.

## Revisit When

- The daemon serves several repositories and the pane needs a project
  picker rather than one section per configured project.
- Label intake gains a project the manual submission file doesn't list;
  the served project list must then include it.
- `freesided submit` resolves the layers too. Follow-up: #3183.
