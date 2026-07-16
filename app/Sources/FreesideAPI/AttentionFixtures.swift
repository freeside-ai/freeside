import Foundation

/// Deterministic, schema-valid attention items: one per Phase 1 attention
/// type, with `requested_decision` transcribed from plan §4's per-type
/// action table. Builders over JSON resources so a schema change breaks
/// these at compile time, not at decode time.
public enum AttentionFixtures {
    /// The recipe digest every fixture's evidence is produced under;
    /// MockServer's default approved set contains exactly this digest.
    public static let approvedRecipeDigest = "sha256:recipe-approved"

    /// The ten Phase 1 attention types, in the schema's enum order.
    public static let phase1Types: [Components.Schemas.AttentionType] = [
        .spec_approval,
        .execution_failure,
        .agent_question,
        .review_diminishing_returns,
        .review_dispute,
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
    public static let phase1ActionSets:
        [Components.Schemas.AttentionType: [Components.Schemas.Action]] = [
            .spec_approval: [.approve, .request_changes, .discuss, .stop],
            .execution_failure: [.retry, .retry_with_capabilities, .discuss, .stop],
            .agent_question: [.answer_and_retry, .answer_without_retry, .stop],
            .review_diminishing_returns: [
                .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
            ],
            .review_dispute: [.adjudicate, .discuss, .stop],
            .ready_for_final_review: [.open_pr, .return_to_agent, .mark_seen, .dismiss, .stop],
            .publish_blocked: [
                .rerun_trust_evaluation, .choose_alternate_profile, .inspect_trust_failure, .stop,
            ],
            .run_proposal: [.start, .start_with_changes, .decline, .snooze],
            .system_health: [.acknowledge, .run_doctor, .stop_unattended],
            .blocked: [],
        ]

    /// The default mock inbox: one open item per Phase 1 type.
    public static func defaultInbox() -> [Components.Schemas.AttentionItemSnapshot] {
        phase1Types.map { fixture(type: $0) }
    }

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
        switch type {
        case .run_proposal:
            subject = .proposal_batch(
                .init(subject_type: .proposal_batch, subject_id: "batch-\(key)", run_id: nil))
            prHeadSHA = ""
            provenance = headIndependent(key: key)
        case .system_health:
            subject = .system(.init(subject_type: .system, subject_id: "system", run_id: nil))
            prHeadSHA = ""
            provenance = headIndependent(key: key)
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
        }

        let priority: Components.Schemas.Priority
        let interruption: Components.Schemas.InterruptionClass
        switch type {
        case .spec_approval, .ready_for_final_review, .run_proposal, .review_diminishing_returns:
            priority = type == .spec_approval ? .high : .normal
            interruption = .planned_gate
        default:
            priority = type == .execution_failure ? .urgent : .normal
            interruption = .exceptional
        }

        guard let actions = phase1ActionSets[type] else {
            preconditionFailure("phase1ActionSets is total over phase1Types")
        }

        let item = Components.Schemas.AttentionItem(
            id: "item-\(key)",
            project_id: "proj-1",
            subject: subject,
            _type: type,
            priority: priority,
            reason: reason(type: type),
            requested_decision: actions,
            evidence_snapshot: [
                .init(
                    id: "art-log-\(key)",
                    _type: "verify_log",
                    digest: evidenceDigest,
                    provenance: provenance,
                    publish_eligible: true
                )
            ],
            agent_claims: [
                .init(label: "screenshot", artifact_id: "art-img-\(key)", digest: claimDigest)
            ],
            artifact_digests: [evidenceDigest, claimDigest].sorted(),
            pr_head_sha: prHeadSHA,
            item_version: 1,
            interruption_class: interruption,
            conversation_id: nil,
            timing: .init(
                delivery_count: 0,
                first_submitted_at: nil,
                first_accepted_at: nil,
                first_opened_at: nil,
                submit_to_first_open: nil
            ),
            expires_when: nil,
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
        case .ready_for_final_review:
            return "checks are green and the diff is ready"
        case .publish_blocked:
            return "trust evaluation failed for the candidate branch"
        case .run_proposal:
            return "a scan proposes a dependency-update run"
        case .system_health:
            return "the runner backend is not responding"
        case .blocked:
            return "the run has waited 18h on an external reviewer"
        }
    }
}
