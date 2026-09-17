import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct OperationalSummaryTests {
    @Test func terminalLifecycleLabelsDoNotImplyCompletion() throws {
        var task = try #require(TaskFixtures.defaultTasks().first { $0.task.lifecycle == .active }).task
        #expect(TaskDisplay.lifecycleLabel(task).text == "Active")
        #expect(TaskDisplay.lifecycleLabel(task).systemImage == "circle.dotted")
        task.lifecycle = .finished
        #expect(TaskDisplay.lifecycleLabel(task).text == "Finished")
        #expect(TaskDisplay.lifecycleLabel(task).systemImage == "checkmark.circle")
        task.lifecycle = .stopped
        #expect(TaskDisplay.lifecycleLabel(task).text == "Stopped")
        #expect(TaskDisplay.lifecycleLabel(task).systemImage == "stop.circle")
        task.lifecycle = .abandoned
        #expect(TaskDisplay.lifecycleLabel(task).text == "Abandoned")
        #expect(TaskDisplay.lifecycleLabel(task).systemImage == "minus.circle")
        task.lifecycle = nil
        #expect(TaskDisplay.lifecycleLabel(task).text == "Active")
    }

    @Test func stoppedAndAbandonedTasksLeaveActiveCountsAndRemainInHistory() throws {
        let active = try #require(TaskFixtures.defaultTasks().first { $0.task.lifecycle == .active })
        let stopped = TaskFixtures.confirmedStopped(active)
        let abandoned = TaskFixtures.explicitlyAbandoned(active)
        var queued = active
        queued.task.lifecycle = nil
        queued.task.current_position = nil
        queued.task.run_ids = []
        queued.task.campaign_ids = []
        queued.task.lifecycle_facts = []
        queued.task.wip = false
        let stoppedQueued = TaskFixtures.confirmedStopped(queued)
        let tasks = [active, stopped, abandoned, queued, stoppedQueued]
        let filter = TaskListFilter()
        #expect(OperationalSummary(openSnapshots: [], tasks: tasks, freshness: .fresh).activeTaskCount == 2)
        #expect(filter.count(in: tasks, scope: .active) == 2)
        #expect(filter.count(in: tasks, scope: .finished) == 3)
        #expect(filter.count(in: tasks, scope: .all) == 5)
        for terminal in [stopped, abandoned, stoppedQueued] {
            var reveal = TaskListFilter()
            reveal.reveal(terminal.task)
            #expect(reveal.scope == .finished)
        }
    }

    @Test func fixtureStateDerivesPriorityAgeRunsAndDaemonState() {
        var older = AttentionFixtures.fixture(type: .spec_approval)
        older.item.created_at = Date(timeIntervalSince1970: 10)
        var urgent = AttentionFixtures.fixture(type: .execution_failure)
        urgent.item.created_at = Date(timeIntervalSince1970: 20)
        let summary = OperationalSummary(
            openSnapshots: [older, urgent],
            tasks: TaskFixtures.defaultTasks(),
            freshness: .fresh)

        #expect(summary.openCount == 2)
        #expect(summary.highestPriorityID == urgent.item.id)
        #expect(summary.highestPriorityTitle == AttentionDisplay.title(urgent.item))
        #expect(summary.highestPriorityLabel == "Urgent")
        #expect(summary.waitingLongestID == older.item.id)
        #expect(summary.waitingLongestTitle == AttentionDisplay.title(older.item))
        #expect(summary.waitingLongestItem == older.item)
        #expect(summary.activeTaskCount == 3)
        #expect(summary.daemonState == .connected)
    }

    @Test func contractMismatchFreshnessMapsToItsDaemonState() {
        let summary = OperationalSummary(
            openSnapshots: [],
            tasks: [],
            freshness: .contractMismatch(daemonContract: "sha256:" + String(repeating: "a", count: 64)))
        #expect(summary.daemonState == .contractMismatch)
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
            openSnapshots: [older, blocked], tasks: [], freshness: .fresh)

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
        let summary = OperationalSummary(openSnapshots: [due], tasks: [], freshness: .fresh)

        #expect(summary.waitingLongestValue(now: now) == "\(AttentionDisplay.title(due.item)) · due 2h")
    }

    @Test func emptyInboxNamesNoItems() {
        let summary = OperationalSummary(openSnapshots: [], tasks: [], freshness: .fresh)

        #expect(summary.highestPriorityID == nil)
        #expect(summary.waitingLongestID == nil)
        #expect(summary.waitingLongestValue(now: Date()) == nil)
    }

    @Test func activeCountUsesTaskLifecycleAndCountsANullLifecycleAsActive() throws {
        var unstarted = try #require(TaskFixtures.defaultTasks().first)
        unstarted.task.id = "task-unstarted"
        unstarted.task.lifecycle = nil
        unstarted.task.current_position = nil
        unstarted.task.run_ids = []
        unstarted.task.campaign_ids = []
        let tasks = TaskFixtures.defaultTasks() + [unstarted]
        let summary = OperationalSummary(openSnapshots: [], tasks: tasks, freshness: .fresh)
        #expect(summary.activeTaskCount == TaskListFilter().count(in: tasks, scope: .active))
        #expect(summary.activeTaskCount == 4)

        let finished = try #require(tasks.first { $0.task.lifecycle == .finished })
        let finishedOnly = OperationalSummary(openSnapshots: [], tasks: [finished], freshness: .fresh)
        let unstartedOnly = OperationalSummary(openSnapshots: [], tasks: [unstarted], freshness: .fresh)
        #expect(finishedOnly.activeTaskCount == 0)
        #expect(unstartedOnly.activeTaskCount == 1)
    }

    @Test func retainedOpenProjectionDoesNotReapplyLiveStatus() {
        var retained = AttentionFixtures.fixture(type: .execution_failure)
        retained.item.status = .resolved

        let summary = OperationalSummary(
            openSnapshots: [retained],
            tasks: [],
            freshness: .fresh)

        #expect(summary.openCount == 1)
        #expect(summary.highestPriorityTitle == AttentionDisplay.title(retained.item))
        #expect(summary.waitingLongestTitle == AttentionDisplay.title(retained.item))
    }
}
