import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #2651: the task page's Change Agent control offers what New Task's role
/// chips offer, with the role's current line chosen, and shows only while
/// the task is active.
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

    @Test func eachRoleOffersWhatTheComposersRoleChipOffersWithItsLineChosen() async throws {
        let coordinator = await coordinator()
        let submission = TaskSubmissionModel(coordinator: coordinator)
        #expect(coordinator.agentCatalog != nil && !coordinator.usagePools.isEmpty)
        var task = try #require(coordinator.tasks.first).task
        task.task_lines = [line(.reviewer, taskID: task.id, selection: Self.codex)]

        for role in AgentChoiceDisplay.roleOrder {
            let page = TaskLineChangeMenu.offer(role, task: task, coordinator: coordinator)
            let chosen = task.task_lines.first { $0.role == role }?.selection?.value1
            let composer = NewTaskSheet.roleMenuOffer(
                role, choices: chosen.map { [role: $0] } ?? [:], model: submission)
            // Same catalog, pools, and chosen selection: the shared menu draws
            // the same enrollments, models, and efforts from them.
            #expect(page == composer)
        }
        #expect(TaskLineChangeMenu.offer(.reviewer, task: task, coordinator: coordinator).current == Self.codex)
        #expect(TaskLineChangeMenu.offer(.implementer, task: task, coordinator: coordinator).current == nil)

        // An enrollment pick from the task's line applies New Task's reset
        // rule; picking what the line holds sends nothing.
        let reviewer = TaskLineChangeMenu.offer(.reviewer, task: task, coordinator: coordinator)
        #expect(
            RoleSelectionMenu.choice(.enrollment(UsageFixtures.readingCollectorID), in: reviewer)
                == .init(
                    selection: AgentChoiceDisplay.choosing(
                        enrollment: UsageFixtures.readingCollectorID, role: .reviewer,
                        catalog: coordinator.agentCatalog)))
        #expect(RoleSelectionMenu.choice(.enrollment(Self.codex.enrollment_id), in: reviewer) == nil)
        #expect(RoleSelectionMenu.choice(.effort(Self.codex.effort), in: reviewer) == nil)
        #expect(RoleSelectionMenu.choice(.enrollment(nil), in: reviewer) == .init(selection: nil))
        // A role on the lineup has nothing to clear.
        let specifier = TaskLineChangeMenu.offer(.specifier, task: task, coordinator: coordinator)
        #expect(RoleSelectionMenu.choice(.enrollment(nil), in: specifier) == nil)
    }

    /// A line recorded by agent name before #2597 holds no selection: the
    /// menu shows its agent as the current entry, and any pick, Clear
    /// included, replaces it.
    @Test func aLineSetByNameShowsItsAgentAndAnyPickReplacesIt() async throws {
        let coordinator = await coordinator()
        var task = try #require(coordinator.tasks.first).task
        task.task_lines = [line(.implementer, taskID: task.id, agent: "claude-primary")]

        let offer = TaskLineChangeMenu.offer(.implementer, task: task, coordinator: coordinator)
        #expect(offer.current == nil && offer.byName == "claude-primary")
        #expect(RoleSelectionMenu.choice(.enrollment(nil), in: offer) == .init(selection: nil))
        #expect(
            RoleSelectionMenu.choice(.enrollment(Self.codex.enrollment_id), in: offer)
                == .init(
                    selection: AgentChoiceDisplay.choosing(
                        enrollment: Self.codex.enrollment_id, role: .implementer, catalog: coordinator.agentCatalog)))
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
        let control = TaskLineChangeMenu(coordinator: coordinator, task: task)
        let menu = TaskActionsMenu(coordinator: coordinator, taskID: task.id, confirmation: .constant(nil))
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
            TaskLineChangeMenu(coordinator: coordinator, task: waiting).blockedReason
                == "An agent change is still waiting for the daemon.")
        #expect(
            TaskLineChangeMenu(coordinator: coordinator, task: other).blockedReason
                == "An agent change on \u{201C}\(waiting.display_names.task.text)\u{201D} is still waiting for the "
                + "daemon. Retry it from that task's page.")
    }
}
