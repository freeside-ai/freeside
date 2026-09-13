import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore

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
        #expect(LaunchInputs(colorSchemeRaw: nil, selectionRaw: id).selection == id)
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
            inboxScopeRaw: "resolved", projectIDRaw: "proj-1", detailsExpanded: true)

        #expect(inputs.inboxScope == .resolved)
        #expect(inputs.projectID == "proj-1")
        #expect(inputs.detailsExpanded)
        #expect(
            LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, inboxScopeRaw: "nope")
                .inboxScope == nil)
    }

    @Test func tasksScreenAcceptsTaskAndRunFixtureSelections() {
        let task = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: TaskFixtures.retryTaskID, screenRaw: "tasks")
        #expect(task.screen == .tasks)
        #expect(task.selection == TaskFixtures.retryTaskID)
        #expect(task.taskSelection == .task(TaskFixtures.retryTaskID))

        // A run id opens the run's task with the run pushed.
        let run = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: RunFixtures.activeRunID, screenRaw: "tasks")
        #expect(run.selection == RunFixtures.activeRunID)
        #expect(run.taskSelection == .run(taskID: TaskFixtures.retryTaskID, runID: RunFixtures.activeRunID))

        let item = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: "item-spec_approval", screenRaw: "tasks")
        #expect(item.selection == nil)
        #expect(item.taskSelection == nil)

        // The inbox screen does not take a task or run id, and never
        // carries a task selection.
        let inbox = LaunchInputs(colorSchemeRaw: nil, selectionRaw: TaskFixtures.retryTaskID)
        #expect(inbox.screen == .inbox)
        #expect(inbox.selection == nil)
        #expect(inbox.taskSelection == nil)
        #expect(LaunchInputs(colorSchemeRaw: nil, selectionRaw: nil, screenRaw: "runs").screen == .inbox)
    }
}
