import Foundation
import Testing

@testable import FreesideCore

@Suite @MainActor struct DecisionFlowPreferencesTests {
    @Test func automaticAdvanceDefaultsOnAndPersistsOptOut() throws {
        let suiteName = "DecisionFlowPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let first = DecisionFlowPreferences(defaults: defaults)
        #expect(first.advancesAutomatically)
        first.advancesAutomatically = false

        #expect(!DecisionFlowPreferences(defaults: defaults).advancesAutomatically)
    }
}
