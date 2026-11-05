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
//   - fake_workflow.go, fake_publication.go
//     the 1A.0 walking skeleton and the 1A.1 fake-candidate publication workflow
//   - invocation.go   dispatch of committed outbox requests into durable
//     invocations
//   - durable_transition.go the production crash-matrix registry
//   - observation.go, workflow_observation.go
//     paced observation writes, run-hold classification and recording, and
//     closed runs' publication observation
//   - legacy_vocabulary.go the pre-rename specification outbox and marker
//     identities
//   - admission.go, agent_admission.go
//     the per-stage admission environment and runner capability floor, and
//     the agent admission refusal
//   - review_admission.go review roles and the snapshot a review is admitted
//     under
//   - router.go, routing_policy.go, task_line_change.go
//     agent routing, routing policy revisions, and an operator's task line
//     change (§5.4)
//   - machine_capacity.go launch bounds under the instance's memory budget
//   - quota_park.go   the refused window's hold on a parked attempt
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
//   - finding_adjudication.go, finding_adjudication_fake.go,
//     finding_adjudication_inference.go, adjudication_park_cause.go
//     the adjudication seam, its contract fake and production adapter, and
//     the park causes it reports
//   - follow_up_filing.go follow-up issue filings from an adjudication
//   - external_finding_quote.go, external_review_cycle.go, external_review_reentry.go
//     admitted external findings as model input, and the re-entry cycle they
//     start
//   - operator_feedback.go, operator_feedback_retry.go
//     operator feedback invocations and their retries
//   - production_publication.go the production publication workflow: import
//     replay and the trust re-gate before publish
//   - publication_core.go the git publication transport, the publication
//     lock, and candidate identity
//   - publication_authoring.go, publication_render_v2.go, publication_metadata.go,
//     publication_source_issue.go
//     authored publication prose, its screening, public metadata, and the
//     source-issue text
//   - publication_closure.go, publication_closure_wait.go
//     the source-issue closure gate and the wait for a published task's closure
//   - publication_watch.go the published PR's checks, review, and base-advance
//     watches
//   - publish_block_facts.go the facts a publish-block card carries
//   - publication_continuation.go, publication_successor.go
//     publication re-evaluation and successor candidates
//   - production_reentry.go, production_reentry_rebase.go,
//     production_unpublished_reentry.go, readiness_reentry.go
//     re-earning readiness against a new base: cycles, rebase proofs, and
//     unpublished and ready-item re-entry
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
