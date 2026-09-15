import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The client-layer task-submission call (plan §5.11): it builds a submit_task
/// command, accepts the result only through the trust gate, and refreshes for
/// read-your-write.
@Suite @MainActor struct TaskSubmissionModelTests {
    private func coordinator(server: MockServer = MockServer()) -> SyncCoordinator {
        SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
    }

    @Test func submitCreatesTaskVisibleAfterRefresh() async throws {
        let coordinator = coordinator()
        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = await model.submit(
            projectID: "project-1", source: "# Add a health endpoint\n\nExpose /health.",
            name: "Health endpoint")
        guard let taskID else {
            Issue.record("submit returned no task id; state \(model.state)")
            return
        }
        #expect(model.state == .submitted(taskID: taskID))
        // Read-your-write: the created task's run is visible on the next read.
        let runs = try await coordinator.store.client.listRuns().ok.body.json
        #expect(runs.contains { $0.run.task_id == taskID })
    }

    @Test func sameSourceConvergesOnOneTask() async {
        let coordinator = coordinator()
        let model = TaskSubmissionModel(coordinator: coordinator)
        let first = await model.submit(projectID: "project-1", source: "Repeated source.")
        let second = await model.submit(projectID: "project-1", source: "Repeated source.")
        #expect(first != nil)
        #expect(first == second)
    }

    @Test func differentProjectsCreateDistinctTasks() async {
        let coordinator = coordinator()
        let model = TaskSubmissionModel(coordinator: coordinator)
        let a = await model.submit(projectID: "project-1", source: "Shared source.")
        let b = await model.submit(projectID: "project-2", source: "Shared source.")
        #expect(a != nil && b != nil)
        #expect(a != b)
    }

    @Test func trustGateRejectsATamperedResult() async {
        let server = MockServer()
        // Mangle the returned source digest so it no longer matches the source
        // the model submitted; the trust gate must refuse the result.
        await server.setCommandResultTransform { result in
            guard case .submit_task(var record) = result.record else { return result }
            record.source_digest = .init(value1: "sha256:" + String(repeating: "0", count: 64))
            return .init(record: .submit_task(record), revision: result.revision)
        }
        let model = TaskSubmissionModel(coordinator: coordinator(server: server))
        let taskID = await model.submit(projectID: "project-1", source: "Some work.")
        #expect(taskID == nil)
        guard case .rejected(let reason) = model.state else {
            Issue.record("expected a rejected state, got \(model.state)")
            return
        }
        #expect(reason.contains("invalid"))
    }

    @Test func lostResponseRetriesWithTheSameCommandID() async throws {
        let server = MockServer()
        let coordinator = coordinator(server: server)
        let model = TaskSubmissionModel(coordinator: coordinator)
        // The response is dropped after the server materializes the task, so
        // the command committed but the client saw no answer: a lost response.
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { try await lost.consume() }
        }
        let first = await model.submit(projectID: "project-1", source: "Add a health endpoint.")
        #expect(first == nil)
        #expect(model.state == .lost)

        let taskID = await model.retry()
        guard let taskID else {
            Issue.record("retry returned no task id; state \(model.state)")
            return
        }
        #expect(model.state == .submitted(taskID: taskID))
        // The retry reused the command id, so the daemon converged on the one
        // task the lost submission already committed: exactly one run for it.
        let runs = try await coordinator.store.client.listRuns().ok.body.json
        #expect(runs.filter { $0.run.task_id == taskID }.count == 1)
    }

    @Test func revokedDeviceIsRejectedAndSetsUnauthenticated() async throws {
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await APIClientFactory.mock(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let client = APIClientFactory.mock(server: server) { grant.device_token }
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: active.id), cache: InMemoryCacheStore())
        _ = try await coordinator.store.client.revokeDevice(path: .init(device_id: active.id)).ok

        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = await model.submit(projectID: "project-1", source: "Work after revocation.")
        #expect(taskID == nil)
        guard case .rejected(let reason) = model.state else {
            Issue.record("expected a rejected state, got \(model.state)")
            return
        }
        #expect(reason.contains("device"))
        // A rejected credential surfaces as device state, the way a decision
        // handles a 401, so the freshness banner leads the operator to re-pair.
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func canComposeOnlyWhenFresh() {
        #expect(TaskSubmissionModel.canCompose(freshness: .fresh))
        let nonFresh: [InboxStore.Freshness] = [
            .unvalidated,
            .unreachable,
            .syncFailing,
            .contractMismatch(daemonContract: "sha256:" + String(repeating: "0", count: 64)),
            .unauthenticated,
        ]
        for freshness in nonFresh {
            #expect(!TaskSubmissionModel.canCompose(freshness: freshness))
        }
    }
}
