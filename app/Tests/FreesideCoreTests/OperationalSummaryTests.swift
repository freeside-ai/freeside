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
        #expect(summary.highestPriorityID == urgent.item.id)
        #expect(summary.highestPriorityTitle == AttentionDisplay.title(urgent.item))
        #expect(summary.highestPriorityLabel == "Urgent")
        #expect(summary.waitingLongestID == older.item.id)
        #expect(summary.waitingLongestTitle == AttentionDisplay.title(older.item))
        #expect(summary.waitingLongestItem == older.item)
        #expect(summary.activeRunCount == 3)
        #expect(summary.daemonState == .connected)
    }

    @Test func waitingLongestFollowsTheDisplayedWaitNotCreation() {
        var older = AttentionFixtures.fixture(type: .spec_approval)
        older.item.created_at = Date(timeIntervalSince1970: 10)
        var blocked = AttentionFixtures.fixture(type: .blocked)
        blocked.item.created_at = Date(timeIntervalSince1970: 100)
        blocked.item.blocked_on = .init(
            value1: .init(
                kind: .spec_approval, since: Date(timeIntervalSince1970: 5),
                item_id: older.item.id))
        let summary = OperationalSummary(
            openSnapshots: [older, blocked], runs: [], freshness: .fresh)

        #expect(summary.waitingLongestID == blocked.item.id)
        let now = Date(timeIntervalSince1970: 3_605)
        #expect(
            summary.waitingLongestValue(now: now)
                == "\(AttentionDisplay.title(blocked.item)) · waiting 1h")
    }

    @Test func waitingLongestShowsTheInboxRowTimeDeadlineFirst() {
        var due = AttentionFixtures.fixture(type: .spec_approval)
        due.item.created_at = Date(timeIntervalSince1970: 10)
        due.item.expires_when = Date(timeIntervalSince1970: 7_210)
        let now = Date(timeIntervalSince1970: 10)
        let summary = OperationalSummary(openSnapshots: [due], runs: [], freshness: .fresh)

        #expect(summary.waitingLongestValue(now: now) == "\(AttentionDisplay.title(due.item)) · due 2h")
    }

    @Test func emptyInboxNamesNoItems() {
        let summary = OperationalSummary(openSnapshots: [], runs: [], freshness: .fresh)

        #expect(summary.highestPriorityID == nil)
        #expect(summary.waitingLongestID == nil)
        #expect(summary.waitingLongestValue(now: Date()) == nil)
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
