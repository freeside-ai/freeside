import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

@Suite struct UnifiedListTests {
    private typealias Item = Components.Schemas.AttentionItemSnapshot
    private typealias TaskRow = Components.Schemas.TaskSnapshot

    private func task(
        _ id: String, project: String = "freeside",
        lifecycle: Components.Schemas.Task.lifecyclePayload? = .active, activity: TimeInterval = 0
    ) -> TaskRow {
        var snapshot = TaskFixtures.defaultTasks()[0]
        snapshot.task.id = id
        snapshot.task.project_id = project
        snapshot.task.lifecycle = lifecycle
        snapshot.task.last_activity_at = Date(timeIntervalSince1970: activity)
        return snapshot
    }

    private func item(
        _ id: String, status: Components.Schemas.ItemStatus = .open, project: String = "freeside",
        created: TimeInterval = 0
    ) -> Item {
        var snapshot = AttentionFixtures.fixture(type: .agent_question)
        snapshot.item.id = id
        snapshot.item.status = status
        snapshot.item.project_id = project
        snapshot.item.created_at = Date(timeIntervalSince1970: created)
        snapshot.item.decided_at = nil
        return snapshot
    }

    private func taskItem(_ id: String, task taskID: String, created: TimeInterval = 0) -> Item {
        var snapshot = item(id, created: created)
        snapshot.item.subject = .task(.init(subject_type: .task, subject_id: taskID, task_id: taskID))
        return snapshot
    }

    private func runItem(_ id: String, task taskID: String?) -> Item {
        var snapshot = item(id)
        snapshot.item.subject = .run(
            .init(subject_type: .run, subject_id: "run-1", run_id: "run-1", task_id: taskID))
        return snapshot
    }

    private func systemItem(_ id: String) -> Item {
        var snapshot = item(id, project: "")
        snapshot.item.subject = .system(.init(subject_type: .system, subject_id: "system"))
        return snapshot
    }

    private func projectItem(_ id: String, project: String) -> Item {
        var snapshot = item(id, project: project)
        snapshot.item.subject = .project(.init(subject_type: .project, subject_id: project))
        return snapshot
    }

    @Test func aTaskWithOpenItemsIsOneRowLedByItsNewest() {
        let list = UnifiedList(
            open: [
                taskItem("newest", task: "task-a", created: 30),
                projectItem("scoped", project: "freeside"),
                taskItem("older", task: "task-a", created: 10),
            ],
            closed: [], tasks: [task("task-a")], projectFilter: nil)

        #expect(list.needsYou.map(\.id) == [.task("task-a"), .item("scoped")])
        #expect(list.needsYou.first?.destination == .attentionItem("newest"))
        #expect(list.working.isEmpty)
    }

    @Test func eachSubjectThatNamesAHeldTaskJoinsIt() {
        var batch = runItem("batch", task: "task-a")
        batch.item.subject = .proposal_batch(
            .init(subject_type: .proposal_batch, subject_id: "batch", task_id: "task-a"))
        let list = UnifiedList(
            open: [runItem("run", task: "task-a"), batch, taskItem("task", task: "task-a")],
            closed: [], tasks: [task("task-a")], projectFilter: nil)

        #expect(list.needsYou.map(\.id) == [.task("task-a")])
    }

    @Test func anItemNamingATaskTheClientDoesNotHoldStandsAlone() {
        let list = UnifiedList(
            open: [runItem("orphan", task: "task-missing"), runItem("no-task", task: nil)],
            closed: [], tasks: [], projectFilter: nil)

        #expect(list.needsYou.map(\.id) == [.item("orphan"), .item("no-task")])
    }

    @Test func aTerminalTaskWithAnOpenItemIsInNeedsYouAlone() {
        let list = UnifiedList(
            open: [taskItem("item", task: "task-done")], closed: [],
            tasks: [task("task-done", lifecycle: .finished)], projectFilter: nil)

        #expect(list.needsYou.map(\.id) == [.task("task-done")])
        #expect(list.finished.isEmpty)
    }

    @Test func quietTasksSplitByLifecycleAndOpenTheirTimelines() {
        let list = UnifiedList(
            open: [], closed: [],
            tasks: [
                task("task-working"), task("task-unstarted", lifecycle: nil),
                task("task-finished", lifecycle: .finished),
                task("task-abandoned", lifecycle: .abandoned),
                task("task-stopped", lifecycle: .stopped),
            ],
            projectFilter: nil)

        #expect(Set(list.working.map(\.id)) == [.task("task-working"), .task("task-unstarted")])
        #expect(
            Set(list.finished.map(\.id))
                == [.task("task-finished"), .task("task-abandoned"), .task("task-stopped")])
        #expect(Set(list.working.map(\.destination)) == [.task("task-working"), .task("task-unstarted")])
    }

    @Test func finishedMergesTasksAndClosedScopedItemsNewestFirst() {
        var decided = projectItem("decided", project: "freeside")
        decided.item.status = .resolved
        decided.item.decided_at = Date(timeIntervalSince1970: 20)
        var expired = projectItem("expired", project: "freeside")
        expired.item.status = .expired
        expired.item.created_at = Date(timeIntervalSince1970: 40)
        let list = UnifiedList(
            open: [], closed: [decided, expired],
            tasks: [
                task("task-old", lifecycle: .finished, activity: 10),
                task("task-new", lifecycle: .finished, activity: 30),
            ],
            projectFilter: nil)

        #expect(
            list.finished.map(\.id) == [
                .item("expired"), .task("task-new"), .item("decided"), .task("task-old"),
            ])
    }

    @Test func aClosedItemOfAHeldTaskLeavesTheList() {
        var closed = taskItem("closed", task: "task-a")
        closed.item.status = .resolved
        let list = UnifiedList(open: [], closed: [closed], tasks: [task("task-a")], projectFilter: nil)

        #expect(list.finished.isEmpty)
        #expect(list.selection(attentionItemID: "closed", taskID: nil) == .task("task-a"))
    }

    @Test func aProjectFilterKeepsSystemRowsAndFollowsProjectRows() {
        let open = [
            systemItem("system"), projectItem("mine", project: "freeside"),
            projectItem("other", project: "oriole"), taskItem("other-task-item", task: "task-other"),
        ]
        let tasks = [task("task-mine"), task("task-other", project: "oriole")]
        let list = UnifiedList(open: open, closed: [], tasks: tasks, projectFilter: "freeside")

        #expect(list.needsYou.map(\.id) == [.item("system"), .item("mine")])
        #expect(list.working.map(\.id) == [.task("task-mine")])
    }

    @Test func aFilteredOutTaskKeepsItsItemsFromReadingAsScopedRows() {
        let list = UnifiedList(
            open: [taskItem("item", task: "task-other")], closed: [],
            tasks: [task("task-other", project: "oriole")], projectFilter: "freeside")

        #expect(list.rows.isEmpty)
    }

    @Test func noProjectKeepsScopedItemsWithoutAProjectAndNoTask() {
        var unnamed = projectItem("unnamed", project: "")
        unnamed.item.subject = .project(.init(subject_type: .project, subject_id: ""))
        let list = UnifiedList(
            open: [unnamed, projectItem("named", project: "freeside")], closed: [],
            tasks: [task("task-a")], projectFilter: UnifiedList.noProject)

        #expect(list.needsYou.map(\.id) == [.item("unnamed")])
        #expect(list.working.isEmpty)
    }

    @Test func noProjectIsOfferedOnlyWhenAHeldItemNamesNone() {
        let tasks = [task("task-a")]
        #expect(
            !UnifiedList(
                open: [projectItem("named", project: "freeside")], closed: [], tasks: tasks,
                projectFilter: nil
            ).offersNoProject)
        #expect(
            UnifiedList(open: [], closed: [systemItem("system")], tasks: tasks, projectFilter: nil)
                .offersNoProject)
        // An item under a held task is the task's, so it isn't a scoped row.
        var underTask = taskItem("under-task", task: "task-a")
        underTask.item.project_id = ""
        #expect(!UnifiedList(open: [underTask], closed: [], tasks: tasks, projectFilter: nil).offersNoProject)
    }

    @Test func knownProjectsUnionItemsAndTasksWhateverTheFilter() {
        let list = UnifiedList(
            open: [projectItem("item", project: "beta"), systemItem("system")],
            closed: [projectItem("closed", project: "gamma")],
            tasks: [task("task-a", project: "alpha"), task("task-b", project: "beta")],
            projectFilter: "alpha")

        #expect(list.knownProjects == ["alpha", "beta", "gamma"])
    }

    @Test func nextItemSkipsTheConcludedItemAndOneConcludedLocally() {
        var concluded = taskItem("concluded", task: "task-a", created: 30)
        concluded.item.status = .resolved
        let list = UnifiedList(
            open: [
                concluded, taskItem("current", task: "task-b", created: 20),
                taskItem("next", task: "task-a", created: 10),
            ],
            closed: [], tasks: [task("task-a"), task("task-b")], projectFilter: nil)

        #expect(list.nextOpenItemID(excluding: "current") == "next")
        #expect(list.nextOpenItemID(inTask: "task-a", excluding: "current") == "next")
        #expect(list.nextOpenItemID(inTask: "task-b", excluding: "current") == nil)
    }

    @Test func conclusionAdvanceStaysInsideTheFilteredProject() {
        let list = UnifiedList(
            open: [
                projectItem("other", project: "oriole"), projectItem("current", project: "freeside"),
                projectItem("same", project: "freeside"),
            ],
            closed: [], tasks: [], projectFilter: "freeside")

        #expect(list.nextOpenItemID(excluding: "current") == "same")
    }

    @Test func workingKeywordsFollowTheStageAndNeverHideAStatus() {
        var snapshot = TaskFixtures.defaultTasks()[0].task
        snapshot.lifecycle = .active
        snapshot.cancellation = nil
        func row(_ status: String, stage: Components.Schemas.StageName?) -> TaskDisplay.SectionRow {
            TaskDisplay.workingRow(
                snapshot,
                position: position(status: status),
                stage: stage.map { .init(stage: $0, round: nil) })
        }
        #expect(row("In Progress", stage: .specification).keyword == "Specifying")
        #expect(row("In Progress", stage: .implementation).keyword == "Implementing")
        #expect(row("In Progress", stage: .review).keyword == "Reviewing")
        #expect(row("In Progress", stage: .verification).keyword == "Verifying")
        #expect(row("Queued", stage: .implementation).keyword == "Parked")
        #expect(row("On Hold", stage: .implementation).keyword == "Parked")
        #expect(row("Execution Failed", stage: .implementation).keyword == "Execution Failed")
        #expect(row("Stopping", stage: .review).keyword == "Stopping")
    }

    @Test func aParkedRowNamesItsHoldAndAFinishedRowIsFaint() {
        var snapshot = TaskFixtures.defaultTasks()[0].task
        snapshot.cancellation = nil
        let parked = TaskDisplay.workingRow(
            snapshot, position: position(status: "On Hold", hold: .usage_pool_exhausted), stage: nil)
        #expect(parked.context.hasSuffix(RunDisplay.label(.usage_pool_exhausted)))
        #expect(TaskDisplay.finishedRow(snapshot, position: nil).cut == .faint)
    }

    private func position(
        status: String, hold: Components.Schemas.RunHoldReason? = nil
    ) -> TaskDisplay.Position {
        TaskDisplay.Position(
            heading: nil, rail: .init(entries: [], summary: ""), holdReason: hold, status: status)
    }
}
