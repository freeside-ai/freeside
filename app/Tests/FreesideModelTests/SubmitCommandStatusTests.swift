import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// #2968: `POST /commands` declares 403, 404, 413, and 500, which the
/// generated client used to surface as `.undocumented`. Each command model
/// classifies a declared status exactly as it classified the undocumented
/// one. These run on Linux and cover the models whose own suites forced
/// none of these statuses; the policy and stop suites force 413 with
/// their other kept statuses.
@Suite @MainActor struct SubmitCommandStatusTests {
    private func coordinator(_ server: MockServer) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: InMemoryCacheStore(), submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func forceSubmit(_ server: MockServer, status: Int) async {
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
    }

    private func expectAuthentication(_ coordinator: SyncCoordinator, unauthenticated: Bool) {
        if unauthenticated {
            #expect(coordinator.store.freshness == .unauthenticated)
        } else {
            #expect(coordinator.store.freshness != .unauthenticated)
        }
    }

    /// A line change: 400 and 404 refuse it and drop it; 403 is the
    /// device's credential and keeps it; 413 and 500 leave it unresolved.
    @Test(arguments: [400, 403, 404, 413, 500])
    func aTaskLineChangeKeepsItsClassification(status: Int) async throws {
        let server = MockServer()
        let task = try #require(
            try await APIClientFactory.mock(server: server).getSyncBootstrap().ok.body.json.tasks.first)
        let coordinator = await coordinator(server)
        let model = TaskLineChangeModel(coordinator: coordinator)
        await forceSubmit(server, status: status)

        await model.change(taskID: task.task.id, role: .implementer, to: UsageFixtures.agentSelection("codex-primary"))

        switch status {
        case 400, 404:
            #expect(coordinator.pendingTaskLineChange == nil)
            #expect(model.failure?.message.contains("status \(status)") == true)
        default:
            let pending = try #require(coordinator.pendingTaskLineChange)
            #expect(pending.receipt == nil)
            #expect(model.failure == nil)
            expectAuthentication(coordinator, unauthenticated: status == 403)
        }
    }

    /// A parked-task move: the same classification as a line change.
    @Test(arguments: [400, 403, 404, 413, 500])
    func aParkedTaskMoveKeepsItsClassification(status: Int) async throws {
        // The forced status answers before the daemon would check the park,
        // so any task stands in for a parked one.
        let server = MockServer()
        let task = try #require(
            try await APIClientFactory.mock(server: server).getSyncBootstrap().ok.body.json.tasks.first)
        let coordinator = await coordinator(server)
        let model = ParkedTaskMoveModel(coordinator: coordinator)
        #expect(model.canMove)
        await forceSubmit(server, status: status)

        await model.move(
            taskID: task.task.id,
            to: Components.Schemas.AlternateAgentChoice(
                enrollment_id: UsageFixtures.readingCollectorID, model_id: nil, effort: "low"))

        switch status {
        case 400, 404:
            #expect(coordinator.pendingParkedTaskMove == nil)
            #expect(model.failure?.message.contains("status \(status)") == true)
        default:
            let pending = try #require(coordinator.pendingParkedTaskMove)
            #expect(pending.receipt == nil)
            #expect(model.failure == nil)
            expectAuthentication(coordinator, unauthenticated: status == 403)
        }
    }

    /// A usage refresh: the same classification as a line change.
    @Test(arguments: [400, 403, 404, 413, 500])
    func aUsageRefreshKeepsItsClassification(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await forceSubmit(server, status: status)

        await model.refresh(pool: UsageFixtures.readingPoolID)

        switch status {
        case 400, 404:
            #expect(coordinator.pendingUsageCommand == nil)
            #expect(model.failure?.contains("status \(status)") == true)
        default:
            let pending = try #require(coordinator.pendingUsageCommand)
            #expect(pending.receipt == nil)
            #expect(model.failure == nil)
            expectAuthentication(coordinator, unauthenticated: status == 403)
        }
    }

    /// A task submission: every 4xx but 401 and 403 rejects it; 403 is the
    /// device's credential and 500 is ambiguous, so both keep it as lost.
    @Test(arguments: [400, 403, 404, 413, 500])
    func aTaskSubmissionKeepsItsClassification(status: Int) async throws {
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        let model = TaskSubmissionModel(coordinator: coordinator)
        await forceSubmit(server, status: status)

        let taskID = await model.submit(projectID: "project-1", source: "Add a health endpoint.")

        #expect(taskID == nil)
        switch status {
        case 403, 500:
            #expect(model.state == .lost)
            #expect(model.pendingSubmissions.count == 1)
            expectAuthentication(coordinator, unauthenticated: status == 403)
        default:
            guard case .rejected(let message) = model.state else {
                Issue.record("status \(status) left \(model.state), want rejected")
                return
            }
            #expect(message.contains("status \(status)"))
            #expect(model.pendingSubmissions.isEmpty)
        }
    }

    /// A decision: every 4xx but 401, 403 included, is a definite refusal
    /// that releases the ledger slot; a 500 is ambiguous and keeps it.
    @Test(arguments: [400, 403, 404, 413, 500])
    func aDecisionKeepsItsClassification(status: Int) async throws {
        let server = MockServer()
        let store = InboxStore(client: APIClientFactory.mock(server: server))
        await store.refresh()
        let model = DecisionModel(store: store, itemID: "item-spec_approval", openURL: { _ in true })
        await model.validate()
        await forceSubmit(server, status: status)

        await model.submit(.approve)

        if status == 500 {
            #expect(model.pendingCommand != nil)
        } else {
            #expect(model.pendingCommand == nil)
            #expect(model.submissionError?.contains("status \(status)") == true)
        }
        #expect(model.phase != .applied)
    }
}
