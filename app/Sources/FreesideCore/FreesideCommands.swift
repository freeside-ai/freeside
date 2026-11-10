import FreesideAPI
import SwiftUI

enum DecisionKeyboardGate {
    /// Whether Return takes the recommendation. It takes it only while the
    /// recommendation's button holds the card's fill (R6), so Return never
    /// sends a control the card draws outlined: while a finding pick holds
    /// the fill, the acceptance would drop the pick (#3011).
    static func canTakeRecommendation(
        rankedRecommendation: Components.Schemas.Action?,
        presentedRecommendation: Components.Schemas.Action?,
        recommendationHoldsFill: Bool,
        actionsEnabled: Bool,
        isSubmittable: Bool,
        inputIsReady: Bool
    ) -> Bool {
        guard let rankedRecommendation,
            rankedRecommendation == presentedRecommendation,
            recommendationHoldsFill
        else { return false }
        return actionsEnabled && isSubmittable && inputIsReady
    }
}

public enum FreesideCommandAction: String, CaseIterable, Identifiable, Sendable {
    case refresh
    case nextItem
    case previousItem
    case takeRecommendation
    case cancelPendingAction
    case newTask

    public var id: Self { self }
}

public struct FreesideCommandDescriptor: Identifiable {
    public let id: FreesideCommandAction
    public let title: String
    public let shortcut: KeyboardShortcut?

    @MainActor public static let all: [Self] = [
        .init(id: .refresh, title: "Refresh", shortcut: .init("r", modifiers: .command)),
        .init(id: .nextItem, title: "Next Item", shortcut: nil),
        .init(id: .previousItem, title: "Previous Item", shortcut: nil),
        .init(id: .takeRecommendation, title: "Take Recommendation", shortcut: nil),
        .init(
            id: .cancelPendingAction, title: "Cancel Pending Action",
            shortcut: .init(.escape, modifiers: [])),
        .init(id: .newTask, title: "New Task", shortcut: .init("n", modifiers: .command)),
    ]
}

public struct FocusedDecisionCommandActions {
    public let canTakeRecommendation: Bool
    public let takeRecommendation: @MainActor () -> Void
    public let cancelPendingAction: @MainActor () -> Void

    public init(
        canTakeRecommendation: Bool,
        takeRecommendation: @escaping @MainActor () -> Void,
        cancelPendingAction: @escaping @MainActor () -> Void
    ) {
        self.canTakeRecommendation = canTakeRecommendation
        self.takeRecommendation = takeRecommendation
        self.cancelPendingAction = cancelPendingAction
    }
}

private struct DecisionCommandActionsKey: FocusedValueKey {
    typealias Value = FocusedDecisionCommandActions
}

extension FocusedValues {
    public var decisionCommandActions: FocusedDecisionCommandActions? {
        get { self[DecisionCommandActionsKey.self] }
        set { self[DecisionCommandActionsKey.self] = newValue }
    }
}
