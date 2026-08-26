import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct OperationalSummaryTests {
    @Test func fixtureStateDerivesPriorityAgeRunsAndDaemonState() {
        var older = AttentionFixtures.fixture(type: .spec_approval)
        older.item.created_at = Date(timeIntervalSince1970: 10)
        var urgent = AttentionFixtures.fixture(type: .execution_failure)
        urgent.item.created_at = Date(timeIntervalSince1970: 20)
        let summary = OperationalSummary(
            openSnapshots: [older, urgent],
            runs: RunFixtures.defaultRuns(),
            freshness: .fresh)

        #expect(summary.openCount == 2)
        #expect(summary.highestPriorityTitle == AttentionDisplay.title(urgent.item))
        #expect(summary.highestPriorityLabel == "Urgent")
        #expect(summary.waitingLongestTitle == AttentionDisplay.title(older.item))
        #expect(summary.activeRunCount == 1)
        #expect(summary.daemonState == .connected)
    }

    @Test func retainedOpenProjectionDoesNotReapplyLiveStatus() {
        var retained = AttentionFixtures.fixture(type: .execution_failure)
        retained.item.status = .resolved

        let summary = OperationalSummary(
            openSnapshots: [retained],
            runs: [],
            freshness: .fresh)

        #expect(summary.openCount == 1)
        #expect(summary.highestPriorityTitle == AttentionDisplay.title(retained.item))
        #expect(summary.waitingLongestTitle == AttentionDisplay.title(retained.item))
    }
}
