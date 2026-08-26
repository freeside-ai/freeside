import Foundation
import Testing

@testable import FreesideCore

@Suite @MainActor struct DecisionSectionPreferencesTests {
    @Test func sectionExpansionPersistsAcrossInstances() throws {
        let suiteName = "DecisionSectionPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let first = DecisionSectionPreferences(defaults: defaults)
        #expect(first.factsExpanded)
        #expect(!first.detailsExpanded)
        first.factsExpanded = false
        first.claimsExpanded = true
        first.detailsExpanded = true

        let relaunched = DecisionSectionPreferences(defaults: defaults)
        #expect(!relaunched.factsExpanded)
        #expect(relaunched.claimsExpanded)
        #expect(relaunched.detailsExpanded)
    }

    @Test func launchOverrideWinsWithoutChangingOtherSections() throws {
        let suiteName = "DecisionSectionPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }
        defaults.set(false, forKey: "FreesideDecisionInspectorDetailsExpanded")

        let preferences = DecisionSectionPreferences(
            defaults: defaults,
            detailsExpandedOverride: true)

        #expect(preferences.detailsExpanded)
        #expect(preferences.factsExpanded)
        #expect(!preferences.claimsExpanded)
    }
}
