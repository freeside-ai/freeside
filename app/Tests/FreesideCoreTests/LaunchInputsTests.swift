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

    @Test func runsScreenAcceptsOnlyRunFixtureSelections() {
        let run = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: RunFixtures.activeRunID, screenRaw: "runs")
        #expect(run.screen == .runs)
        #expect(run.selection == RunFixtures.activeRunID)

        let item = LaunchInputs(
            colorSchemeRaw: nil, selectionRaw: "item-spec_approval", screenRaw: "runs")
        #expect(item.selection == nil)
    }
}
