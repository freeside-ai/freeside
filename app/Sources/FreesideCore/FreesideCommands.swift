import FreesideAPI
import SwiftUI

enum DecisionKeyboardGate {
    static func canTakeRecommendation(
        rankedRecommendation: Components.Schemas.Action?,
        presentedRecommendation: Components.Schemas.Action?,
        actionsEnabled: Bool,
        isSubmittable: Bool,
        inputIsReady: Bool
    ) -> Bool {
        guard let rankedRecommendation,
            rankedRecommendation == presentedRecommendation
        else { return false }
        return actionsEnabled && isSubmittable && inputIsReady
    }
}

public enum FreesideCommandAction: String, CaseIterable, Identifiable, Sendable {
    case showInbox
    case showRuns
    case refresh
    case toggleInspector
    case nextItem
    case previousItem
    case takeRecommendation
    case cancelPendingAction
    case find

    public var id: Self { self }
}

public struct FreesideCommandDescriptor: Identifiable {
    public let id: FreesideCommandAction
    public let title: String
    public let key: KeyEquivalent
    public let modifiers: EventModifiers

    @MainActor public static let all: [Self] = [
        .init(id: .showInbox, title: "Show Inbox", key: "1", modifiers: .command),
        .init(id: .showRuns, title: "Show Runs", key: "2", modifiers: .command),
        .init(id: .refresh, title: "Refresh", key: "r", modifiers: .command),
        .init(
            id: .toggleInspector, title: "Toggle Inspector", key: "i",
            modifiers: [.command, .option]),
        .init(id: .nextItem, title: "Next Item", key: "j", modifiers: []),
        .init(id: .previousItem, title: "Previous Item", key: "k", modifiers: []),
        .init(
            id: .takeRecommendation, title: "Take Recommendation", key: .return,
            modifiers: []),
        .init(
            id: .cancelPendingAction, title: "Cancel Pending Action", key: .escape,
            modifiers: []),
        .init(id: .find, title: "Find", key: "f", modifiers: .command),
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
