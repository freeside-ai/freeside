import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct FreesideCommandsTests {
    @Test func everyRegisteredShortcutHasOneMenuDescriptor() {
        let descriptors = FreesideCommandDescriptor.all

        #expect(descriptors.map(\.id) == FreesideCommandAction.allCases)
        #expect(Set(descriptors.map(\.title)).count == descriptors.count)
        #expect(!FreesideCommandAction.allCases.map(\.rawValue).contains("find"))
        #expect(
            !descriptors.contains {
                guard let shortcut = $0.shortcut else { return false }
                return shortcut.modifiers.isEmpty && shortcut.key != .escape
            })
    }

    @Test func theListHasNoSectionsToSwitch() {
        #expect(!FreesideCommandDescriptor.all.contains { $0.shortcut?.key == "1" || $0.shortcut?.key == "2" })
    }

    @Test func newTaskCommandCarriesCommandN() {
        let descriptor = FreesideCommandDescriptor.all.first { $0.id == .newTask }
        #expect(descriptor?.shortcut?.key == "n")
        #expect(descriptor?.shortcut?.modifiers == .command)
    }

    @Test func returnRequiresTheValidatedAuthoritativeRecommendation() {
        #expect(
            DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: .approve,
                presentedRecommendation: .approve,
                recommendationHoldsFill: true,
                actionsEnabled: true,
                isSubmittable: true,
                inputIsReady: true))
        #expect(
            !DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: nil,
                presentedRecommendation: .approve,
                recommendationHoldsFill: true,
                actionsEnabled: true,
                isSubmittable: true,
                inputIsReady: true))
        #expect(
            !DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: .approve,
                presentedRecommendation: .approve,
                recommendationHoldsFill: true,
                actionsEnabled: false,
                isSubmittable: true,
                inputIsReady: true))
    }

    /// Return follows the fill (R6): while a held finding pick moves the
    /// fill to Choose Another Route, Return does not send the acceptance,
    /// which would drop the pick (#3011).
    @Test func returnLeavesARecommendationThatDoesNotHoldTheFill() {
        #expect(
            !DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: .accept_recommended_route,
                presentedRecommendation: .accept_recommended_route,
                recommendationHoldsFill: false,
                actionsEnabled: true,
                isSubmittable: true,
                inputIsReady: true))
    }
}
