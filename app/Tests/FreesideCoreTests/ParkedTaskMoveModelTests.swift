import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// #869: a task parked on its implementer's exhausted quota window moves to
/// an agent its hold offers through a saved `move_parked_task` command, which
/// settles once sync shows the revision the daemon recorded it at.
@Suite @MainActor struct ParkedTaskMoveModelTests {
    /// The selection a move to the hold's offered enrollment at effort low
    /// records: the lineup implementer's model with the chosen effort.
    private static let teamSelection = UsageFixtures.selection(
        UsageFixtures.readingCollectorID, "claude-opus", "low")
    private static let teamChoice = Components.Schemas.AlternateAgentChoice(
        enrollment_id: UsageFixtures.readingCollectorID, model_id: nil, effort: "low")

    /// A mock daemon holding the parked task, its run, and the timeline whose
    /// hold offers the reading collector's enrollment.
    private func parkedServer() throws -> (server: MockServer, taskID: String) {
        let parked = HoldPresentationFixtures.make("Quota window")
        let history = try #require(parked.history)
        let server = MockServer(
            runs: parked.runs,
            tasks: [.init(as_of_revision: 1, entity_version: 1, task: parked.task)],
            taskTimelines: [history], agentCatalog: UsageFixtures.defaultAgentCatalog())
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

    @Test func aMovePinsTheImplementerToTheChosenSelectionAndSettles() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        #expect(model.canMove)

        await model.move(taskID: taskID, to: Self.teamChoice)

        #expect(model.failure == nil)
        #expect(coordinator.pendingParkedTaskMove == nil)
        let line = try #require(implementerLine(coordinator, taskID))
        #expect(line.agent == nil && line.selection?.value1 == Self.teamSelection)
        #expect(line.source == .alternate_agent_move)
        #expect(line.provenance?.value1.pool == "claude-team-pool")
    }

    @Test func anEnrollmentTheHoldDoesNotOfferIsRefusedAndNothingChanges() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        let before = implementerLine(coordinator, taskID)

        await model.move(taskID: taskID, to: .init(enrollment_id: UsageFixtures.unsupportedCollectorID))

        #expect(model.failure?.message == "The daemon refused the move (status 400). Nothing was changed.")
        #expect(model.failure?.taskID == taskID)
        #expect(coordinator.pendingParkedTaskMove == nil)
        #expect(implementerLine(coordinator, taskID) == before)
    }

    /// The daemon holds a choice to the enrollment's discovered catalog, so
    /// an effort the offered enrollment's model doesn't list can't move the
    /// task.
    @Test func anEffortTheCatalogDoesNotServeIsRefused() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        let before = implementerLine(coordinator, taskID)

        await model.move(
            taskID: taskID,
            to: .init(enrollment_id: UsageFixtures.readingCollectorID, model_id: "claude-sonnet", effort: "max"))

        #expect(model.failure?.message != nil)
        #expect(implementerLine(coordinator, taskID) == before)
    }

    /// The client trusts a move receipt only when its record holds the
    /// choice sent, its retry the selection that choice picks, and its
    /// implementer line that selection.
    @Test func aReceiptThatDoesNotCarryTheChoiceThroughIsRefused() async throws {
        let (server, taskID) = try parkedServer()
        let client = APIClientFactory.mock(server: server)
        let command = Components.Schemas.ClientCommand(
            command_id: "cmd-move", device_id: "device-mock",
            payload: .move_parked_task(.init(kind: .move_parked_task, task_id: taskID, choice: Self.teamChoice)))
        let result = try await client.submitCommand(body: .json(command)).ok.body.json
        #expect(CommandResultTrust.accepts(result, for: command))
        guard case .move_parked_task(let record) = result.record,
            let index = record.lines.firstIndex(where: { $0.role == .implementer })
        else {
            Issue.record("expected a move record with an implementer line")
            return
        }
        let high = UsageFixtures.selection(UsageFixtures.readingCollectorID, "claude-opus", "high")
        var otherChoice = record
        otherChoice.choice.effort = "high"
        var lineRunsAnother = record
        lineRunsAnother.lines[index].selection = .init(value1: high)
        // The retry and its line agree, on an effort the choice didn't pick.
        var retryUnpicked = lineRunsAnother
        retryUnpicked.retry.selection = high
        for forged in [otherChoice, lineRunsAnother, retryUnpicked] {
            let result = Components.Schemas.CommandResult(record: .move_parked_task(forged), revision: result.revision)
            #expect(!CommandResultTrust.accepts(result, for: command))
        }
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
        await ParkedTaskMoveModel(coordinator: first).move(taskID: taskID, to: Self.teamChoice)
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
        #expect(implementerLine(restored, taskID)?.selection?.value1 == Self.teamSelection)
    }
}
