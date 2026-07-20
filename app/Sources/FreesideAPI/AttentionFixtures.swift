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
    public static let phase1ActionSets: [Components.Schemas.AttentionType: [Components.Schemas.Action]] = [
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

    /// Every Phase 1 action, in the schema's enum order: the Swift analogue of
    /// Go's `domain.AllActions`, hand-authored like `phase1Types` rather than
    /// derived. The cross-language policy-parity suite enumerates each type's
    /// *disallowed* complement against this list, so a daemon that reassigned an
    /// action would surface; `FixtureTests` guards it against the union of
    /// `phase1ActionSets` so it cannot silently omit a member.
    public static let phase1Actions: [Components.Schemas.Action] = [
        .approve, .request_changes, .discuss, .stop,
        .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
        .adjudicate, .retry, .retry_with_capabilities,
        .answer_and_retry, .answer_without_retry,
        .rerun_trust_evaluation, .choose_alternate_profile, .inspect_trust_failure,
        .open_pr, .return_to_agent, .mark_seen, .dismiss,
        .start, .start_with_changes, .decline, .snooze,
        .acknowledge, .run_doctor, .stop_unattended,
    ]

    /// The default mock inbox: one open item per Phase 1 type.
    public static func defaultInbox() -> [Components.Schemas.AttentionItemSnapshot] {
        phase1Types.map { fixture(type: $0) }
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
        if type != .system_health, type != .blocked {
            let summary = "Work on **\(key)** is ready; one decision is open."
            agentClaims.append(
                .init(
                    label: "summary",
                    artifact_id: "art-sum-\(key)",
                    digest: MockContractValidation.sha256Digest(of: summary),
                    provenance: claimProvenance,
                    text: .init(media_type: .text_sol_markdown, content: summary)
                ))
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
            agent_claims: agentClaims,
            artifact_digests: (agentClaims.map(\.digest) + [evidenceDigest]).sorted(),
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
            decided_at: nil,
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
