import Foundation
import Testing

@testable import FreesideModel

@Suite @MainActor struct FindingReasonPreferencesTests {
    /// A reason opened on one card is open when the card is reopened,
    /// including after the app relaunches (#2608).
    @Test func anExpandedReasonSurvivesAReopen() throws {
        let suiteName = "FindingReasonPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let first = FindingReasonPreferences(defaults: defaults)
        #expect(!first.isExpanded(findingID: "finding-1", itemID: "item-a"))
        first.setExpanded(true, findingID: "finding-1", itemID: "item-a")

        let reopened = FindingReasonPreferences(defaults: defaults)
        #expect(reopened.isExpanded(findingID: "finding-1", itemID: "item-a"))

        reopened.setExpanded(false, findingID: "finding-1", itemID: "item-a")
        #expect(!FindingReasonPreferences(defaults: defaults).isExpanded(findingID: "finding-1", itemID: "item-a"))
    }

    /// Finding ids such as `finding-1` repeat across items, so the same id on
    /// another item keeps its own state.
    @Test func theSameFindingIDOnAnotherItemStaysApart() throws {
        let suiteName = "FindingReasonPreferencesTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        let preferences = FindingReasonPreferences(defaults: defaults)
        preferences.setExpanded(true, findingID: "finding-1", itemID: "item-a")

        #expect(preferences.isExpanded(findingID: "finding-1", itemID: "item-a"))
        #expect(!preferences.isExpanded(findingID: "finding-1", itemID: "item-b"))
        let reopened = FindingReasonPreferences(defaults: defaults)
        #expect(!reopened.isExpanded(findingID: "finding-1", itemID: "item-b"))
    }

    /// The in-memory store a screenshot uses neither reads nor writes the
    /// device's state.
    @Test func anInMemoryStoreLeavesNothingBehind() {
        let preferences = FindingReasonPreferences(defaults: nil)
        preferences.setExpanded(true, findingID: "finding-1", itemID: "item-a")

        #expect(preferences.isExpanded(findingID: "finding-1", itemID: "item-a"))
        #expect(!FindingReasonPreferences(defaults: nil).isExpanded(findingID: "finding-1", itemID: "item-a"))
    }
}
