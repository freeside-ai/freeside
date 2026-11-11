// Package domain holds the daemon's shared domain types and their validation:
// the vocabulary every lane depends on. It is spine-owned and changes only
// through kind:contract work units. It is types and validation only, with no
// storage, no I/O, and no identifier generation; the daemon assigns ids and
// sequences and the store persists these shapes.
//
// Layout, by concept:
//
//   - enums.go        named-string enumerations and their valid() checks
//   - ids.go          identifier newtypes and Digest
//   - errors.go       sentinel validation errors (wrapped with %w)
//   - clone.go        clonePtr, the detached copy of an optional field
//   - transitions.go  the legal-successor checks for stored runs,
//     conversations, attention items and deliveries, devices, pairing codes,
//     and decision surfaces
//   - legacy_vocabulary.go the pre-rename specification-run spelling and its
//     canonical mapping
//   - attention_item.go, attention_delivery.go, attention_conclusion.go
//     the attention model and how a closed item concluded (§4)
//   - decision_surface.go the daemon-owned decision surface an item presents
//     and its synchronized reference (§4)
//   - attention_item_view.go the first open of an item's decision surface,
//     the source of its unviewed mark
//   - card_facts.go   the typed facts an item's card shows: display names,
//     cost, failures, questions, publish blocks, diff stats, health
//     diagnostics, review disputes, and specification revisions
//   - recommendation.go recommendations and their provenance: daemon policy,
//     agent judgment, or project policy
//   - command.go, command_receipt.go
//     accepted client decisions on an item and operator command receipts (§4)
//   - comprehension.go the client capability contract and comprehension events
//   - agent_decision.go the decisions an agent hands back and the blocked outcome (§4)
//   - scope_conflict.go a candidate whose required work extends past its
//     approved paths
//   - artifact.go     artifacts, provenance, and the evidence gate (§5.15)
//   - evidence_metadata.go daemon-validated facts about an evidence or claim
//     reference (§5.15)
//   - export_rejection.go the write-once record of a gauntlet rejection
//   - finding.go      raw findings and versioned classification (§5.12)
//   - finding_adjudication.go, finding_adjudication_binding.go
//     the adjudication artifact, its synchronized binding, and the offered
//     routes
//   - external_finding_disposition.go the outcome for one admitted external
//     finding: fixed, declined, or deferred
//   - native_review.go forge-hosted review signals and their bounds
//   - review.go       ReviewRecord, the authority record of one review pass (§7)
//   - review_admission.go the admission snapshot of a routed or shadow review
//   - review_progress.go review request records and progress vocabulary
//   - review_recovery.go, review_config_recovery.go
//     the bindings that name one parked review failure or configuration
//     failure for recovery
//   - review_diminishing.go, drift_audit.go
//     the diminishing-returns card facts and the drift audit that parks a run (§7)
//   - review_dispute.go dispute routes and per-finding adjudication blocks
//   - review_guide.go a ready item's reading order: the agent's stops and the
//     daemon-read excerpts, bound to the item's head and claim (§7)
//   - review_yield.go per-round diff metrics and yield history (§7)
//   - shadow_review.go, shadow_review_configuration.go
//     shadow review sources and their owner-approved configuration
//   - conversation.go conversations, messages, invocations (§5.14)
//   - device.go, pairing_code.go
//     paired devices, their credential verifiers, and pairing codes (§5.14)
//   - device_notification_preference.go a device's notifications switch (§4)
//   - policy.go       resolved policy with per-key provenance (§5.12)
//   - routing_policy.go routing policy revisions and their source (§5.4)
//   - unattended.go   the unattended-operation state an operator moves
//   - update_drain.go the update drain that holds new launches for a restart
//   - workflow.go     resolved workflow-definition initiators (§5.12)
//   - run.go          run/stage/attempt and invocation identities (§5.3)
//   - observation.go  run observations: milestones, holds, invocation
//     liveness, and outcomes
//   - invocation_intent.go, remediation_intent.go, operator_feedback_intent.go
//     durable dispatch intents and the retained remediation and operator
//     feedback contracts
//   - operator_feedback_identity.go operator feedback invocation and
//     execution-failure card identities
//   - invocation_admission_facts.go the admitted facts read back for an
//     invocation and judging-role independence (§7)
//   - interpretation_failure.go why a completed invocation's output could not
//     be carried forward
//   - specification_identity.go, specification_source.go
//     specification run identities, the feedback, discussion, and approval
//     IDs derived for a specification, and what a specification assembles
//     its subject from
//   - stage_input.go  stage input snapshots and vendor instruction bindings
//   - production_attempt.go production acceptance attempts and their lineage
//   - prospective_merge_identity.go the merge a base-advance re-entry evaluates
//   - work_unit_rebase_proof.go the verdict on a head someone else pushed
//   - capability.go   runner capabilities and their persisted snapshot (§5.7)
//   - capability_manifest.go content-addressed execution environments an
//     operator may select (§5.7)
//   - conformance.go  runner backend isolation classes and the persisted
//     backend conformance record, a ceiling and never a grant
//   - agent_conformance.go what one adapter build's stage contract suite proved
//   - execution.go    the durable admission and export records (§5.3, §5.7)
//   - agent.go, agent_fragment.go, harness_effort.go
//     admitted agents, credential route fragments, and effort translation (§5.4)
//   - agent_selection.go the enrollment, model, and effort an agent stands for,
//     its digest, and the adapter a launch shape picks (§5.4)
//   - role.go         a role's launch shape, stage, and judgment sites
//   - launch.go       digest-addressed stage launch specs
//   - machine_capacity.go launch memory reservations and capacity holds
//   - wardless_admission.go wardless admission and its interim launch audit
//   - failure.go      the failure cause and disposition axes, and the pure disposition rule (§4)
//   - alternate_agent_retry.go the rule an agent must meet to stand in for a
//     failed one, the offers a failure card makes, and the operator's retry
//     record (§4)
//   - task.go, task_lifecycle.go, task_timeline.go
//     task identity, its lifecycle log and display projection, and its
//     recorded event timeline
//   - task_submission.go, manual_submission.go
//     who submitted a task and manual intake bookkeeping
//   - task_attachment.go the operator-supplied files a submission carries (§5.15)
//   - task_cancellation.go stop-task requests, targets, and receipts
//   - task_triage.go  the specifier triager's outcome (§5.13)
//   - intake_occurrence.go intake subject bindings and atomic admission
//   - task_line.go, task_line_change.go
//     the per-task agent choice, its line versions, and an operator's change (§5.4)
//   - auth_identity.go provider identities and the mutation lease (§5.4)
//   - client_enrollment.go, generation_integrity.go, codex_reenrollment_recovery.go
//     client enrollments and store generations, integrity marks, and Codex
//     re-enrollment recovery (§5.4)
//   - usage_pool.go, usage_observation.go, pool_usage_observation.go
//     provider quota pools, usage measurements, and collected pool readings
//   - discovered_model_catalog.go the models and efforts an enrollment's
//     harness reported it can run
//   - schedule.go     the durable scheduler's kinds, aggregate, and events (§5.16)
//   - work_unit.go    the §5.18 capture records: declarations, bindings, observed facts
//   - follow_up_filing.go, follow_up_filing_ledger.go
//     follow-up issue filing: its policy and bounds, and the create-attempt
//     ledger
//   - project.go      the project authority binding one repository
//   - project_image.go, admitted_image.go
//     project image toolchains and the admitted-image pointer (§5.7)
//   - image_upkeep.go, image_policy_adoption.go, rebuild_refusal.go
//     image upkeep refusals, image policy adoption, and rebuild refusal
//     clauses (§5.7)
//   - registry_set.go project registry sets and the egress profile key
//   - effect_proposal.go effect proposals: task proposals and image policy
//     adoption, and their gates
//   - trust_profile.go automation trust profile and workflow audit (§5.5)
//   - workflow_audit_evidence.go the canonical workflow-audit evidence body
//   - authorization.go candidate authorization and finding classes (§5.6, §5.8)
//   - readiness.go    readiness requirements, check proofs, and waivers
//   - handoff_check.go a publication's sealed handoff tip and merge proof
//   - publication_authoring.go, publication_continuation.go, publication_successor.go
//     authored publication prose, publication re-evaluation, and successor
//     re-entry shapes
//   - closure_approval.go, source_issue_closure.go, closure_outcome.go
//     the source-issue-closure approval binding, its re-gate, and the pure
//     outcome matrix mapping a proposal and its approval to reference/hold/item (§5.13)
//   - backup.go       encrypted backup checkpoints and backup health (§5.10)
//
// See docs/plan.md §4 (the attention model), §5.3 (execution identities),
// §5.12 (findings and policy resolution), §5.14 (conversations and sync), and
// §5.15 (evidence and provenance).
//
// # Conventions
//
// These patterns are set here for every later lane to copy (recorded for spine
// review in the Wave 0 domain devlog entry):
//
//   - Enums are named string types with a valid() predicate and an AllX slice;
//     the zero value ("") is invalid by design and rejected by Validate.
//   - Invariants are enforced at the validation boundary, not by hiding fields:
//     types keep exported, json-tagged fields so encoding/json serializes them
//     for free (golden tests, store/api round-trips), and constructors plus
//     Validate reject illegal states.
//   - Fields that must never come from a caller (an artifact's publish_eligible,
//     a message's sequence, an item's derived timing) are omitted from the
//     input/request struct, so no input path can set them; they are computed or
//     daemon-assigned and exported only on the output type.
//   - Immutability is value semantics plus the absence of mutators; a correction
//     is a new value with an incremented version, never an in-place edit.
//   - A validity switch uses default; a switch that dispatches behaviour on an
//     enum omits default so the exhaustive linter forces new members to be
//     handled (see artifact.go).
package domain
