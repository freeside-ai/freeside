import Foundation
import FreesideAPI
import FreesideTestSupport
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #2651, #3027: the task page's Change Agent opens the picker New Task's
/// role chips open, holding the role's current line, shows only while the
/// task is active, and sends one change per confirm.
@Suite @MainActor struct TaskLineChangeMenuTests {
    private static let codex = UsageFixtures.agentSelection("codex-primary")

    private func coordinator() async -> SyncCoordinator {
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()), cache: InMemoryCacheStore())
        await coordinator.refresh()
        return coordinator
    }

    private func line(
        _ role: Components.Schemas.TaskLineRole, taskID: String, selection: AgentChoiceDisplay.Selection? = nil,
        agent: String? = nil
    ) -> Components.Schemas.TaskLine {
        .init(
            id: "sha256:" + String(repeating: "b", count: 64), task_id: taskID, role: role, agent: agent,
            selection: selection.map { .init(value1: $0) }, version: 1, predecessor_id: nil, source: .submit_task,
            set_by: "command-seeded", set_at: Date(timeIntervalSince1970: 0), provenance: nil)
    }

    @Test func eachRoleOpensWhatTheComposersRoleChipOpensWithItsLineHeld() async throws {
        let coordinator = await coordinator()
        let submission = TaskSubmissionModel(coordinator: coordinator)
        #expect(coordinator.agentCatalog != nil && !coordinator.usagePools.isEmpty)
        // The page's picker draws from the sources New Task's does.
        #expect(submission.agentCatalog == coordinator.agentCatalog)
        #expect(submission.usagePools == coordinator.usagePools)
        #expect(submission.routingPolicy == coordinator.routingPolicy?.policy)
        var task = try #require(coordinator.tasks.first).task
        task.task_lines = [line(.reviewer, taskID: task.id, selection: Self.codex)]

        for role in AgentChoiceDisplay.roleOrder {
            let page = TaskLineChangeMenu.picker(role, task: task, coordinator: coordinator)
            let chosen = task.task_lines.first { $0.role == role }?.selection?.value1
            let composer = NewTaskSheet.rolePicker(role, choices: chosen.map { [role: $0] } ?? [:], model: submission)
            // The same staged pick and the same enrollments, models, and
            // efforts; only the lineup row's name differs (Clear, Default).
            #expect(page.staged == composer.staged)
            #expect(page.mode == .change(current: chosen, byName: nil))
            let catalog = coordinator.agentCatalog
            let rows = { (picker: RolePicker) in
                picker.enrollmentRows(
                    catalog: catalog, policy: submission.routingPolicy, pools: coordinator.usagePools, now: Date()
                ).dropFirst()
            }
            #expect(rows(page) == rows(composer))
            #expect(page.modelPills(catalog: catalog) == composer.modelPills(catalog: catalog))
            #expect(page.effortPills(catalog: catalog) == composer.effortPills(catalog: catalog))
        }
        #expect(
            TaskLineChangeMenu.picker(.reviewer, task: task, coordinator: coordinator).staged == .selection(Self.codex))
        #expect(TaskLineChangeMenu.picker(.implementer, task: task, coordinator: coordinator).staged == .lineup)
    }

    /// A line recorded by agent name before #2597 holds no selection: the
    /// picker holds its agent, and any pick, Clear included, replaces it.
    @Test func aLineSetByNameIsHeldAndAnyPickReplacesIt() async throws {
        let coordinator = await coordinator()
        var task = try #require(coordinator.tasks.first).task
        task.task_lines = [line(.implementer, taskID: task.id, agent: "claude-primary")]
        let catalog = coordinator.agentCatalog

        var picker = TaskLineChangeMenu.picker(.implementer, task: task, coordinator: coordinator)
        #expect(picker.staged == .byName("claude-primary"))
        #expect(picker.mode == .change(current: nil, byName: "claude-primary"))
        picker.pick(.enrollment(nil), catalog: catalog)
        #expect(picker.confirmation(catalog: catalog) == .ready(nil))
        picker.pick(.enrollment(Self.codex.enrollment_id), catalog: catalog)
        #expect(
            picker.confirmation(catalog: catalog)
                == .ready(
                    AgentChoiceDisplay.choosing(
                        enrollment: Self.codex.enrollment_id, role: .implementer, catalog: catalog)))
    }

    /// The confirm sends one change and closes the picker; a second confirm
    /// before the first lands finds it closed and sends nothing.
    @Test func aConfirmSendsOneChangeAndASecondSendsNothing() async throws {
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.refresh()
        let task = try #require(coordinator.tasks.first { TaskLineChangeMenu.offers($0.task) }).task
        let submits = SubmitCounter()
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { await submits.add() }
        }
        let opened = TaskLineChangeRequest(taskID: task.id, role: .implementer)
        var held: TaskLineChangeRequest? = opened
        let request = Binding(get: { held }, set: { held = $0 })

        let first = TaskLineChangePicker.send(Self.codex, for: opened, request: request, coordinator: coordinator)
        let second = TaskLineChangePicker.send(Self.codex, for: opened, request: request, coordinator: coordinator)
        #expect(held == nil)
        #expect(second == nil)
        await first?.value
        #expect(await submits.count == 1)
    }

    /// An open picker turns its confirm off with a reason once the task
    /// takes no change: a sync gone stale, or the task stopped or gone.
    @Test func anOpenPickersConfirmFollowsTheTask() async throws {
        let coordinator = await coordinator()
        let task = try #require(coordinator.tasks.first { TaskLineChangeMenu.offers($0.task) }).task
        let picker = { (taskID: String) in
            TaskLineChangePicker(
                request: .init(taskID: taskID, role: .implementer), coordinator: coordinator, onConfirm: { _ in },
                onCancel: {})
        }
        #expect(picker(task.id).unavailableReason == nil)
        #expect(picker("task-missing").unavailableReason == TaskLineChangePicker.endedReason)
        coordinator.store.freshness = .syncFailing
        #expect(picker(task.id).unavailableReason == "The task isn't current. Refresh before changing the agent.")
    }

    @Test func theControlShowsOnlyWhileTheTaskIsActiveAndUnstopped() throws {
        var task = try #require(TaskFixtures.defaultTasks().first).task
        task.lifecycle = .active
        #expect(TaskLineChangeMenu.offers(task))
        for lifecycle in [Components.Schemas.Task.lifecyclePayload.finished, .stopped, .abandoned] {
            task.lifecycle = lifecycle
            #expect(!TaskLineChangeMenu.offers(task))
        }
        task.lifecycle = .active
        task = TaskFixtures.confirmedStopped(.init(as_of_revision: 1, entity_version: 1, task: task)).task
        task.lifecycle = .active
        #expect(!TaskLineChangeMenu.offers(task))
        #expect(!TaskLineChangeMenu.offers(nil))
    }

    /// Stale sync disables Change Agent with its reason as a menu item, and
    /// keeps the Task Actions menu enabled so the reason can be read.
    @Test func staleSyncDisablesTheChangeWithAReason() async throws {
        let coordinator = await coordinator()
        let task = try #require(coordinator.tasks.first { TaskLineChangeMenu.offers($0.task) }).task
        let control = TaskLineChangeMenu(coordinator: coordinator, task: task, request: .constant(nil))
        let menu = TaskActionsMenu(
            coordinator: coordinator, taskID: task.id, confirmation: .constant(nil), lineChange: .constant(nil))
        #expect(control.blockedReason == nil)
        #expect(menu.offersLineChange && menu.drawsMenu && !menu.isDisabled)

        coordinator.store.freshness = .syncFailing
        #expect(control.blockedReason == "The task isn't current. Refresh before changing the agent.")
        #expect(menu.stopIsUnavailable)
        #expect(!menu.isDisabled)
    }

    /// A device sends one change at a time. While one waits unanswered,
    /// another task's control names the task holding it, whose page alone
    /// offers its Retry.
    @Test func aChangeWaitingOnAnotherTaskIsNamedWhereItBlocks() async throws {
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.refresh()
        let active = coordinator.tasks.map(\.task).filter { TaskLineChangeMenu.offers($0) }
        try #require(active.count >= 2)
        let (waiting, other) = (active[0], active[1])
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        await coordinator.taskLineChange.change(taskID: waiting.id, role: .implementer, to: Self.codex)
        #expect(coordinator.pendingTaskLineChange?.receipt == nil)

        #expect(
            TaskLineChangeMenu.blockedReason(taskID: waiting.id, coordinator: coordinator)
                == "An agent change is still waiting for the daemon.")
        #expect(
            TaskLineChangeMenu.blockedReason(taskID: other.id, coordinator: coordinator)
                == "An agent change on \u{201C}\(waiting.display_names.task.text)\u{201D} is still waiting for the "
                + "daemon. Retry it from that task's page.")
    }
}

/// Counts the mock server's command submissions.
private actor SubmitCounter {
    private(set) var count = 0
    func add() { count += 1 }
}
