import Foundation
import Testing

@testable import FreesideModel

@Suite @MainActor struct ListSectionPreferencesTests {
    @Test func sectionsStartCollapsedAndPersistPerDevice() throws {
        let suiteName = "ListSectionPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let first = ListSectionPreferences(defaults: defaults)
        #expect(!first.workingExpanded)
        #expect(!first.finishedExpanded)
        first.workingExpanded = true

        let relaunched = ListSectionPreferences(defaults: defaults)
        #expect(relaunched.workingExpanded)
        #expect(!relaunched.finishedExpanded)
        #expect(defaults.bool(forKey: "FreesideListWorkingExpanded"))

        relaunched.finishedExpanded = true
        #expect(defaults.bool(forKey: "FreesideListFinishedExpanded"))
    }
}
