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
        #expect(summary.activeRunCount == 3)
        #expect(summary.daemonState == .connected)
    }

    @Test func activeCountUsesLifecycleIncludingPublishedAndExcludingSupersededPending() throws {
        var superseded = RunFixtures.defaultRuns()[0]
        superseded.run.superseded_by = "successor"
        superseded.run.lifecycle = .finished
        let runs = RunFixtures.defaultRuns() + [superseded]
        let summary = OperationalSummary(openSnapshots: [], runs: runs, freshness: .fresh)
        #expect(summary.activeRunCount == RunListFilter().count(in: runs, scope: .active))
        #expect(summary.activeRunCount == 3)

        let published = try #require(runs.first { $0.run.outcome == .published })
        let publishedOnly = OperationalSummary(openSnapshots: [], runs: [published], freshness: .fresh)
        let supersededOnly = OperationalSummary(openSnapshots: [], runs: [superseded], freshness: .fresh)
        #expect(publishedOnly.activeRunCount == 1)
        #expect(supersededOnly.activeRunCount == 0)
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
