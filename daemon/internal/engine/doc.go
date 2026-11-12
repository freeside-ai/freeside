// Package engine owns the durable workflow state machine. It composes the
// store, signet, and StageDriver boundaries; none of those packages needs to
// know the workflow that uses it.
//
// The Phase 1A walking skeleton is deliberately concrete: a fake run creates
// an approval item, an accepted approval opens a conversation-feedback item,
// and a fake invocation records one attempt before it starts, then completes
// the conversation from that durable recovery index. Reconcile derives every
// step from durable state, so retry and restart use the same path as first
// execution.
//
// Layout, by concept:
//
//   - engine.go       Engine, its options, and the Reconcile and Run loops
//   - fake_workflow.go the 1A.0 walking skeleton
//   - legacy_fake_publication.go what outlives the retired 1A.1 fake-candidate
//     publication lane: its outbox codecs, backup extractors, and the legacy
//     run-policy converge
//   - invocation.go   dispatch of committed outbox requests into durable
//     invocations
//   - hold_classification.go the one table that classifies a dispatch error
//     as a hold or refusal, with its reason and admission check
//   - durable_transition.go the production crash-matrix registry
//   - observation.go, workflow_observation.go
//     paced observation writes, run-hold details and recording, and closed
//     runs' publication observation
//   - legacy_vocabulary.go the pre-rename specification outbox and marker
//     identities
//   - admission.go, agent_admission.go
//     the per-stage admission environment and runner capability floor, and
//     the agent admission refusal
//   - review_admission.go review roles and the snapshot a review is admitted
//     under
//   - host_verification_admission.go the gate that admits a run's
//     verification job onto the credential-free macOS executor (§5.7)
//   - host_verification_slots.go the notice on a run whose macOS executor
//     has every slot quarantined, and its retirement
//   - router.go, routing_policy.go, task_line_change.go
//     agent routing, routing policy revisions, and an operator's task line
//     change (§5.4)
//   - specifier_triage.go the triager's one call per task, before the
//     specifier's first admission, and the line an escalation writes (§5.13)
//   - machine_capacity.go launch bounds under the instance's memory budget
//   - quota_park.go   the refused window's hold on a parked attempt
//   - daemon_rerun.go the next attempt of a root implementation invocation
//     the daemon's own restart, shutdown, or forced drain ended (#3105)
//   - update_drain.go, running_work.go
//     the update drain's request, cancel, lift, and force, and the count of
//     in-process work a restart would cut short
//   - capability_manifest.go, capability_retry.go
//     the capability choices a failed writer card offers and their retries
//   - alternate_agent.go, alternate_agent_retry.go
//     the other agents a provider failure card offers, and the operator's
//     retry or parked-task move that runs one (§5.4)
//   - held_work_notice.go the advisory notice for a long refusal hold
//   - vendor_instruction.go, review_instruction.go
//     snapshots of the operator-host instruction files a writer or review
//     admits
//   - task_submission.go, submission.go
//     client task submission and its declared-path boundary
//   - proposal_admission.go admitting one proposal occurrence and its task
//   - task_namer.go, display_names.go
//     task names and the display names cards carry
//   - task_execution.go, task_cancellation.go
//     task-fenced invocation resume and recovery, and task cancellation
//   - task_completion.go, task_abandonment.go, closure_settlement.go
//     retiring a task's items on completion, closure, or settled closure
//   - attention_discussion.go conversation intents on attention items
//   - specification.go, specification_discussion.go, specification_retry.go
//     the specification workflow, its discussion, and its retry
//   - production_workflow.go the production lane's dispatch intents
//   - production_attempt.go a deliberate new implementation attempt
//   - production_question.go a blocked implementation's agent question
//   - production_scope_conflict.go parking a candidate whose work exceeds its paths
//   - execution_failure.go the facts and claims a failed invocation's card carries
//   - usage.go, claude_usage.go
//     usage observations, the card's spend figure, and the Claude usage sink
//   - summary_claims.go the renderable summary claim of an invocation
//   - commit_message.go the daemon-authored fallback commit message
//   - diff_stats.go   diff stats from a stored patch
//   - review_guide.go the ready item's review guide, resolved from the
//     producing agent's claim at the verified head and base
//   - production_review_gate.go whether a reviewed candidate may reach
//     publication
//   - shadow_review.go the observation-only shadow review arm
//   - remediation.go  remediation dispatch and its review outcome
//   - review_round_metrics.go, review_yield.go
//     per-round diff metrics and the yield history derived from them (§7)
//   - review_diminishing.go, drift_audit.go
//     the diminishing-returns park and the round's drift audit (§7)
//   - review_dispute.go, per_finding_acceptance.go
//     review dispute decisions and per-finding routes awaiting a person
//   - finding_adjudication.go, finding_adjudication_inference.go,
//     adjudication_park_cause.go
//     the adjudication seam, its production adapter, and the park causes it
//     reports
//   - follow_up_filing.go follow-up issue filings from an adjudication
//   - external_finding_quote.go, external_review_cycle.go, external_review_reentry.go
//     admitted external findings as model input, and the re-entry cycle they
//     start
//   - operator_feedback.go, operator_feedback_retry.go
//     operator feedback invocations and their retries
//   - production_publication.go the production publication workflow: the
//     reconcile loop, the binding re-gate, readiness, completion, and PR
//     bindings
//   - production_publication_task.go publication task keys and the restart
//     reconstruction of a stored task row
//   - production_publication_progress.go how far a forward-route task got
//     before this pass, read from its checkpoint, ready item, and intent
//   - production_publication_completion.go authenticating a publication's
//     terminal and ready records
//   - production_export_record.go committing the trusted export and the
//     task that replays it
//   - production_publication_outcome.go recovering and authorizing a
//     committed publication outcome
//   - production_verification.go running verification under an approved
//     recipe and checkpointing its verdict
//   - verification_memory_held.go the end-of-task proof of a held
//     verification reservation, its alert, and the operator's release
//   - production_remediation_source.go authenticating a remediation's source
//     tree
//   - production_publication_holds.go a publication task's run holds and
//     retry pacing
//   - publication_core.go the git publication transport, the publication
//     lock, and candidate identity
//   - publication_authoring.go, publication_render_v2.go, publication_metadata.go,
//     publication_source_issue.go
//     authored publication prose, its screening, public metadata, and the
//     source-issue text
//   - publication_closure.go, publication_closure_wait.go
//     the source-issue closure gate and the wait for a published task's closure
//   - publication_watch.go the published PR's checks and review watches
//   - publish_block_facts.go the facts a publish-block card carries
//   - publication_continuation.go, publication_successor.go
//     publication re-evaluation and successor candidates
//   - production_handoff.go the handoff check: sealing the target's tip,
//     verifying the head's merge into a moved tip, and retiring queued
//     pre-publication re-entries
//   - production_reentry.go, production_reentry_rebase.go, readiness_reentry.go
//     re-earning readiness against a new base: cycles, rebase proofs, and
//     ready-item re-entry
//   - verification_report.go reading the verifier's immutable report bytes
//   - image_selection.go, image_upkeep.go, image_upkeep_adoption.go, image_repair_task.go
//     project image selection, upkeep at the tip, policy adoption cards, and
//     repair runs (§5.7)
//   - production_rebuild.go, project_image_build_gate.go
//     candidate image rebuilds and the one-build-at-a-time gate
//   - backup_payloads.go the outbox blob extractors the backup checkpoint reads
//
// See docs/plan.md §5.3 (execution), §5.12 (workflow definition), §5.14
// (conversations and effectively-once acceptance), and §11 (the 1A.0 exit).
package engine
