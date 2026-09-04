import FreesideAPI

/// The daemon's authoritative lead for one decision surface, projected for
/// display. Built only from the contract's `recommendation` (plan §4): the
/// client never infers one from offer order, decision history, or item type,
/// and no recommendation means equally weighted actions.
struct DecisionRecommendationPresentation: Equatable {
    /// Section 9's source registers. A `daemon_policy` recommendation renders
    /// as a card fact, an `agent_judgment` recommendation as a labeled
    /// (unverified) proposal, and a `project_policy` recommendation as a card
    /// fact citing its exact policy key and digest.
    enum Register: Equatable {
        case daemonFact
        case agentClaim
        case projectPolicy

        /// Agent judgment is the only register whose content is a claim, so it
        /// carries the dashed unverified treatment the claim layer uses.
        var isUnverifiedClaim: Bool { self == .agentClaim }
    }

    struct SourceFact: Equatable, Identifiable {
        let label: String
        let value: String
        let monospaced: Bool

        var id: String { label }
    }

    let action: Components.Schemas.Action
    let reason: String
    let confidence: String?
    let register: Register
    let title: String
    let sourceFacts: [SourceFact]

    /// Fails when `source` and `provenance` disagree, so a recommendation whose
    /// source-specific provenance cannot be revalidated renders as no
    /// recommendation at all rather than in a register its provenance does not
    /// support (plan §9). The contract requires exactly one non-null
    /// provenance property matching `source`; a client that trusted `source`
    /// alone would render a register the item never authenticated.
    init?(_ recommendation: Components.Schemas.Recommendation) {
        let provenance = recommendation.provenance
        let daemonPolicy = provenance.daemon_policy?.value1
        let agentJudgment = provenance.agent_judgment?.value1
        let projectPolicy = provenance.project_policy?.value1
        let present =
            [daemonPolicy != nil, agentJudgment != nil, projectPolicy != nil]
            .filter { $0 }.count
        guard present == 1 else { return nil }

        switch recommendation.source {
        case .daemon_policy:
            guard let daemonPolicy else { return nil }
            register = .daemonFact
            title = "Recommended · daemon policy"
            sourceFacts = [
                .init(label: "Rule digest", value: daemonPolicy.rule_digest, monospaced: true),
                .init(label: "Input digest", value: daemonPolicy.input_digest, monospaced: true),
            ]
        case .agent_judgment:
            guard let agentJudgment else { return nil }
            register = .agentClaim
            title = "Recommended · agent judgment"
            sourceFacts = [
                .init(
                    label: "Judgment site",
                    value: AttentionDisplay.label(agentJudgment.judgment_site),
                    monospaced: false),
                .init(
                    label: "Judgment invocation",
                    value: agentJudgment.invocation_id, monospaced: true),
                .init(
                    label: "Artifact digest",
                    value: agentJudgment.artifact_digest, monospaced: true),
            ]
        case .project_policy:
            guard let projectPolicy else { return nil }
            register = .projectPolicy
            title = "Recommended · project policy"
            sourceFacts = [
                .init(label: "Policy key", value: projectPolicy.policy_key, monospaced: true),
                .init(
                    label: "Policy digest",
                    value: projectPolicy.resolved_policy_digest, monospaced: true),
                .init(
                    label: "Application digest",
                    value: projectPolicy.application_digest, monospaced: true),
            ]
        }

        action = recommendation.action
        reason = recommendation.reason
        confidence = recommendation.confidence.map { AttentionDisplay.label($0.value1) }
    }

    /// The block's one label line: the register, then the daemon's confidence
    /// when it recorded one. Confidence is a property of the recommendation
    /// itself, so it reads with the register rather than as a fact row the
    /// operator has to find below the reason (#1107).
    var label: String {
        guard let confidence else { return title }
        return "\(title) · \(confidence)"
    }

    /// The recommendation an item carries, or none. Kept beside the projection
    /// so every call site reads the same authoritative field.
    static func of(
        _ item: Components.Schemas.AttentionItem
    ) -> DecisionRecommendationPresentation? {
        item.recommendation.flatMap { DecisionRecommendationPresentation($0.value1) }
    }
}
