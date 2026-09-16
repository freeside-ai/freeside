import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct TaskSubmissionRecoveryTests {
    @Test func newComposerStaysSeparateFromSavedRecovery() async throws {
        let client = APIClientFactory.mock()
        let coordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
        let saved = Components.Schemas.ClientCommand(
            command_id: "saved-command", device_id: "device-mock",
            payload: .submit_task(.init(kind: .submit_task, project_id: "project-1", source: "Saved work")))
        #expect(coordinator.retainTaskSubmission(saved))
        let recovery = TaskSubmissionModel(coordinator: coordinator)
        #expect(recovery.selectPending(saved.command_id) != nil)
        let composer = TaskSubmissionModel(coordinator: coordinator)
        #expect(composer.state == .idle)
        #expect(await composer.retry() == nil)
        let newTask = try #require(await composer.submit(projectID: "project-1", source: "Saved work"))
        #expect(coordinator.pendingTaskSubmissions == [saved.command_id: saved])
        let originalTask = try #require(await recovery.retry())
        #expect(originalTask != newTask)
        #expect(coordinator.pendingTaskSubmissions.isEmpty)
    }

    @Test func restartRestoresSeparateCommandsWithoutSending() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let client = APIClientFactory.mock(server: server)
        let first = SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a")
        let lost = InjectedFailures(times: 2)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        // Dismissing the first composer and opening another must not replace
        // its saved command, even when both drafts contain identical text.
        _ = await TaskSubmissionModel(coordinator: first).submit(projectID: "project-1", source: "Add health checks.")
        _ = await TaskSubmissionModel(coordinator: first).submit(projectID: "project-1", source: "Add health checks.")
        let saved = first.pendingTaskSubmissions
        #expect(saved.count == 2)
        let before = try await client.listRuns().ok.body.json

        let restored = SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a")
        await restored.refresh()
        #expect(restored.pendingTaskSubmissions == saved)
        let after = try await client.listRuns().ok.body.json
        #expect(after == before)
        let model = TaskSubmissionModel(coordinator: restored)
        let ids = saved.keys.sorted()
        #expect(model.selectPending(ids[0])?.source == "Add health checks.")
        #expect(restored.pendingTaskSubmissions == saved)
        let taskA = try #require(await model.retry())
        #expect(restored.pendingTaskSubmissions.count == 1)
        #expect(restored.pendingTaskSubmissions[ids[1]] == saved[ids[1]])
        #expect(model.selectPending(ids[1]) != nil)
        let taskB = try #require(await model.retry())
        #expect(taskA != taskB)
        #expect(restored.pendingTaskSubmissions.isEmpty)
        #expect(try await client.listRuns().ok.body.json == before)
        #expect(
            SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a").pendingTaskSubmissions.isEmpty
        )
    }

    @Test func pendingCommandsCannotCrossOwnership() async throws {
        let client = APIClientFactory.mock()
        let cache = InMemoryCacheStore()
        let owner = SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a")
        let command = Components.Schemas.ClientCommand(
            command_id: "saved-command", device_id: "device-mock",
            payload: .submit_task(.init(kind: .submit_task, project_id: "project-1", source: "Saved work")))
        #expect(owner.retainTaskSubmission(command))
        #expect(
            SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-b").pendingTaskSubmissions.isEmpty
        )
        #expect(
            SyncCoordinator(
                client: client, device: .init(deviceID: "another-device"), cache: cache,
                submissionDaemonID: "daemon-a"
            ).pendingTaskSubmissions.isEmpty)
        #expect(
            SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a").pendingTaskSubmissions.count
                == 1)
    }

    @Test(arguments: [401, 403])
    func authenticationFailurePreservesLostSubmissionAcrossRestart(status: Int) async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let client = APIClientFactory.mock(server: server)
        let first = SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a")
        let model = TaskSubmissionModel(coordinator: first)
        let attempts = Counter()
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { await attempts.increment() }
        }
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        #expect(await model.submit(projectID: "project-1", source: "Keep the original request.") == nil)
        let saved = first.pendingTaskSubmissions
        let command = try #require(saved.values.first)
        let acceptedRuns = try await client.listRuns().ok.body.json

        await server.setBeforeRespond { operation in
            if operation == "submitCommand" {
                await attempts.increment()
                throw MockServer.ForcedStatus(status)
            }
        }
        #expect(await model.retry() == nil)
        #expect(model.state == .lost)
        #expect(first.store.freshness == .unauthenticated)
        #expect(first.pendingTaskSubmissions == saved)
        #expect(cache.load()?.pendingTaskSubmissions == saved)

        let restored = SyncCoordinator(client: client, cache: cache, submissionDaemonID: "daemon-a")
        let recovered = TaskSubmissionModel(coordinator: restored)
        await restored.refresh()
        #expect(restored.pendingTaskSubmissions == saved)
        #expect(recovered.selectPending(command.command_id) != nil)
        #expect(await attempts.count == 2)

        // Restore authentication for the same device. Re-pairing as a new
        // device remains forbidden by the ledger's ownership boundary.
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { await attempts.increment() }
        }
        let taskID = try #require(await recovered.retry())
        #expect(await attempts.count == 3)
        #expect(acceptedRuns.contains { $0.run.task_id == taskID })
        #expect(try await client.listRuns().ok.body.json == acceptedRuns)
        #expect(restored.pendingTaskSubmissions.isEmpty)
        #expect(cache.load()?.pendingTaskSubmissions?.isEmpty == true)
    }

    @Test func failedPersistencePreventsSending() async throws {
        let client = APIClientFactory.mock()
        let before = try await client.listRuns().ok.body.json
        let coordinator = SyncCoordinator(client: client, cache: RefusingSubmissionCache())
        let model = TaskSubmissionModel(coordinator: coordinator)
        #expect(await model.submit(projectID: "project-1", source: "Never sent") == nil)
        guard case .rejected = model.state else {
            Issue.record("Expected a local persistence refusal")
            return
        }
        #expect(coordinator.pendingTaskSubmissions.isEmpty)
        #expect(try await client.listRuns().ok.body.json == before)
    }

    @Test func diskRoundTripPreservesExactCommandWithoutCursors() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let command = Components.Schemas.ClientCommand(
            command_id: "saved-command", device_id: "device-mock",
            payload: .submit_task(
                .init(kind: .submit_task, project_id: "project-1", source: "  Exact\nsource\n", name: "Name")))
        let owner = SyncCoordinator(client: APIClientFactory.mock(), cache: cache, submissionDaemonID: "daemon-a")
        #expect(owner.retainTaskSubmission(command))
        let restored = SyncCoordinator(client: APIClientFactory.mock(), cache: cache, submissionDaemonID: "daemon-a")
        #expect(restored.pendingTaskSubmissions == [command.command_id: command])
        #expect(restored.cursors == nil)
    }
}

private struct RefusingSubmissionCache: CacheStore {
    struct Unavailable: Error {}
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws { throw Unavailable() }
    func discard() {}
}
