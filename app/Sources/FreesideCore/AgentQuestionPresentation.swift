import Foundation
import FreesideAPI

/// The daemon-typed facts of an agent_question card, shaped for display:
/// which role asked, why the implementer stopped, and each decision with its
/// options and the agent's recommendation marked as a claim (plan §9).
public struct AgentQuestionPresentation: Equatable, Sendable {
    public struct Option: Equatable, Sendable {
        public let label: String
        public let tradeoffs: String
        /// True for the option whose label the agent recommended. The
        /// recommendation is an agent claim, never a verified fact.
        public let recommended: Bool
    }

    public struct Decision: Equatable, Sendable {
        public let question: String
        public let whyBlocking: String
        public let options: [Option]
        public let recommendation: String
    }

    public let stage: Components.Schemas.StageName
    public let kind: Components.Schemas.BlockedKind?
    public let decisions: [Decision]

    public init?(_ item: Components.Schemas.AttentionItem) {
        guard item._type == .agent_question, let facts = item.agent_question?.value1 else {
            return nil
        }
        stage = facts.stage
        kind = facts.kind?.value1
        decisions = facts.decisions.map { decision in
            Decision(
                question: decision.question,
                whyBlocking: decision.why_blocking,
                options: decision.options.map {
                    Option(
                        label: $0.label, tradeoffs: $0.tradeoffs,
                        recommended: $0.label == decision.recommendation)
                },
                recommendation: decision.recommendation)
        }
    }

    /// Who stopped to ask.
    public var stageLabel: String {
        switch stage {
        case .specification: return "The specifier stopped to ask"
        case .implementation: return "The implementer stopped to ask"
        case .review: return "The reviewer stopped to ask"
        case .verification: return "The verifier stopped to ask"
        }
    }

    /// The blocker kind in operator words; nil on the specification stage,
    /// which has no blocker taxonomy.
    public var kindLabel: String? {
        kind.map(Self.kindLabel)
    }

    static func kindLabel(_ kind: Components.Schemas.BlockedKind) -> String {
        switch kind {
        case .specification_contradiction: return "Specification contradiction"
        case .owner_decision: return "Owner decision"
        case .scope_expansion: return "Scope expansion"
        case .capability_unavailable: return "Capability unavailable"
        case .commit_plan_collision: return "Commit plan collision"
        }
    }

    /// The answer_route an answer_and_retry on this item must carry: the
    /// implementer retry on an implementation-stage question, nothing on a
    /// specification-stage one. The daemon refuses revise_specification until
    /// a revised specification can mint a fresh implementation identity, so
    /// the app offers no route choice yet; this is where a picker plugs in.
    public static func answerRoute(
        for item: Components.Schemas.AttentionItem?
    ) -> Components.Schemas.AnswerRoute? {
        guard let item, let presentation = AgentQuestionPresentation(item),
            presentation.stage == .implementation
        else { return nil }
        return .retry_implementation
    }
}
