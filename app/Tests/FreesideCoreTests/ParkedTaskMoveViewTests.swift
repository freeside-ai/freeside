import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #2958: a saved `move_parked_task` keeps its Retry on its task's page after
/// the hold that offered the move clears, its refusal shows on that task
/// alone, and a move waiting on one task names that task where it blocks
/// another task's move.
/// Counts the commands a mock daemon receives.
private actor CommandCount {
    private(set) var value = 0
    func increment() { value += 1 }
}

@Suite @MainActor struct ParkedTaskMoveViewTests {
    private static let teamChoice = Components.Schemas.AlternateAgentChoice(
        enrollment_id: UsageFixtures.readingCollectorID, model_id: nil, effort: "low")
    private static let refusal = "The daemon refused the move (status 400). Nothing was changed."

    /// A mock daemon holding a task parked on a quota window whose hold
    /// offers the reading collector's enrollment, beside `others`.
    private func parkedServer(
        others: [Components.Schemas.TaskSnapshot] = []
    ) throws -> (server: MockServer, taskID: String) {
        let parked = HoldPresentationFixtures.make("Quota window")
        let history = try #require(parked.history)
        let server = MockServer(
            runs: parked.runs,
            tasks: [.init(as_of_revision: 1, entity_version: 1, task: parked.task)] + others,
            taskTimelines: [history], agentCatalog: UsageFixtures.defaultAgentCatalog())
        return (server, parked.task.id)
    }

    private func coordinator(_ server: MockServer) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: InMemoryCacheStore(), submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    /// The task's position in its loaded timeline.
    private func position(_ coordinator: SyncCoordinator, _ taskID: String) async throws -> TaskDisplay.Position? {
        await coordinator.refreshTaskTimeline(for: taskID)
        let task = try #require(coordinator.tasks.first { $0.task.id == taskID }).task
        let history = try #require(coordinator.taskTimelinesByTaskID[taskID])
        return TaskDisplay.position(task, runs: coordinator.runs, history: history)
    }

    /// What the task page passes the view: the offers of the task's position.
    private func offers(
        _ coordinator: SyncCoordinator, _ taskID: String
    ) async throws -> [Components.Schemas.AlternateAgentOffer] {
        TaskDisplay.moveOffers(try await position(coordinator, taskID))
    }

    private func revision(_ coordinator: SyncCoordinator) async throws -> Int64 {
        try await coordinator.store.client.getSyncRevision().ok.body.json.revision
    }

    /// Loses the answer to the next command sent.
    private func loseNextAnswer(_ server: MockServer) async {
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
    }

    /// #1266: a daemon whose `/health` doesn't list `parked_task_move`, or
    /// lists no features at all as one built before the list, refuses the
    /// move, so the page offers none and a pick sends nothing.
    @Test func aDaemonThatDoesNotServeTheMoveOffersNone() async throws {
        for features: [Components.Schemas.DaemonFeature]? in [[], nil] {
            let (server, taskID) = try parkedServer()
            await server.setHealthFeatures(features)
            let sent = CommandCount()
            await server.setBeforeRespond { operation in
                if operation == "submitCommand" { await sent.increment() }
            }
            let coordinator = await coordinator(server)
            #expect(coordinator.store.freshness == .fresh)
            let offers = try await offers(coordinator, taskID)
            #expect(!offers.isEmpty)
            let view = ParkedTaskMoveView(coordinator: coordinator, taskID: taskID, offers: offers)
            #expect(!view.offersMove && view.isQuiet)

            let model = coordinator.parkedTaskMove
            #expect(!model.isServed && !model.canMove)
            await model.move(taskID: taskID, to: Self.teamChoice)
            #expect(await sent.value == 0)
            #expect(coordinator.pendingParkedTaskMove == nil && model.failure == nil)
        }
    }

    /// A daemon restarted on a build that predates the move, after the round
    /// that listed it: the refusal tells the operator to update the daemon,
    /// and the command is dropped rather than kept for Retry.
    @Test func aMoveTheDaemonNoLongerServesIsDroppedWithTheUpdatePrompt() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let model = coordinator.parkedTaskMove
        #expect(model.canMove)
        let before = try await revision(coordinator)
        await server.setHealthFeatures([])

        await model.move(taskID: taskID, to: Self.teamChoice)

        #expect(model.failure == .init(taskID: taskID, message: CommandRefusal.unservedKind))
        #expect(coordinator.pendingParkedTaskMove == nil)
        #expect(try await revision(coordinator) == before)
        // The round the refusal ran read the list again.
        #expect(!model.isServed)
    }

    /// The daemon records the move and clears the hold, but the answer is
    /// lost: the page still draws the move with its Retry, and Retry
    /// replays the recorded receipt instead of moving twice.
    @Test func aRecordedMoveWhoseAnswerIsLostKeepsRetryAfterTheHoldClears() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        #expect(try await !offers(coordinator, taskID).isEmpty)
        await loseNextAnswer(server)

        await coordinator.parkedTaskMove.move(taskID: taskID, to: Self.teamChoice)
        let saved = try #require(coordinator.pendingParkedTaskMove)
        #expect(saved.receipt == nil)
        await coordinator.refresh()

        let position = try await position(coordinator, taskID)
        #expect(TaskDisplay.moveOffers(position).isEmpty)
        let snapshot = try #require(coordinator.tasks.first { $0.task.id == taskID })
        let page = TaskPageView(coordinator: coordinator, snapshot: snapshot, onOpenRun: { _ in })
        let view = try #require(page.parkedTaskMove(at: position))
        #expect(view.pending == saved)
        #expect(coordinator.parkedTaskMove.canRetry)
        // Unpaired, Retry can't send, and the view says why.
        let freshness = coordinator.store.freshness
        coordinator.store.freshness = .unauthenticated
        #expect(!coordinator.parkedTaskMove.canRetry)
        #expect(coordinator.parkedTaskMove.unavailableReason == "Pair this device again before moving the task.")
        coordinator.store.freshness = freshness
        let applied = try await revision(coordinator)

        await coordinator.parkedTaskMove.retry()

        #expect(coordinator.pendingParkedTaskMove == nil)
        #expect(coordinator.parkedTaskMove.failure == nil)
        #expect(try await revision(coordinator) == applied)
        #expect(page.parkedTaskMove(at: position) == nil)
    }

    /// The daemon never recorded the move and the task is no longer parked:
    /// Retry is refused, the saved move ends, and only that task's view
    /// says so.
    @Test func aMoveOfATaskNoLongerParkedIsRefusedOnThatTaskAlone() async throws {
        var other = TaskProgressFixtures.make("No position").task
        other.id = "task-not-parked"
        other.task_lines = other.task_lines.map { line in
            var line = line
            line.task_id = other.id
            return line
        }
        let (server, parkedID) = try parkedServer(others: [.init(as_of_revision: 1, entity_version: 1, task: other)])
        let coordinator = await coordinator(server)
        let epoch = try #require(coordinator.cursors?.syncEpoch)
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: "device-mock",
            payload: .move_parked_task(.init(kind: .move_parked_task, task_id: other.id, choice: Self.teamChoice)))
        #expect(coordinator.retainParkedTaskMove(.init(command: command, syncEpoch: epoch)))
        let view = ParkedTaskMoveView(coordinator: coordinator, taskID: other.id, offers: [])
        let parkedView = ParkedTaskMoveView(
            coordinator: coordinator, taskID: parkedID, offers: try await offers(coordinator, parkedID))
        #expect(!view.isQuiet && view.pending != nil)
        #expect(parkedView.pending == nil)

        await coordinator.parkedTaskMove.retry()

        #expect(coordinator.pendingParkedTaskMove == nil)
        #expect(coordinator.parkedTaskMove.failure == .init(taskID: other.id, message: Self.refusal))
        #expect(view.failure == Self.refusal)
        #expect(!view.isQuiet)
        #expect(parkedView.failure == nil)
    }

    /// A device sends one move at a time. While one waits unanswered,
    /// another task's disabled menu names the task holding it, whose page
    /// alone offers its Retry; a freshness reason takes precedence.
    @Test func aMoveWaitingOnAnotherTaskIsNamedBesideTheDisabledMenu() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let offers = try await offers(coordinator, taskID)
        let name = try #require(coordinator.tasks.first { $0.task.id == taskID }).task.display_names.task.text
        // The reason depends only on the saved move and freshness, so the
        // other parked task reuses this hold's offers.
        let other = ParkedTaskMoveView(coordinator: coordinator, taskID: "task-other", offers: offers)
        #expect(other.blockedReason == nil)
        await loseNextAnswer(server)

        await coordinator.parkedTaskMove.move(taskID: taskID, to: Self.teamChoice)
        #expect(coordinator.pendingParkedTaskMove?.receipt == nil)

        #expect(
            ParkedTaskMoveView(coordinator: coordinator, taskID: taskID, offers: offers).blockedReason
                == "A move is still waiting for the daemon.")
        #expect(
            other.blockedReason
                == "A move of \u{201C}\(name)\u{201D} is still waiting for the daemon. "
                + "Retry it from that task's page.")
        #expect(other.pending == nil && !other.isQuiet)

        coordinator.store.freshness = .syncFailing
        #expect(other.blockedReason == "The task isn't current. Refresh before moving it.")
    }

    /// Once the daemon answered, the waiting task's page offers no Retry, so
    /// another task's menu says the move is being recorded.
    @Test func aMoveTheDaemonAnsweredIsNamedAsBeingRecorded() async throws {
        let (server, taskID) = try parkedServer()
        let coordinator = await coordinator(server)
        let offers = try await offers(coordinator, taskID)
        let name = try #require(coordinator.tasks.first { $0.task.id == taskID }).task.display_names.task.text
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: "device-mock",
            payload: .move_parked_task(.init(kind: .move_parked_task, task_id: taskID, choice: Self.teamChoice)))
        let receipt = try await coordinator.store.client.submitCommand(body: .json(command)).ok.body.json
        let epoch = try #require(coordinator.cursors?.syncEpoch)
        #expect(coordinator.retainParkedTaskMove(.init(command: command, syncEpoch: epoch, receipt: receipt)))

        #expect(!coordinator.parkedTaskMove.canRetry)
        #expect(
            ParkedTaskMoveView(coordinator: coordinator, taskID: "task-other", offers: offers).blockedReason
                == "A move of \u{201C}\(name)\u{201D} is being recorded.")
    }
}
