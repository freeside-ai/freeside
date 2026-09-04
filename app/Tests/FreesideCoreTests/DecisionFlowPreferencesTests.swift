import Foundation
import Testing

@testable import FreesideCore

@Suite @MainActor struct DecisionFlowPreferencesTests {
    @Test func nextItemAdvanceDefaultsOffAndPersistsOptIn() throws {
        let suiteName = "DecisionFlowPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let first = DecisionFlowPreferences(defaults: defaults)
        #expect(!first.advancesToNextItem)
        first.advancesToNextItem = true

        #expect(DecisionFlowPreferences(defaults: defaults).advancesToNextItem)
    }
}
