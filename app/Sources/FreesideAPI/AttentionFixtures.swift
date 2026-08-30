import Foundation

/// Deterministic, schema-valid attention items: one per Phase 1 attention
/// type, with `requested_decision` transcribed from plan §4's per-type
/// action table. Built in Swift from the generated schema types (no JSON
/// resources or decode step), so a schema change breaks these at compile
/// time.
public enum AttentionFixtures {
    /// The recipe digest every fixture's evidence is produced under;
    /// MockServer's default approved set contains exactly this digest.
    public static let approvedRecipeDigest = "sha256:recipe-approved"

    /// A fixed creation instant shared by the seeded inbox so screenshots and
    /// cache tests never depend on the host clock.
    public static let createdInstant = Date(timeIntervalSince1970: 1_767_323_045)

    /// The Phase 1 attention types, in the schema's enum order.
    public static let phase1Types: [Components.Schemas.AttentionType] = [
        .spec_approval,
        .execution_failure,
        .agent_question,
        .review_diminishing_returns,
        .review_dispute,
        .review_contradiction,
        .review_configuration,
        .finding_adjudication,
        .ready_for_final_review,
        .publish_blocked,
        .run_proposal,
        .system_health,
        .blocked,
    ]

    /// Plan §4's per-type action sets (docs/plan.md §4 "Actions"; approve
    /// is not universal), matching signet's authoritative
    /// allowedActionsByType policy. `blocked` is read-only: the policy pins
    /// it to no actions, and the schema permits the empty set (#96).
    public static let phase1ActionSets: [Components.Schemas.AttentionType: [Components.Schemas.Action]] = [
        .spec_approval: [.approve, .request_changes, .discuss, .stop],
        .execution_failure: [.retry, .retry_with_capabilities, .discuss, .stop],
        .agent_question: [.answer_and_retry, .answer_without_retry, .stop],
        .review_diminishing_returns: [
            .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
        ],
        .review_dispute: [.approve, .discuss, .stop],
        .review_contradiction: [.recover_review],
        .review_configuration: [.adopt_review_configuration, .discuss, .stop],
        .finding_adjudication: [
            .accept_recommended_route, .choose_alternative_route, .discuss, .stop,
        ],
        .ready_for_final_review: [.open_pr, .return_to_agent, .mark_seen, .dismiss, .stop],
        .publish_blocked: [
            .rerun_trust_evaluation, .choose_alternate_profile, .inspect_trust_failure, .stop,
        ],
        .run_proposal: [.start, .start_with_changes, .decline, .snooze],
        .system_health: [
            .acknowledge, .run_doctor, .stop_unattended, .resume_unattended,
            .resolve_reenrollment,
        ],
        .blocked: [],
    ]

    /// Every Phase 1 action, in the schema's enum order: the Swift analogue of
    /// Go's `domain.AllActions`, hand-authored like `phase1Types` rather than
    /// derived. The cross-language policy-parity suite enumerates each type's
    /// *disallowed* complement against this list, so a daemon that reassigned an
    /// action would surface; `FixtureTests` guards it against the union of
    /// `phase1ActionSets` so it cannot silently omit a member.
    public static let phase1Actions: [Components.Schemas.Action] = [
        .approve, .request_changes, .discuss, .stop,
        .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
        .retry, .retry_with_capabilities,
        .answer_and_retry, .answer_without_retry,
        .rerun_trust_evaluation, .choose_alternate_profile, .inspect_trust_failure,
        .open_pr, .return_to_agent, .mark_seen, .dismiss,
        .start, .start_with_changes, .decline, .snooze,
        .acknowledge, .run_doctor, .stop_unattended,
        .resume_unattended, .recover_review, .adopt_review_configuration,
        .resolve_reenrollment,
        .accept_recommended_route, .choose_alternative_route,
    ]

    /// The default mock inbox: one open item per Phase 1 type.
    public static func defaultInbox() -> [Components.Schemas.AttentionItemSnapshot] {
        phase1Types.map { fixture(type: $0) }
    }

    /// The deterministic thread carried by the default spec-approval card.
    /// Other discuss-capable cards acquire a conversation on first submit.
    public static func defaultConversations() -> [Components.Schemas.ConversationSnapshot] {
        let id = "conv-item-spec_approval"
        return [
            .init(
                as_of_revision: 1,
                entity_version: 1,
                conversation: .init(
                    id: id,
                    status: .idle,
                    messages: [
                        .init(
                            id: "msg-user-fixture",
                            conversation_id: id,
                            sequence: 1,
                            author: .user,
                            body: "Can the revised spec preserve the existing migration order?",
                            attachments: [],
                            created_at: createdInstant.addingTimeInterval(60)
                        ),
                        .init(
                            id: "msg-agent-fixture",
                            conversation_id: id,
                            sequence: 2,
                            author: .agent,
                            body: "Yes. The revision keeps the order and narrows the rollback step.",
                            attachments: [],
                            created_at: createdInstant.addingTimeInterval(120)
                        ),
                    ]
                ))
        ]
    }

    /// A ready fixture carrying the other valid readiness class. The default
    /// inbox remains clean; tests and screenshots can opt into this one to
    /// prove degraded readiness survives sync and renders distinctly.
    public static func degradedReady() -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = fixture(type: .ready_for_final_review)
        snapshot.item.id = "item-ready_for_final_review-degraded"
        snapshot.item.readiness = .init(
            value1: .init(
                _class: .ready_degraded,
                evaluation_set_digest: "sha256:evaluation-degraded"
            ))
        return snapshot
    }

    /// The bytes behind the default inbox's attachment digests, for the
    /// mock's digest-addressed read path (plan §4: cards render image
    /// attachments directly from the artifact store by digest). Every
    /// `log-` evidence digest resolves to text (a non-image attachment
    /// keeps its plain digest row) and every `img-` claim digest to the
    /// fixture PNG — except `blocked`'s, deliberately unseeded so one
    /// default card exercises the missing-attachment placeholder.
    public static func defaultAttachments() -> [String: Data] {
        var bytes: [String: Data] = [:]
        for type in phase1Types {
            let key = type.rawValue
            bytes["sha256:log-\(key)"] = Data("verify log for \(key)\n".utf8)
            if type != .blocked {
                bytes["sha256:img-\(key)"] = fixtureImagePNG
            }
        }
        return bytes
    }

    /// A small deterministic PNG (320×200 gradient, metadata stripped),
    /// embedded so the platform-portable FreesideAPI target needs no
    /// bundle resources or image frameworks to serve fixture bytes.
    // swift-format-ignore: NeverForceUnwrap
    public static let fixtureImagePNG = Data(
        base64Encoded:
            "iVBORw0KGgoAAAANSUhEUgAAAUAAAADIEAIAAABG9nO/AAAEfUlEQVR42u3dsa0dOQwFUBogsI24DFfgcBbYuoz/+hrF7sCxG+AG0wNvMOdUoEwgRV59+/nz16/fvwsAWNR16qv+TR8DAN6l58zXuIABYFXXqY8LGAB2dZ3RggaAZV2ntKABYFmXN2AAWNdz10cLGgB2aUEDQIAWNAAEPEEcV/oYAPAuKmAACOgRxAEA6wRxAECAKWgACHABA0BA1xlBHACwTAUMAAH+AwaAgCeIwwUMAKu67rEHDADLvAEDQIALGAACeiRhAcA6FTAABHQdQ1gAsM0aEgAE+A8YAAJ6Tn3NlT4GALxL1ymfMQDAMi1oAAiwhgQAAV2COABg3TOE5QIGgFV+QwKAAEEcABBgCAsAAqwhAUCAIA4ACOhRAQPAOm/AABAgiAMAAlTAABDQdUoQBwAs69GCBoB1WtAAEPAEcVzpYwDAu3TdgjgAYJsoSgAI8B8wAAT4jhAAArrO2AMGgGXWkAAgQBY0AAQYwgKAAGtIABDgMwYACLCGBAABWtAAEGAICwACuu7xGQMALBPEAQAB3oABIMAUNAAEPC3oK30MAHiXHr8hAcA6LWgACDCEBQAB1pAAIKDrCOIAgG2iKAEgQAsaAAK6zpiCBoBlXXfZAwaAZdaQACCgRxAHAKxTAQNAgCloAAjoOiWIAwCWaUEDQIAkLAAIEMQBAAFdpz5zpY8BAO/iDRgAAroEcQDAOkNYABCgBQ0AAV23IA4A2KYCBoAAWdAAECCIAwACek59VMAAsEsLGgACDGEBQIAkLAAIUAEDQIAhLAAIsIYEAAGmoAEgwAUMAAFd93zqSh8DAN7Ff8AAEGANCQACBHEAQEDXGXvAALDMFDQABPRoQQPAOkNYABCgBQ0AAV1nPlrQALBLBQwAAd6AASDAFDQABHTdJYgDAJZpQQNAgCxoAAhQAQNAgP+AASDgCeK40scAgHcRxAEAAS5gAAjoOmMKGgCW9RxBHACwzRoSAAQI4gCAABUwAASYggaAgB6/IQHAOmtIABCgBQ0AAYawACCg69RHCxoAdvWogAFgnTdgAAiQhAUAAV1n7AEDwLKnBX2ljwEA7yKIAwACegxhAcA6QRwAENB1C+IAgG32gAEgQAsaAAJ6BHEAwDpBHAAQ4A0YAAIEcQBAgAoYAAIkYQFAQNcZQRwAsEwLGgACBHEAQECXIA4AWNdzC+IAgG3PG/CVPgYAvIsWNAAEGMICgICuU96AAWCZLGgACBBFCQAB3oABIMAUNAAEGMICgAAtaAAIMIQFAAHWkAAgoOv2BgwA27wBA0CANSQACDCEBQABWtAAENB16qMFDQC7ngr4Sh8DAN6lyxswAKzrEcQBAOt8xgAAAVrQABAgCxoAAlTAABDQI4gDANZ13YI4AGCbKEoACPAGDAABviMEgICuM4I4AGCZ/4ABIEAQBwAEGMICgABvwAAQYAoaAAKeIawrfQwAeBdJWAAQoAUNAAFdtyEsANhmDQkAAr59//Pj73//pI8BAO+iAgaAgK5T3oABYJksaAAI0IIGgID/AXOWWIKW1YGjAAAAAElFTkSuQmCC"
    )!

    /// The default inbox's item ids, in inbox order: the canonical value
    /// list for the `-FreesideSelect` launch argument. The "Running"
    /// section of app/README.md mirrors this list for capture workflows;
    /// keep them in sync.
    public static func defaultInboxItemIDs() -> [String] {
        defaultInbox().map(\.item.id)
    }

    /// One valid open item of the given type. The artifact_digests set is
    /// the sorted, deduplicated union of the evidence and claim digests,
    /// as the daemon derives it.
    public static func fixture(
        type: Components.Schemas.AttentionType
    ) -> Components.Schemas.AttentionItemSnapshot {
        let key = type.rawValue
        let evidenceDigest = "sha256:log-\(key)"
        let claimDigest = "sha256:img-\(key)"

        let subject: Components.Schemas.Subject
        let prHeadSHA: String
        let provenance: Components.Schemas.EvidenceProvenance
        let claimProvenance: Components.Schemas.ClaimProvenance
        switch type {
        case .run_proposal:
            subject = .proposal_batch(
                .init(subject_type: .proposal_batch, subject_id: "batch-\(key)", run_id: nil))
            prHeadSHA = ""
            provenance = headIndependent(key: key)
            claimProvenance = claimHeadIndependent(key: key)
        case .system_health:
            subject = .system(.init(subject_type: .system, subject_id: "system", run_id: nil))
            prHeadSHA = ""
            provenance = headIndependent(key: key)
            claimProvenance = claimHeadIndependent(key: key)
        default:
            subject = .run(
                .init(subject_type: .run, subject_id: "run-\(key)", run_id: "run-\(key)"))
            prHeadSHA = "cafebabe"
            provenance = .head_bound(
                .init(
                    producer_class: .verifier,
                    producer_invocation_id: "inv-\(key)",
                    head_binding: .head_bound,
                    source_head_sha: "cafebabe",
                    verification_recipe_digest: AttentionFixtures.approvedRecipeDigest,
                    sensitivity_class: .normal
                ))
            claimProvenance = .head_bound(
                .init(
                    producer_class: .agent,
                    producer_invocation_id: "inv-agent-\(key)",
                    head_binding: .head_bound,
                    source_head_sha: "cafebabe",
                    verification_recipe_digest: nil,
                    sensitivity_class: .normal
                ))
        }

        let priority: Components.Schemas.Priority
        let interruption: Components.Schemas.InterruptionClass
        switch type {
        case .spec_approval, .ready_for_final_review, .run_proposal,
            .review_diminishing_returns, .finding_adjudication:
            priority = type == .spec_approval ? .high : .normal
            interruption = .planned_gate
        case .review_contradiction:
            priority = .high
            interruption = .exceptional
        case .review_configuration:
            priority = .high
            interruption = .planned_gate
        default:
            priority = type == .execution_failure ? .urgent : .normal
            interruption = .exceptional
        }

        guard let actions = phase1ActionSets[type] else {
            preconditionFailure("phase1ActionSets is total over phase1Types")
        }

        // Every card keeps its referenced screenshot claim; cards whose type
        // carries §9's summary layer also get an inline text claim, whose
        // digest is computed over the content so the mock's binding check and
        // the fixture can never disagree. The purely mechanical types
        // (system_health, blocked) carry daemon facts alone (§9), so they
        // stay text-free — blocked's unseeded screenshot digest keeps
        // exercising the missing-attachment placeholder.
        var agentClaims: [Components.Schemas.AgentClaim] = [
            .init(
                label: "screenshot",
                artifact_id: "art-img-\(key)",
                digest: claimDigest,
                provenance: claimProvenance
            )
        ]
        // Section 9's agent_question card is self-contained: the question,
        // what it blocks, and the enumerated options are one labeled claim,
        // so answering never needs the transcript. The typed producer arrives
        // with #990; the carrier is the claim contract that exists today.
        if type == .agent_question {
            let question =
                "**Which order should the migration run in?** Implementation is "
                + "blocked until this is answered; the implementer will not "
                + "choose an order on its own.\n\n"
                + "**Option A, store first, then API.** Existing rows migrate "
                + "before any client can read them, and the API keeps the old "
                + "shape for one release.\n\n"
                + "**Option B, API first, then store.** Clients move "
                + "immediately, and the daemon reads both shapes until the "
                + "store migration lands."
            // The question leads the claim register: it is the reason the
            // card exists, and the screenshot claim is supporting context.
            agentClaims.insert(
                .init(
                    label: "Question (unverified)",
                    artifact_id: "art-question-\(key)",
                    digest: MockContractValidation.sha256Digest(of: question),
                    provenance: claimProvenance,
                    text: .init(media_type: .text_sol_markdown, content: question)
                ),
                at: 0)
        }
        if type != .system_health, type != .blocked {
            let summary =
                switch type {
                case .review_dispute:
                    "**P1 shadow finding** at `daemon/main.go:42`: the sampled reviewer found a blocking defect."
                case .execution_failure:
                    "The **build** attempt likely failed because the fixture's generated client is stale."
                case .spec_approval, .agent_question, .review_diminishing_returns,
                    .review_contradiction, .review_configuration, .finding_adjudication,
                    .ready_for_final_review, .publish_blocked, .run_proposal:
                    "Work on **\(key)** is ready; one decision is open."
                case .system_health, .blocked:
                    preconditionFailure("Mechanical cards never carry agent claims")
                }
            let label =
                switch type {
                case .review_dispute: "Shadow finding review-fixture"
                case .execution_failure: "Likely cause (unverified)"
                case .spec_approval, .agent_question, .review_diminishing_returns,
                    .review_contradiction, .review_configuration, .finding_adjudication,
                    .ready_for_final_review, .publish_blocked, .run_proposal:
                    "freeside.summary"
                case .system_health, .blocked:
                    preconditionFailure("Mechanical cards never carry agent claims")
                }
            agentClaims.append(
                .init(
                    label: label,
                    artifact_id: "art-sum-\(key)",
                    digest: MockContractValidation.sha256Digest(of: summary),
                    provenance: claimProvenance,
                    text: .init(media_type: .text_sol_markdown, content: summary)
                ))
            if type == .review_dispute || type == .execution_failure {
                let summary = "Work on **\(key)** stopped; the diagnostic claim above needs a decision."
                agentClaims.append(
                    .init(
                        label: "freeside.summary",
                        artifact_id: "art-summary-\(key)",
                        digest: MockContractValidation.sha256Digest(of: summary),
                        provenance: claimProvenance,
                        text: .init(media_type: .text_sol_markdown, content: summary)
                    ))
            }
        }
        // A run-proposal item is the exact store-derived carrier for one
        // proposal digest. Unlike ordinary attention cards it has no agent
        // claims, so the client's authenticated-facts tuple can require the
        // sole command binding to equal that proposal digest.
        if type == .run_proposal {
            agentClaims = []
        }

        // The daemon-derived commit-plan notice (plan §5.6) rides the review
        // card in the seeded inbox, matching the daemon's golden fixture: a
        // present plan a single_commit repository consumed but did not
        // honor. Every other card keeps the null render.
        let commitPlanNotice: Components.Schemas.AttentionItem.commit_plan_noticePayload? =
            type == .ready_for_final_review ? .init(value1: .present_but_not_honored) : nil
        let prReference: Components.Schemas.AttentionItem.pr_referencePayload? =
            type == .ready_for_final_review
            ? .init(value1: .init(repo: "owner/repo", number: 123)) : nil
        let readiness: Components.Schemas.AttentionItem.readinessPayload? =
            type == .ready_for_final_review
            ? .init(
                value1: .init(
                    _class: .ready_clean,
                    evaluation_set_digest: "sha256:evaluation-clean"
                ))
            : nil
        let yieldHistory: Components.Schemas.AttentionItem.yield_historyPayload?
        switch type {
        case .ready_for_final_review:
            yieldHistory = .init(
                value1: .init(
                    rounds: [
                        .init(
                            round: 1, findings_ingested: 2, new_findings: 2,
                            recurring_findings: 0, fixed: 1, declined: 0, deferred: 1,
                            outcome: .findings),
                        .init(
                            round: 2, findings_ingested: 2, new_findings: 1,
                            recurring_findings: 1, fixed: 1, declined: 1, deferred: 0,
                            outcome: .findings),
                        .init(
                            round: 3, findings_ingested: 0, new_findings: 0,
                            recurring_findings: 0, fixed: 0, declined: 0, deferred: 0,
                            outcome: .clean),
                    ],
                    terminal_outcome: .clean
                ))
        case .review_diminishing_returns:
            yieldHistory = .init(
                value1: .init(
                    rounds: [
                        .init(
                            round: 1, findings_ingested: 4, new_findings: 4,
                            recurring_findings: 0, fixed: 2, declined: 1, deferred: 1,
                            outcome: .findings),
                        .init(
                            round: 2, findings_ingested: 3, new_findings: 1,
                            recurring_findings: 2, fixed: 1, declined: 1, deferred: 1,
                            outcome: .findings),
                        .init(
                            round: 3, findings_ingested: 3, new_findings: 0,
                            recurring_findings: 3, fixed: 0, declined: 2, deferred: 1,
                            outcome: .findings),
                    ],
                    terminal_outcome: .findings
                ))
        case .spec_approval, .execution_failure, .agent_question, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .publish_blocked, .run_proposal, .system_health, .blocked:
            yieldHistory = nil
        }
        let reviewRecoveryBinding: Components.Schemas.AttentionItem.review_recovery_bindingPayload? =
            type == .review_contradiction
            ? .init(
                value1: .init(
                    run_id: "run-\(key)",
                    invocation_id: "review-run-\(key)-1",
                    round: 1,
                    base_sha: "beefcafe",
                    head_sha: "cafebabe",
                    failure_digest: "sha256:failure-\(key)"
                ))
            : nil
        let reviewConfigurationRecovery: Components.Schemas.AttentionItem.review_configuration_recoveryPayload? =
            type == .review_configuration
            ? .init(
                value1: .init(
                    run_id: "run-\(key)",
                    invocation_id: "review-run-\(key)-2",
                    round: 2,
                    base_sha: "beefcafe",
                    head_sha: "cafebabe",
                    failure_digest: "sha256:failure-\(key)",
                    repo: "owner/repo",
                    repository_id: 84_958_515,
                    superseded_profile_digest: "sha256:profile-\(key)"
                ))
            : nil
        let codexReenrollmentRecovery: Components.Schemas.AttentionItem.codex_reenrollment_recovery_bindingPayload? =
            type == .system_health
            ? .init(
                value1: .init(
                    auth_identity_id: "codex-primary",
                    lease_fence: 4,
                    auth_store_digest: "sha256:replacement-store",
                    access_token_expires_at: Date(timeIntervalSince1970: 1_786_502_645)
                ))
            : nil
        let findingAdjudication: Components.Schemas.AttentionItem.finding_adjudicationPayload? =
            type == .finding_adjudication
            ? .init(
                value1: .init(
                    run_id: "run-\(key)",
                    round: 3,
                    adjudication_digest: "sha256:adjudication-\(key)",
                    proposals: [
                        .init(
                            finding_id: "review-finding-17",
                            finding_message:
                                "Command handler retries without preserving the write-once command identity.",
                            finding_location: .init(
                                value1: .init(
                                    path: "daemon/internal/signet/service.go",
                                    start_line: 214, end_line: 227)),
                            producer: .model,
                            goal_relationship: .contradictory,
                            route: .decline,
                            rationale:
                                "The finding assumes a retry guarantee the approved work-unit contract explicitly rejects.",
                            evidence: [
                                "service.go:214 re-derives the command id on each retry",
                                "the contract requires a stable command identity",
                            ],
                            cited_rules: [
                                "AGENTS.md: fail correctly",
                                "Issue contract: preserve write-once command identity",
                            ],
                            assumptions: [
                                "The caller can retry after losing the first response."
                            ],
                            open_questions: [
                                "Should the follow-up also add a durability metric?"
                            ],
                            confidence: .init(value1: .high),
                            offered_alternatives: [
                                .init(
                                    route: .dispute,
                                    consequence:
                                        "Escalate the contract conflict to a human before routing the finding.")
                            ]
                        ),
                        .init(
                            finding_id: "review-finding-18",
                            finding_message:
                                "Review-level observation: the change lacks a regression test.",
                            finding_location: nil,
                            producer: .engine_model,
                            goal_relationship: .required,
                            compatibility: .init(value1: .allowed),
                            route: .remediate,
                            rationale:
                                "The model judged this finding required, and the daemon verified that remediation stays inside the declared paths.",
                            evidence: [
                                "no test file accompanies the changed handler"
                            ],
                            cited_rules: [
                                "Issue contract: preserve declared-path authority"
                            ],
                            assumptions: [],
                            open_questions: [],
                            confidence: .init(value1: .high),
                            offered_alternatives: []
                        ),
                    ]
                ))
            : nil

        let recommendation: Components.Schemas.AttentionItem.recommendationPayload? =
            findingAdjudication.map { binding in
                .init(
                    value1: .init(
                        action: .accept_recommended_route,
                        reason: "Accept the adjudicator's recommended route for each finding.",
                        source: .agent_judgment,
                        provenance: .init(
                            agent_judgment: .init(
                                value1: .init(
                                    judgment_site: .finding_adjudicator,
                                    invocation_id: "adjudicator-run-\(key)-3",
                                    artifact_digest: binding.value1.adjudication_digest
                                ))),
                        confidence: .init(value1: .high)
                    ))
            }
        let decisionSurface = Components.Schemas.DecisionSurfaceRef(
            epoch: 1,
            digest: MockContractValidation.sha256Digest(of: "decision-surface-\(key)-1")
        )

        let displayNames = Components.Schemas.AttentionItem.display_namesPayload(
            value1: .init(
                project: .init(text: "owner/repo", source: .name),
                work_unit: .init(text: "#724", source: .name)
            ))
        let billableCost: Components.Schemas.AttentionItem.billable_cost_so_farPayload? =
            type == .review_diminishing_returns
            ? .init(value1: .init(currency: "USD", amount: "42.75", invocations: 6, complete: false))
            : nil
        let executionFailure: Components.Schemas.AttentionItem.execution_failurePayload? =
            type == .execution_failure
            ? .init(value1: .init(outcome: .failed, stage: .implementation, invocation_id: "inv-\(key)"))
            : nil
        let publishBlock: Components.Schemas.AttentionItem.publish_blockPayload? =
            type == .publish_blocked
            ? .init(
                value1: .init(
                    trust_rule: .init(value1: .trust_profile_drift)
                ))
            : nil
        let diffStats: Components.Schemas.AttentionItem.diff_statsPayload? =
            type == .ready_for_final_review
            ? .init(
                value1: .init(
                    files_changed: 12, additions: 240, deletions: 31,
                    base_sha: "deadbeef", head_sha: "cafebabe"
                ))
            : nil
        let blockedOn: Components.Schemas.AttentionItem.blocked_onPayload? =
            type == .blocked
            ? .init(
                value1: .init(
                    kind: .spec_approval, since: createdInstant,
                    item_id: "item-spec_approval"
                ))
            : nil
        let healthDiagnostic: Components.Schemas.AttentionItem.health_diagnosticPayload? =
            type == .system_health
            ? .init(
                value1: .init(
                    code: "run_projection.unavailable", impairs: .run_visibility
                ))
            : nil
        let reviewDispute: Components.Schemas.AttentionItem.review_disputePayload? =
            type == .review_dispute
            ? .init(
                value1: .init(
                    run_id: "run-\(key)", round: 2,
                    finding_ids: ["finding-1", "finding-2"],
                    completion_evidence: "sha256:review-completion"
                ))
            : nil

        let item = Components.Schemas.AttentionItem(
            id: "item-\(key)",
            project_id: "proj-1",
            subject: subject,
            _type: type,
            priority: priority,
            reason: reason(type: type),
            requested_decision: actions,
            recommendation: recommendation,
            decision_surface: decisionSurface,
            evidence_snapshot: [
                .init(
                    id: "art-log-\(key)",
                    _type: .verify_log,
                    digest: evidenceDigest,
                    provenance: provenance,
                    publish_eligible: true
                )
            ],
            agent_claims: agentClaims,
            artifact_digests: (agentClaims.map(\.digest) + [evidenceDigest]
                + (findingAdjudication.map { [$0.value1.adjudication_digest] } ?? [])).sorted(),
            pr_head_sha: prHeadSHA,
            pr_reference: prReference,
            readiness: readiness,
            yield_history: yieldHistory,
            commit_plan_notice: commitPlanNotice,
            review_recovery_binding: reviewRecoveryBinding,
            codex_reenrollment_recovery_binding: codexReenrollmentRecovery,
            review_configuration_recovery: reviewConfigurationRecovery,
            finding_adjudication: findingAdjudication,
            display_names: displayNames,
            billable_cost_so_far: billableCost,
            execution_failure: executionFailure,
            publish_block: publishBlock,
            diff_stats: diffStats,
            blocked_on: blockedOn,
            health_diagnostic: healthDiagnostic,
            review_dispute: reviewDispute,
            item_version: 1,
            interruption_class: interruption,
            conversation_id: type == .spec_approval ? "conv-item-spec_approval" : nil,
            timing: .init(
                delivery_count: 0,
                first_submitted_at: nil,
                first_accepted_at: nil,
                first_opened_at: nil,
                submit_to_first_open: nil
            ),
            created_at: createdInstant,
            expires_when: nil,
            decided_at: nil,
            posture: type == .system_health ? .init(value1: .advisory) : nil,
            status: .open
        )
        return .init(as_of_revision: 1, entity_version: 1, item: item)
    }

    private static func headIndependent(
        key: String
    ) -> Components.Schemas.EvidenceProvenance {
        .head_independent(
            .init(
                producer_class: .daemon,
                producer_invocation_id: "inv-\(key)",
                head_binding: .head_independent,
                verification_recipe_digest: AttentionFixtures.approvedRecipeDigest,
                sensitivity_class: .normal
            ))
    }

    private static func claimHeadIndependent(
        key: String
    ) -> Components.Schemas.ClaimProvenance {
        .head_independent(
            .init(
                producer_class: .agent,
                producer_invocation_id: "inv-agent-\(key)",
                head_binding: .head_independent,
                verification_recipe_digest: nil,
                sensitivity_class: .normal
            ))
    }

    private static func reason(type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .spec_approval:
            return "the spec for the auth work is ready for approval"
        case .execution_failure:
            return "the build stage failed twice on the same test"
        case .agent_question:
            return "the agent needs a decision on the migration order"
        case .review_diminishing_returns:
            return "review rounds are surfacing only marginal nits"
        case .review_dispute:
            return "the agent disputes a review finding as contrived"
        case .review_contradiction:
            return "review contradicted its execution contract"
        case .review_configuration:
            return "the trust profile no longer approves the reviewer configuration"
        case .finding_adjudication:
            return "a review finding needs an operator-selected disposition"
        case .ready_for_final_review:
            return "checks are green and the diff is ready"
        case .publish_blocked:
            return "trust evaluation failed for the candidate branch"
        case .run_proposal:
            return "a scan proposes a dependency-update run"
        case .system_health:
            return "active-resource observation is temporarily unavailable"
        case .blocked:
            // Consistent with the typed blocked_on wait the card leads with.
            return "the run is waiting on specification approval"
        }
    }
}
