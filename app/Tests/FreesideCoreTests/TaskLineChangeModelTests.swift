import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// #2651: the task page changes one role's line, or clears it back to the
/// lineup, through a saved `change_task_line` command that settles once sync
/// shows the revision the daemon recorded it at. A change refused because
/// the role runs says so and refreshes through sync; nothing resends on its
/// own.
@Suite @MainActor struct TaskLineChangeModelTests {
    private static let codex = UsageFixtures.agentSelection("codex-primary")

    private func server() async throws -> (server: MockServer, taskID: String) {
        let server = MockServer()
        let task = try #require(
            try await APIClientFactory.mock(server: server).getSyncBootstrap().ok.body.json.tasks.first)
        return (server, task.task.id)
    }

    private func coordinator(
        _ server: MockServer, cache: any CacheStore = InMemoryCacheStore()
    ) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: cache, submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func line(
        _ coordinator: SyncCoordinator, _ taskID: String, _ role: Components.Schemas.TaskLineRole = .implementer
    ) -> Components.Schemas.TaskLine? {
        coordinator.tasks.first { $0.task.id == taskID }?.task.task_lines.first { $0.role == role }
    }

    /// The task timeline's `task_line_set` events, newest version first: the
    /// mock records every event at one instant, so time can't order them.
    private func lineEvents(
        _ coordinator: SyncCoordinator, _ taskID: String
    ) async -> [Components.Schemas.TaskEventTaskLine] {
        await coordinator.refreshTaskTimeline(for: taskID)
        return (coordinator.taskTimelinesByTaskID[taskID]?.events ?? [])
            .filter { $0.kind == .task_line_set }.compactMap { $0.line?.value1 }.sorted { $0.version > $1.version }
    }

    @Test func aChangeSetsTheLineAndAClearReturnsTheRoleToTheLineup() async throws {
        let (server, taskID) = try await server()
        let coordinator = await coordinator(server)
        let model = TaskLineChangeModel(coordinator: coordinator)
        #expect(model.canChange)

        await model.change(taskID: taskID, role: .implementer, to: Self.codex)

        #expect(model.failure == nil)
        #expect(coordinator.pendingTaskLineChange == nil)
        let set = try #require(line(coordinator, taskID))
        #expect(set.agent == nil && set.selection?.value1 == Self.codex)
        #expect(set.source == .client_line_change)
        let afterSet = await lineEvents(coordinator, taskID)
        #expect(afterSet.first?.role == .implementer && afterSet.first?.selection?.value1 == Self.codex)

        await model.change(taskID: taskID, role: .implementer, to: nil)

        #expect(model.failure == nil)
        #expect(coordinator.pendingTaskLineChange == nil)
        #expect(line(coordinator, taskID) == nil)
        let afterClear = await lineEvents(coordinator, taskID)
        #expect(afterClear.count == afterSet.count + 1)
        #expect(afterClear.first?.role == .implementer && afterClear.first?.selection == nil)
        #expect(afterClear.first?.superseded_selection?.value1 == Self.codex)
    }

    @Test func aRunningRoleRefusesTheChangeNamesItsLineAndRefreshes() async throws {
        let (server, taskID) = try await server()
        let coordinator = await coordinator(server)
        let model = TaskLineChangeModel(coordinator: coordinator)
        await model.change(taskID: taskID, role: .reviewer, to: Self.codex)
        await server.setTaskLineRoleRunning(taskID: taskID, role: .reviewer, running: true)
        let sent = CommandCounter()
        let refreshes = CommandCounter()
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { await sent.increment() }
            if operation == "getSyncRevision" || operation == "getSyncBootstrap" { await refreshes.increment() }
        }

        await model.change(taskID: taskID, role: .reviewer, to: nil)

        #expect(
            model.failure
                == .init(
                    taskID: taskID,
                    message: "The reviewer is running, so its agent didn't change. "
                        + "It stays on \(AgentChoiceDisplay.selectionLabel(Self.codex))."))
        #expect(coordinator.pendingTaskLineChange == nil)
        #expect(await sent.count == 1)
        #expect(await refreshes.count >= 1)
        #expect(line(coordinator, taskID, .reviewer)?.selection?.value1 == Self.codex)
        // Nothing resends: a retry has no saved command to send.
        await model.retry()
        #expect(await sent.count == 1)
        #expect(model.canChange)
    }

    @Test func aRoleOnTheLineupIsNamedAsTheLineupWhenRefused() {
        let task = TaskFixtures.defaultTasks()[0].task
        #expect(
            TaskLineChangeModel.runningMessage(role: .specifier, task: task)
                == "The specifier is running, so its agent didn't change. It stays on the lineup.")
    }

    @Test func aRefusedSelectionRecordsNothingAndSaysSo() async throws {
        let (server, taskID) = try await server()
        let coordinator = await coordinator(server)
        let model = TaskLineChangeModel(coordinator: coordinator)
        var malformed = Self.codex
        malformed.model_id = "gpt codex"

        await model.change(taskID: taskID, role: .implementer, to: malformed)

        #expect(
            model.failure
                == .init(taskID: taskID, message: "The daemon refused the change (status 400). Nothing was changed."))
        #expect(coordinator.pendingTaskLineChange == nil)
        #expect(line(coordinator, taskID) == nil)
    }

    @Test func aLostAnswerSurvivesRelaunchAndOnlyRetryResendsTheSameCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let (server, taskID) = try await server()
        let first = await coordinator(server, cache: cache)
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        await TaskLineChangeModel(coordinator: first).change(taskID: taskID, role: .implementer, to: Self.codex)
        let saved = try #require(first.pendingTaskLineChange)
        #expect(saved.receipt == nil)
        #expect(!TaskLineChangeModel(coordinator: first).canChange)
        let applied = try await first.store.client.getSyncRevision().ok.body.json.revision

        let restored = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "mock")
        #expect(restored.pendingTaskLineChange == saved)
        await restored.refresh()
        // A relaunch never resends on its own.
        #expect(restored.pendingTaskLineChange == saved)
        let resent = CommandCounter()
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { await resent.increment() }
        }
        await TaskLineChangeModel(coordinator: restored).retry()
        // The replay returns the recorded receipt and records nothing twice.
        #expect(await resent.count == 1)
        #expect(restored.pendingTaskLineChange == nil)
        #expect(try await restored.store.client.getSyncRevision().ok.body.json.revision == applied)
        #expect(line(restored, taskID)?.selection?.value1 == Self.codex)
    }

    @Test func aStaleTaskPageCantChangeALine() async throws {
        let (server, taskID) = try await server()
        let coordinator = await coordinator(server)
        let model = TaskLineChangeModel(coordinator: coordinator)
        coordinator.store.freshness = .unreachable

        #expect(model.unavailableReason == "Offline. Connect to the daemon before changing the agent.")
        #expect(!model.canChange)
        await model.change(taskID: taskID, role: .implementer, to: Self.codex)
        #expect(coordinator.pendingTaskLineChange == nil)
        #expect(line(coordinator, taskID) == nil)
    }
}

/// Counts the mock operations a test watches.
private actor CommandCounter {
    private(set) var count = 0
    func increment() { count += 1 }
}
