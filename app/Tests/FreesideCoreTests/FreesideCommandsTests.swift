import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct FreesideCommandsTests {
    @Test func everyRegisteredShortcutHasOneMenuDescriptor() {
        let descriptors = FreesideCommandDescriptor.all

        #expect(descriptors.map(\.id) == FreesideCommandAction.allCases)
        #expect(Set(descriptors.map(\.title)).count == descriptors.count)
        #expect(!descriptors.contains { $0.key == " " })
    }

    @Test func returnRequiresTheValidatedAuthoritativeRecommendation() {
        #expect(
            DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: .approve,
                presentedRecommendation: .approve,
                actionsEnabled: true,
                isSubmittable: true,
                inputIsReady: true))
        #expect(
            !DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: nil,
                presentedRecommendation: .approve,
                actionsEnabled: true,
                isSubmittable: true,
                inputIsReady: true))
        #expect(
            !DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: .approve,
                presentedRecommendation: .approve,
                actionsEnabled: false,
                isSubmittable: true,
                inputIsReady: true))
    }
}
