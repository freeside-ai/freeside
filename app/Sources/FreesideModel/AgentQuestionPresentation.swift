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
        /// The option's 1-based place in the agent's order. The card draws
        /// the recommended option first but keeps this number, so an answer
        /// typed as "option 2" means the option the agent listed second.
        public let number: Int
    }

    public struct Decision: Equatable, Sendable {
        public let question: String
        public let whyBlocking: String
        /// The recommended option first, then the rest in the agent's order.
        /// Only the card sorts: the record and the wire keep the agent's
        /// order, and an option is named by its label, never its position.
        public let options: [Option]
        public let recommendation: String
    }

    /// The owner's choice for one decision on the card: an option, named by
    /// its exact label (unique within its decision and kept when a re-asked
    /// question reorders its options), or an answer written in the note.
    public enum Choice: Hashable, Sendable {
        case option(String)
        case other
    }

    /// The choice the "Other answer" row draws as, in the card and the sheet.
    public static let otherAnswerLabel = "Other answer"

    public let stage: Components.Schemas.StageName
    public let kind: Components.Schemas.BlockedKind?
    public let decisions: [Decision]
    public let scopeConflict: Components.Schemas.ScopeConflictFacts?

    public init?(_ item: Components.Schemas.AttentionItem) {
        guard item._type == .agent_question, let facts = item.agent_question?.value1 else {
            return nil
        }
        stage = facts.stage
        kind = facts.kind?.value1
        scopeConflict = facts.scope_conflict?.value1
        decisions = facts.decisions.map { decision in
            Decision(
                question: decision.question,
                whyBlocking: decision.why_blocking,
                options: Self.recommendedFirst(
                    decision.options.enumerated().map { index, option in
                        Option(
                            label: option.label, tradeoffs: option.tradeoffs,
                            recommended: option.label == decision.recommendation, number: index + 1)
                    }),
                recommendation: decision.recommendation)
        }
    }

    /// The recommended option, then the others in their order. A stable
    /// partition, so options the agent didn't recommend keep its order.
    static func recommendedFirst(_ options: [Option]) -> [Option] {
        options.filter(\.recommended) + options.filter { !$0.recommended }
    }

    /// The answers the choices make, one per decision in decision order, or
    /// nil until every decision has a choice. Choices outside the question
    /// (an index past the decisions, or a label no option carries) count
    /// as no choice, so a stale selection never reaches the daemon.
    public func answers(_ choices: [Int: Choice]) -> [Components.Schemas.QuestionAnswer]? {
        var answers: [Components.Schemas.QuestionAnswer] = []
        for (index, decision) in decisions.enumerated() {
            switch choices[index] {
            case .option(let label)?:
                guard decision.options.contains(where: { $0.label == label }) else { return nil }
                answers.append(.init(decision_index: index, option_label: label))
            case .other?:
                answers.append(.init(decision_index: index, option_label: nil))
            case nil:
                return nil
            }
        }
        return answers.isEmpty ? nil : answers
    }

    /// Whether the answers leave a decision to the note, which the daemon
    /// then requires.
    public static func needsNote(_ answers: [Components.Schemas.QuestionAnswer]) -> Bool {
        answers.contains { $0.option_label == nil }
    }

    /// The choices as the answer sheet lists them above its note, one line
    /// per decision: `Decision N: <label>`.
    public static func choiceLines(_ answers: [Components.Schemas.QuestionAnswer]) -> [String] {
        answers.map { "Decision \($0.decision_index + 1): \($0.option_label ?? otherAnswerLabel)" }
    }

    /// The answer sheet's confirm label: the chosen option for a one-decision
    /// question, else the number of answers, which the sheet lists above it.
    /// A long label is shortened; the sheet's list shows it whole.
    public static func confirmLabel(
        _ action: Components.Schemas.Action, answers: [Components.Schemas.QuestionAnswer]
    ) -> String {
        let verb = action == .answer_without_retry ? "Record" : "Answer and Retry"
        guard answers.count == 1, let answer = answers.first else {
            return "\(verb) with \(answers.count) Answers"
        }
        let choice = answer.option_label ?? otherAnswerLabel
        let shown = choice.count > 40 ? choice.prefix(39) + "\u{2026}" : Substring(choice)
        return "\(verb): \(shown)"
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
        case .unknown: return "Unrecognized blocker"
        }
    }

    /// The answer_route choices an answer_and_retry on this item offers: an
    /// implementation-stage question without a scope conflict lets the operator
    /// retry the implementer or revise the specification (#1083); every other
    /// item offers none, so its answer carries no route. The order is the
    /// display order, and the first entry is the default.
    public static func answerRoutes(
        for item: Components.Schemas.AttentionItem?
    ) -> [Components.Schemas.AnswerRoute] {
        guard let item, let presentation = AgentQuestionPresentation(item),
            presentation.stage == .implementation, presentation.scopeConflict == nil
        else { return [] }
        return [.retry_implementation, .revise_specification]
    }

    /// The default route an answer_and_retry on this item carries when the
    /// operator does not choose otherwise, or nil when the item offers none.
    public static func answerRoute(
        for item: Components.Schemas.AttentionItem?
    ) -> Components.Schemas.AnswerRoute? {
        answerRoutes(for: item).first
    }

    /// The operator-facing label for an answer route.
    public static func answerRouteLabel(_ route: Components.Schemas.AnswerRoute) -> String {
        switch route {
        case .retry_implementation: return "Retry the implementer"
        case .revise_specification: return "Revise the specification"
        case .unknown: return "Unrecognized route"
        }
    }
}
