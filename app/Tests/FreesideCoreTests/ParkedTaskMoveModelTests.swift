import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// #869: a task parked on its implementer's exhausted quota window moves to
/// an agent its hold offers through a saved `move_parked_task` command, which
/// settles once sync shows the revision the daemon recorded it at.
@Suite @MainActor struct ParkedTaskMoveModelTests {
    /// A mock daemon holding the parked task, its run, and the timeline whose
    /// hold offers claude-team.
    private func parkedServer() throws -> (server: MockServer, taskID: String) {
        let parked = HoldPresentationFixtures.make("Quota window")
        let history = try #require(parked.history)
        let server = MockServer(
            runs: parked.runs,
            tasks: [.init(as_of_revision: 1, entity_version: 1, task: parked.task)],
            taskTimelines: [history])
        return (server, parked.task.id)
    }

    private func coordinator(
        _ server: MockServer, cache: any CacheStore = InMemoryCacheStore()
    ) async
        -> SyncCoordinator
    {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: cache, submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func implementerLine(_ coordinator: SyncCoordinator, _ taskID: String) -> Components.Schemas.TaskLine? {
        coordinator.tasks.first { $0.task.id == taskID }?.task.task_lines.first { $0.role == .implementer }
    }

    @Test func aMovePinsTheImplementerToTheChosenAgentAndSettles() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        #expect(model.canMove)

        await model.move(taskID: taskID, to: "claude-team")

        #expect(model.failure == nil)
        #expect(coordinator.pendingParkedTaskMove == nil)
        let line = try #require(implementerLine(coordinator, taskID))
        #expect(line.agent == "claude-team" && line.source == .alternate_agent_move)
        #expect(line.provenance?.value1.pool == "claude-team-pool")
    }

    @Test func anAgentTheHoldDoesNotOfferIsRefusedAndNothingChanges() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        let before = implementerLine(coordinator, taskID)

        await model.move(taskID: taskID, to: "claude-other")

        #expect(model.failure == "The daemon refused the move (status 400). Nothing was changed.")
        #expect(coordinator.pendingParkedTaskMove == nil)
        #expect(implementerLine(coordinator, taskID) == before)
    }

    @Test func aLostAnswerSurvivesRelaunchAndRetryResendsTheSameCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let (server, taskID) = try parkedServer()
        let first = await coordinator(server, cache: cache)
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        await ParkedTaskMoveModel(coordinator: first).move(taskID: taskID, to: "claude-team")
        let saved = try #require(first.pendingParkedTaskMove)
        #expect(saved.receipt == nil)
        #expect(!ParkedTaskMoveModel(coordinator: first).canMove)
        let applied = try await first.store.client.getSyncRevision().ok.body.json.revision

        let restored = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "mock")
        #expect(restored.pendingParkedTaskMove == saved)
        await restored.refresh()
        // A relaunch never resends on its own.
        #expect(restored.pendingParkedTaskMove == saved)
        await ParkedTaskMoveModel(coordinator: restored).retry()
        // The replay returns the recorded receipt and moves nothing twice.
        #expect(restored.pendingParkedTaskMove == nil)
        #expect(try await restored.store.client.getSyncRevision().ok.body.json.revision == applied)
        #expect(implementerLine(restored, taskID)?.agent == "claude-team")
    }
}
