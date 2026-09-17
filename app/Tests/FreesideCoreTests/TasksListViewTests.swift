import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit
    import Observation
    import SwiftUI
#endif

@Suite struct TasksListViewTests {
    @Test func publishedHandoffRequiresCurrentBoundReadiness() throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.readyRunID })
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.id == run.run.task_id }).task
        let ready = AttentionFixtures.publishedTaskReady()
        for snapshots in [[run], []] {
            func heading(_ item: Components.Schemas.AttentionItemSnapshot) -> TaskDisplay.Position.Heading? {
                TaskDisplay.position(task, runs: snapshots, attentionItems: [item])?.heading
            }
            #expect(heading(ready)?.text == "Ready for final review")
            #expect(heading(ready)?.round == nil)
            #expect(
                heading(AttentionFixtures.publishedTaskReady(degraded: true))?.text
                    == "Ready for final review (degraded)")
            let mutations: [(String, (inout Components.Schemas.AttentionItem) -> Void)] = [
                ("wrong project", { $0.project_id = "another-project" }),
                ("wrong type", { $0._type = .publish_blocked }),
                ("missing readiness", { $0.readiness = nil }),
                ("missing detail", { $0.readiness_detail = nil }),
                ("missing PR", { $0.pr_reference = nil }),
                ("missing head", { $0.pr_head_sha = "" }),
                ("different head", { $0.pr_head_sha = "different-head" }),
                ("different verdict", { $0.readiness?.value1.evaluation_set_digest = "different-evaluation" }),
                (
                    "invalidated",
                    { $0.readiness_invalidation = AttentionFixtures.staleReady().item.readiness_invalidation }
                ),
                (
                    "base advanced",
                    {
                        $0.base_freshness = .init(
                            value1: .init(
                                base_ref: "main", admitted_base_sha: "deadbeef", observed_base_sha: "0badf00d",
                                advanced: true, observed_at: AttentionFixtures.createdInstant))
                    }
                ),
                (
                    "foreign task",
                    {
                        $0.subject = .run(
                            .init(subject_type: .run, subject_id: run.run.id, run_id: run.run.id, task_id: "other-task")
                        )
                    }
                ),
                (
                    "older run",
                    {
                        $0.subject = .run(
                            .init(subject_type: .run, subject_id: "older-run", run_id: "older-run", task_id: task.id))
                    }
                ),
                (
                    "missing task binding",
                    {
                        $0.subject = .run(.init(subject_type: .run, subject_id: run.run.id, run_id: run.run.id))
                    }
                ),
                (
                    "missing run binding",
                    {
                        $0.subject = .run(.init(subject_type: .run, subject_id: run.run.id, task_id: task.id))
                    }
                ),
                (
                    "task-only subject",
                    {
                        $0.subject = .task(.init(subject_type: .task, subject_id: task.id, task_id: task.id))
                    }
                ),
            ]
            for (name, mutate) in mutations {
                var changed = ready
                mutate(&changed.item)
                #expect(
                    heading(changed)?.label == "Verification"
                        || (snapshots.isEmpty && heading(changed)?.label == "Implementation"), "\(name)")
            }
            for status in Components.Schemas.ItemStatus.allCases where status != .open {
                var changed = ready
                changed.item.status = status
                #expect(heading(changed)?.label != "Ready for final review", "\(status)")
            }
        }
        #expect(TaskDisplay.isActive(task))
        #expect(TaskListFilter(scope: .active).rows(in: TaskFixtures.defaultTasks()).contains { $0.task.id == task.id })
        #expect(
            !TaskListFilter(scope: .finished).rows(in: TaskFixtures.defaultTasks()).contains { $0.task.id == task.id })
        #expect(TaskDisplay.position(task, runs: [run])?.heading?.text == "Verification · Round 1")
        #expect(
            TaskDisplay.position(task, runs: [run], attentionItems: [ready])?.rail
                == TaskDisplay.position(task, runs: [run])?.rail)
    }

    @Test func lifecycleAndHoldOverrideAnOldHandoff() throws {
        let snapshot = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.readyRunID })
        let task = try #require(TaskFixtures.defaultTasks().first { $0.task.id == snapshot.run.task_id }).task
        let items = [AttentionFixtures.publishedTaskReady()]
        let changes: [(inout Components.Schemas.Run) -> Void] =
            [
                { $0.lifecycle = .finished }, { $0.superseded_by = "successor" },
                { $0.hold_reason = .init(value1: .operation_stopped) },
                { $0.hold_reason = .init(value1: .verification_findings) },
                { $0.task_id = "foreign-task" }, { $0.project_id = "foreign-project" },
            ]
            + Components.Schemas.RunOutcome.allCases.filter { $0 != .published }.map { outcome in
                { $0.outcome = outcome }
            }
        for change in changes {
            var changed = snapshot
            change(&changed.run)
            #expect(
                TaskDisplay.position(task, runs: [changed], attentionItems: items)?.heading?.label == "Verification")
        }
        for runs in [[snapshot], []] {
            var held = task
            held.current_position?.value1.hold_reason = .init(value1: .operation_stopped)
            #expect(
                TaskDisplay.position(held, runs: runs, attentionItems: items)?.heading?.label
                    != "Ready for final review")
            var finished = task
            finished.lifecycle = .finished
            #expect(
                TaskDisplay.position(finished, runs: runs, attentionItems: items)?.heading?.label
                    != "Ready for final review")
        }
        var fallback = task
        fallback.current_position?.value1.round = 3
        #expect(TaskDisplay.position(fallback, runs: [])?.heading?.text == "Implementation · Round 3")
    }

    @Test func fixtureTasksAreNamedAndDistinguishable() throws {
        let freeside = TaskFixtures.defaultTasks().map(\.task).filter { $0.project_id == "freeside" }
        let names = freeside.map(\.display_names.task)

        #expect(freeside.count >= 3)
        #expect(Set(names.map(\.text)).count == names.count)
        let agent = try #require(names.first { $0.source == .agent })
        #expect(agent == RunFixtures.readyTaskName)
        let operatorNamed = try #require(freeside.first { $0.display_names.task.source == ._operator })
        #expect(operatorNamed.display_names.task == RunFixtures.retryTaskName)
        let fallback = try #require(freeside.first { $0.id == TaskFixtures.legacyTaskID })
        #expect(fallback.display_names.task == .init(text: TaskFixtures.legacyTaskID, source: .identifier))
        #expect(names.allSatisfy { $0.source != .name }, "a task name source is never `name`")
    }

    @Test func theRetryCampaignIsOneRowHoldingBothAttempts() throws {
        let rows = TaskListFilter(scope: .all).rows(in: TaskFixtures.defaultTasks())
        let holders = rows.filter {
            $0.task.run_ids.contains("run-freeside-656") || $0.task.run_ids.contains(RunFixtures.activeRunID)
        }

        #expect(holders.map(\.task.id) == [TaskFixtures.retryTaskID])
        let retry = try #require(holders.first).task
        #expect(retry.run_ids == ["run-freeside-656", RunFixtures.activeRunID])
        #expect(retry.current_position?.value1.run_id == RunFixtures.activeRunID)
    }

    @Test func ordersByActivityThenID() {
        let earlier = Date(timeIntervalSinceReferenceDate: 100)
        let later = Date(timeIntervalSinceReferenceDate: 200)
        let tasks = [("task-c", later), ("task-a", earlier), ("task-b", later)].map { id, activity in
            var snapshot = TaskFixtures.defaultTasks()[0]
            snapshot.task.id = id
            snapshot.task.last_activity_at = activity
            return snapshot
        }

        #expect(TaskDisplay.sortedTasks(tasks).map(\.task.id) == ["task-b", "task-c", "task-a"])
    }

    @Test func scopesUseLifecycleAndANullLifecycleCountsAsActive() throws {
        var tasks = TaskFixtures.defaultTasks()
        var unstarted = try #require(tasks.first)
        unstarted.task.id = "task-unstarted"
        unstarted.task.lifecycle = nil
        unstarted.task.current_position = nil
        unstarted.task.run_ids = []
        unstarted.task.campaign_ids = []
        tasks.append(unstarted)

        let active = TaskListFilter().rows(in: tasks.reversed())
        let finished = TaskListFilter(scope: .finished).rows(in: tasks.reversed())

        #expect(active.contains { $0.task.id == unstarted.task.id })
        #expect(active.allSatisfy { $0.task.lifecycle != .finished })
        #expect(finished.allSatisfy { $0.task.lifecycle == .finished })
        #expect(finished.contains { $0.task.id == TaskFixtures.legacyTaskID })
        #expect(active.count + finished.count == tasks.count)
        #expect(TaskListFilter().scope == .active)
        #expect(
            TaskListFilter(scope: .all).rows(in: tasks.reversed()).map(\.task.id)
                == TaskDisplay.sortedTasks(tasks).map(\.task.id))
    }

    @Test func scopeCountsMatchRowsWithinTheProjectFilter() {
        let tasks = TaskFixtures.defaultTasks()
        for project in [nil, "freeside", "oriole", "missing"] as [String?] {
            for scope in TaskListFilter.Scope.allCases {
                let filter = TaskListFilter(scope: scope, projectID: project)
                let rows = filter.rows(in: tasks)
                #expect(rows.allSatisfy { project == nil || $0.task.project_id == project })
                #expect(filter.count(in: tasks, scope: scope) == rows.count)
                #expect(
                    filter.count(in: tasks, scope: .all)
                        == filter.count(in: tasks, scope: .active) + filter.count(in: tasks, scope: .finished))
                #expect(filter.rows(in: []).isEmpty)
                #expect(filter.count(in: [], scope: scope) == 0)
            }
        }
        let finishedProject = TaskListFilter(scope: .active, projectID: "oriole")
        #expect(finishedProject.rows(in: tasks).isEmpty)
        #expect(finishedProject.count(in: tasks, scope: .finished) > 0)
    }

    @Test func explicitTaskLinksRevealTheirScopeAndProject() throws {
        let tasks = TaskFixtures.defaultTasks()
        let finished = try #require(tasks.first { $0.task.id == "task-campaign-freeside-completed" }).task
        let active = try #require(tasks.first { $0.task.id == TaskFixtures.retryTaskID }).task
        var filter = TaskListFilter(projectID: "oriole")
        filter.reveal(finished)
        #expect(filter.scope == .finished)
        #expect(filter.projectID == nil)

        filter.reveal(active)
        #expect(filter.scope == .active)
        filter = TaskListFilter(scope: .all, projectID: active.project_id)
        filter.reveal(active)
        #expect(filter.scope == .all)
        #expect(filter.projectID == active.project_id)
    }

    @Test @MainActor func changingScopeRepairsTheWholeStackAgainstVisibleTasks() {
        let tasks = TaskFixtures.defaultTasks()
        let path = ["task-campaign-freeside-completed", RunFixtures.completedRunID]
        let activeIDs = Set(TaskListFilter().rows(in: tasks).map(\.task.id))
        let finishedIDs = Set(TaskListFilter(scope: .finished).rows(in: tasks).map(\.task.id))
        #expect(NavigationModel.repairedTaskPath(path, availableTaskIDs: activeIDs).isEmpty)
        #expect(NavigationModel.repairedTaskPath(path, availableTaskIDs: finishedIDs) == path)
        #expect(NavigationModel.repairedTaskPath([], availableTaskIDs: activeIDs).isEmpty)
    }

    @Test func stageLineFollowsTheListedRunAndFallsBackToTheDaemonPosition() throws {
        let runs = RunFixtures.defaultRuns()
        let active = try #require(runs.first { $0.run.id == RunFixtures.activeRunID }).run
        let retry = try #require(TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.retryTaskID }).task

        let listed = try #require(TaskDisplay.position(retry, runs: runs))
        #expect(listed.heading == .init(label: "Verification", round: "Round 1"))
        #expect(listed.heading?.text == RunDisplay.title(active))
        #expect(listed.rail.entries == RunDisplay.stageRail(active).entries)
        #expect(
            listed.rail.summary
                == "Specification approval history unavailable, Implementation completed, Review completed, Verification current"
        )
        #expect(listed.hold == "Verification findings block publication")

        let fallback = try #require(TaskDisplay.position(retry, runs: []))
        #expect(fallback.heading == .init(label: "Implementation", round: nil))
        #expect(fallback.hold == "Verification findings block publication")
        #expect(
            fallback.rail.entries.map { ($0.id, $0.state) }.map { "\($0.0):\($0.1)" }
                == ["specification:pending", "implementation:current", "review:pending", "verification:pending"])

        var positioned = retry
        positioned.current_position = .init(
            value1: .init(run_id: "run-elsewhere", stage: "implement", round: 3, hold_reason: nil))
        let daemonShaped = try #require(TaskDisplay.position(positioned, runs: []))
        #expect(daemonShaped.heading == .init(label: "Implementation", round: "Round 3"))
        #expect(daemonShaped.hold == nil)

        var unpositioned = retry
        unpositioned.current_position = nil
        #expect(TaskDisplay.position(unpositioned, runs: runs) == nil)
    }

    @Test func attachedWatchesFollowTheTasksRuns() throws {
        let tasks = TaskFixtures.defaultTasks()
        let schedules = RunFixtures.defaultSchedules()
        let retry = try #require(tasks.first { $0.task.id == TaskFixtures.retryTaskID }).task
        let ready = try #require(tasks.first { $0.task.id == "task-campaign-freeside-ready" }).task
        let legacy = try #require(tasks.first { $0.task.id == TaskFixtures.legacyTaskID }).task

        let retryWatches = TaskDisplay.armedSchedules(for: retry, in: schedules)
        #expect(retryWatches.map(\.schedule.run_id).allSatisfy { $0 == RunFixtures.activeRunID })
        #expect(Set(retryWatches.map(\.schedule.kind)) == [.pr_checks_deadline, .review_wait_threshold])
        #expect(retryWatches.allSatisfy { $0.schedule.status == .armed })
        #expect(TaskDisplay.armedSchedules(for: ready, in: schedules).map(\.schedule.kind) == [.base_advance_watch])
        #expect(TaskDisplay.armedSchedules(for: legacy, in: schedules).isEmpty)

        // A schedule that is no longer armed stays off the row.
        var fired = schedules
        for index in fired.indices { fired[index].schedule.status = .fired }
        #expect(TaskDisplay.armedSchedules(for: retry, in: fired).isEmpty)
    }

    @Test func metaLineCarriesProjectIssueAndLastActive() throws {
        let tasks = TaskFixtures.defaultTasks()
        let retry = try #require(tasks.first { $0.task.id == TaskFixtures.retryTaskID }).task
        let legacy = try #require(tasks.first { $0.task.id == TaskFixtures.legacyTaskID }).task
        let now = retry.last_activity_at.addingTimeInterval(30 * 60)

        #expect(TaskDisplay.metaLine(retry, now: now) == "freeside · #724 · last active 30m ago")
        let dated = TaskDisplay.metaLine(legacy, now: legacy.last_activity_at.addingTimeInterval(3 * 86_400))
        #expect(dated.hasPrefix("freeside · last active "))
        #expect(TaskDisplay.issueReference(legacy) == nil)
        #expect(TaskDisplay.sourceLine(retry) == "Source: freeside-ai/freeside#724")
        #expect(TaskDisplay.sourceLine(legacy) == "Source: none recorded")
        #expect(TaskDisplay.exactActivityTimestamp(retry) == retry.last_activity_at.formatted(.iso8601))
    }

    #if os(macOS)
        @Test @MainActor func visibleRowsRequestHistoryWithoutSelection() throws {
            let fixture = TaskFixtures.approvedCampaign()
            let state = TaskListProbeState(tasks: [fixture.task], selection: nil)
            let host = NSHostingView(rootView: TaskListProbe(state: state))
            let window = NSWindow(
                contentRect: NSRect(x: 0, y: 0, width: 400, height: 600),
                styleMask: [.titled], backing: .buffered, defer: false)
            window.contentView = host
            window.orderFront(nil)
            defer { window.orderOut(nil) }
            host.setFrameSize(NSSize(width: 400, height: 600))
            host.layoutSubtreeIfNeeded()
            try #require(pumpUntil { state.requests[fixture.task.task.id] == 1 })
            #expect(state.selection == nil)
            state.tasks[0].as_of_revision += 1
            try #require(pumpUntil { state.requests[fixture.task.task.id] == 2 })
            withExtendedLifetime(host) {}
        }

        @Test @MainActor func finishedSelectionSurvivesColdSnapshotLoad() throws {
            let state = TaskListProbeState(tasks: [], selection: "task-campaign-freeside-completed")
            let host = NSHostingView(rootView: TaskListProbe(state: state))
            host.setFrameSize(NSSize(width: 400, height: 600))
            host.layoutSubtreeIfNeeded()
            try #require(pumpUntil { state.appeared })

            state.tasks = TaskFixtures.defaultTasks()
            try #require(pumpUntil { state.updates > 0 })
            #expect(state.selection == "task-campaign-freeside-completed")
            withExtendedLifetime(host) {}
        }

        @Test @MainActor func lifecycleUpdateKeepsTheOpenDetailSelected() throws {
            let state = TaskListProbeState(
                tasks: TaskFixtures.defaultTasks(), selection: TaskFixtures.retryTaskID)
            let host = NSHostingView(rootView: TaskListProbe(state: state))
            host.setFrameSize(NSSize(width: 400, height: 600))
            host.layoutSubtreeIfNeeded()
            try #require(pumpUntil { state.appeared })

            let index = try #require(state.tasks.firstIndex { $0.task.id == TaskFixtures.retryTaskID })
            state.tasks[index].task.lifecycle = .finished
            try #require(pumpUntil { state.updates > 0 })
            #expect(state.selection == TaskFixtures.retryTaskID)
            withExtendedLifetime(host) {}
        }

        @MainActor private func pumpUntil(_ condition: () -> Bool) -> Bool {
            let deadline = Date().addingTimeInterval(2)
            while !condition(), Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
            }
            return condition()
        }
    #endif
}

#if os(macOS)
    @MainActor @Observable private final class TaskListProbeState {
        var tasks: [Components.Schemas.TaskSnapshot]
        var selection: String?
        var appeared = false
        var updates = 0
        var requests: [String: Int] = [:]

        init(tasks: [Components.Schemas.TaskSnapshot], selection: String?) {
            self.tasks = tasks
            self.selection = selection
        }
    }

    @MainActor private struct TaskListProbe: View {
        @Bindable var state: TaskListProbeState

        var body: some View {
            TasksListView(
                tasks: state.tasks, runs: [], schedules: [],
                onLoadTimeline: { state.requests[$0, default: 0] += 1 }, selection: $state.selection
            )
            .onAppear { state.appeared = true }
            .onChange(of: state.tasks.map(\.task.id)) { state.updates += 1 }
            .onChange(of: state.tasks.map(\.task.lifecycle)) { state.updates += 1 }
        }
    }
#endif
