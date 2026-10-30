import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

@Suite struct LaunchInputsTests {
    @Test func colorSchemeParsesLightAndDark() {
        #expect(LaunchInputs(colorSchemeRaw: "light", selectionRaw: nil).colorScheme == .light)
        #expect(LaunchInputs(colorSchemeRaw: "dark", selectionRaw: nil).colorScheme == .dark)
    }

    @Test(arguments: [nil, "Dark", "auto", ""] as [String?])
    func unrecognizedColorSchemeFollowsTheSystem(raw: String?) {
        #expect(LaunchInputs(colorSchemeRaw: raw, selectionRaw: nil).colorScheme == nil)
    }

    @Test func contrastParsesStandardAndIncreased() {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, contrastRaw: "standard", selectionRaw: nil)
                .contrast == .standard)
        #expect(
            LaunchInputs(colorSchemeRaw: nil, contrastRaw: "increased", selectionRaw: nil)
                .contrast == .increased)
    }

    @Test func screenshotContrastLeavesExistingPreferencesIntact() throws {
        let suite = "FreesideScreenshotContrastTest-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set("increased", forKey: "FreesideContrast")
        LaunchInputs.$screenshotIncreasedContrast.withValue(false) {
            #expect(LaunchInputs.accessibilityContrastOverride(defaults: defaults) == .standard)
            #expect(defaults.string(forKey: "FreesideContrast") == "increased")
        }
        #expect(LaunchInputs.accessibilityContrastOverride(defaults: defaults) == .increased)
        #expect(defaults.string(forKey: "FreesideContrast") == "increased")
    }

    @Test(arguments: [nil, "high", "Increased", ""] as [String?])
    func unrecognizedContrastFollowsTheSystem(raw: String?) {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, contrastRaw: raw, selectionRaw: nil).contrast
                == nil)
    }

    @Test func dynamicTypeSizeParsesScreenshotCuts() {
        #expect(
            LaunchInputs(
                colorSchemeRaw: nil, selectionRaw: nil, dynamicTypeSizeRaw: "large"
            ).dynamicTypeSize == .large)
        #expect(
            LaunchInputs(
                colorSchemeRaw: nil, selectionRaw: nil, dynamicTypeSizeRaw: "ax3"
            ).dynamicTypeSize == .accessibility3)
    }

    @Test(arguments: [nil, "AX3", "accessibility3", ""] as [String?])
    func unrecognizedDynamicTypeSizeFollowsTheSystem(raw: String?) {
        #expect(
            LaunchInputs(
                colorSchemeRaw: nil, selectionRaw: nil, dynamicTypeSizeRaw: raw
            ).dynamicTypeSize == nil)
    }

    @Test(arguments: AttentionFixtures.defaultInboxItemIDs())
    func everyCanonicalItemIDIsAccepted(id: String) {
        #expect(LaunchInputs(colorSchemeRaw: nil, selectionRaw: id).selection == .attentionItem(id))
    }

    @Test(arguments: ["item-nope", "blocked", "ITEM-BLOCKED", ""])
    func unknownSelectionIsIgnored(raw: String) {
        #expect(LaunchInputs(colorSchemeRaw: nil, selectionRaw: raw).selection == nil)
    }

    @Test func unsetSelectionStaysUnselected() {
        #expect(LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil).selection == nil)
    }

    @Test func screenshotPresentationInputsAreExplicitAndOptional() {
        let inputs = LaunchInputs(
            colorSchemeRaw: "dark", selectionRaw: "item-review_configuration",
            projectIDRaw: "proj-1", detailsExpanded: true)

        #expect(inputs.projectID == "proj-1")
        #expect(inputs.detailsExpanded)
    }

    @Test(arguments: [
        ("discuss", "item-spec_approval", LaunchInputs.Composer.discuss),
        ("request_changes", "item-spec_approval", .requestChanges),
        ("return_to_agent", "item-ready_for_final_review", .returnToAgent),
        ("answer_and_retry", "item-agent_question", .answerAndRetry),
        ("answer_without_retry", "item-agent_question", .answerWithoutRetry),
    ])
    func composerIsAcceptedOnAnItemThatOffersIt(
        raw: String, itemID: String, composer: LaunchInputs.Composer
    ) {
        let inputs = LaunchInputs(colorSchemeRaw: nil, selectionRaw: itemID, composerRaw: raw)
        #expect(inputs.composer == composer)
        #expect(inputs.selection == .attentionItem(itemID))
    }

    /// The argument takes the API's action names, so each case must name
    /// its own action and no two may share one.
    @Test func everyComposerNamesItsAPIAction() {
        let composers = LaunchInputs.Composer.allCases
        #expect(composers.map(\.rawValue) == composers.map(\.action.rawValue))
        #expect(
            composers.map(\.rawValue) == [
                "discuss", "request_changes", "return_to_agent", "answer_and_retry",
                "answer_without_retry",
            ])
    }

    @Test(arguments: ["nope", "Discuss", "stop", ""])
    func aValueThatNamesNoComposerIsIgnored(raw: String) {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-spec_approval", composerRaw: raw)
                .composer == nil)
    }

    @Test func aComposerTheSelectedItemDoesNotOfferIsIgnored() {
        let inputs = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: "item-agent_question", composerRaw: "discuss")
        #expect(inputs.composer == nil)
        // Only the composer is dropped: the item still opens.
        #expect(inputs.selection == .attentionItem("item-agent_question"))
    }

    @Test func aComposerWithoutASelectedInboxItemIsIgnored() {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, composerRaw: "discuss").composer
                == nil)
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-nope", composerRaw: "discuss")
                .composer == nil)
        // A composer opens on an item, never on a task.
        #expect(
            LaunchInputs(
                colorSchemeRaw: nil, selectionRaw: TaskFixtures.retryTaskID, composerRaw: "discuss"
            ).composer == nil)
    }

    @Test func unsetComposerStaysUnset() {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-spec_approval").composer == nil)
    }

    @Test func aSelectionResolvesAnItemATaskOrARunWithoutAScreen() {
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: "item-spec_approval").selection
                == .attentionItem("item-spec_approval"))
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: TaskFixtures.retryTaskID).selection
                == .task(TaskFixtures.retryTaskID))
        // A run id opens the run's task with the run pushed.
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: RunFixtures.activeRunID).selection
                == .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))
    }

    @Test func noFixtureIDNamesTwoKindsOfThing() {
        let items = Set(AttentionFixtures.defaultInboxItemIDs())
        let tasks = Set(TaskFixtures.defaultTaskIDs())
        let runs = Set(RunFixtures.defaultRuns().map(\.run.id))
        #expect(items.isDisjoint(with: tasks))
        #expect(items.isDisjoint(with: runs))
        #expect(tasks.isDisjoint(with: runs))
    }
}
