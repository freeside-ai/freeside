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

    @Test func submitSeesTheTaskEvenWhenARefreshWasAlreadyInFlight() async throws {
        let server = MockServer()
        let coordinator = coordinator(server: server)
        let model = TaskSubmissionModel(coordinator: coordinator)
        let reached = AsyncGate()
        let release = AsyncGate()
        let committed = AsyncGate()
        // Submit runs its validation round first, so the in-flight refresh
        // must start after that round and before the commit: the send's
        // before-respond hook starts it and holds it on its run-list read,
        // its last daemon read, so all of its reads precede the commit and it
        // cannot close the gap itself; only a round begun after the commit
        // surfaces the task. Signal the commit from an after-respond hook:
        // the transport runs before-respond ahead of routing
        // (MockServerTransport), so a before-respond signal would open
        // `committed` before submitCommand has actually committed.
        await server.setBeforeRespond { operationID in
            guard operationID == "submitCommand" else { return }
            let firstRunList = ScriptedResponses([.hold(reached: reached, release: release)])
            await server.setAfterRespond { operationID in
                switch operationID {
                case "submitCommand": await committed.open()
                case "listRuns": try await firstRunList.next()
                default: break
                }
            }
            Task { @MainActor in await coordinator.refresh() }
            await reached.wait()
        }

        // `submit` awaits `refreshAfterCommit` after the send, which must run
        // a round that observes the committed task rather than coalescing
        // onto the pre-commit round.
        let submission = Task {
            await model.submit(projectID: "project-1", source: "# Health check")
        }
        await committed.wait()
        // Let submit's refreshAfterCommit reach its wait on the pre-commit round
        // before releasing it, so a regression that coalesced onto that round
        // would leave the task unseen, as the SyncCoordinator coalescing test does.
        await Task.yield()
        await release.open()
        let taskID = await submission.value

        let id = try #require(taskID)
        #expect(model.state == .submitted(taskID: id))
        #expect(coordinator.tasks.contains { $0.task.id == id })
    }

    @Test func submitValidatesAnUnvalidatedCacheBeforeSending() async throws {
        let server = MockServer()
        let coordinator = coordinator(server: server)
        await coordinator.bootstrap()
        // A partial read past the last full snapshot leaves the cache
        // `.unvalidated`, the steady state against a busy daemon (#1554).
        await server.advanceRun(id: RunFixtures.activeRunID)
        await coordinator.refreshRuns()
        #expect(coordinator.store.freshness == .unvalidated)
        #expect(TaskSubmissionModel.canCompose(freshness: coordinator.store.freshness))

        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }
        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = await model.submit(projectID: "project-1", source: "# Health check")

        let id = try #require(taskID)
        #expect(model.state == .submitted(taskID: id))
        // The validation round's first read precedes the send.
        let issued = await operations.ids
        let send = try #require(issued.firstIndex(of: "submitCommand"))
        #expect(issued[..<send].contains("getSyncRevision"))
    }

    @Test func cancellingDuringValidationSendsNothing() async {
        let server = MockServer()
        let coordinator = coordinator(server: server)
        await coordinator.bootstrap()
        let reached = AsyncGate()
        let release = AsyncGate()
        let validationRead = ScriptedResponses([.hold(reached: reached, release: release)])
        let operations = OperationLog()
        await server.setBeforeRespond { operationID in
            await operations.append(operationID)
            if operationID == "getSyncRevision" { try await validationRead.next() }
        }

        let model = TaskSubmissionModel(coordinator: coordinator)
        let submission = Task {
            await model.submit(projectID: "project-1", source: "# Health check")
        }
        await reached.wait()
        // The operator dismisses the sheet while the validation round runs.
        submission.cancel()
        await release.open()
        let taskID = await submission.value

        #expect(taskID == nil)
        #expect(model.state == .idle)
        #expect(model.pendingSubmissions.isEmpty)
        #expect(!(await operations.ids).contains("submitCommand"))
    }

    @Test func unreachableDaemonSendsNothing() async {
        let server = MockServer()
        let coordinator = coordinator(server: server)
        await coordinator.bootstrap()
        let operations = OperationLog()
        await server.setBeforeRespond { operationID in
            await operations.append(operationID)
            throw InjectedFailure()
        }

        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = await model.submit(projectID: "project-1", source: "# Health check")

        #expect(taskID == nil)
        guard case .rejected = model.state else {
            Issue.record("expected a rejection; state \(model.state)")
            return
        }
        #expect(model.pendingSubmissions.isEmpty)
        #expect(!(await operations.ids).contains("submitCommand"))
    }

    @Test func sameSourceCreatesDistinctTasks() async {
        let coordinator = coordinator()
        let model = TaskSubmissionModel(coordinator: coordinator)
        let first = await model.submit(projectID: "project-1", source: "Repeated source.")
        let second = await model.submit(projectID: "project-1", source: "Repeated source.")
        #expect(first != nil)
        #expect(first != second)
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
        #expect(model.state == .lost)
        #expect(model.pendingSubmissions.count == 1)
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

    @Test func revokedDevicePreservesSubmissionAndSetsUnauthenticated() async throws {
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
        // Revoke between the validation round and the send, the one window
        // in which a submission can still meet a rejected credential.
        await server.setBeforeRespond { operationID in
            guard operationID == "submitCommand" else { return }
            _ = try await client.revokeDevice(path: .init(device_id: active.id)).ok
        }

        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = await model.submit(projectID: "project-1", source: "Work after revocation.")
        #expect(taskID == nil)
        #expect(model.state == .lost)
        #expect(model.pendingSubmissions.count == 1)
        // A rejected credential surfaces as device state, the way a decision
        // handles a 401, so the freshness banner leads the operator to re-pair.
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func canComposeUnlessSyncIsFailing() {
        // `.unvalidated` stays open: submit validates with its own round.
        #expect(TaskSubmissionModel.canCompose(freshness: .fresh))
        #expect(TaskSubmissionModel.canCompose(freshness: .unvalidated))
        for freshness in Self.failingFreshness {
            #expect(!TaskSubmissionModel.canCompose(freshness: freshness))
        }
    }

    @Test func everyClosedComposerStatesItsReason() {
        #expect(TaskSubmissionModel.composeBlockedReason(freshness: .fresh) == nil)
        #expect(TaskSubmissionModel.composeBlockedReason(freshness: .unvalidated) == nil)
        for freshness in Self.failingFreshness {
            let reason = TaskSubmissionModel.composeBlockedReason(freshness: freshness)
            #expect(reason?.isEmpty == false)
        }
    }

    private static let failingFreshness: [InboxStore.Freshness] = [
        .unreachable,
        .syncFailing,
        .contractMismatch(daemonContract: "sha256:" + String(repeating: "0", count: 64)),
        .unauthenticated,
    ]
}

/// The daemon operations a test observed, in issue order.
private actor OperationLog {
    private(set) var ids: [String] = []

    func append(_ id: String) {
        ids.append(id)
    }
}
