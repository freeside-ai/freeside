import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct TaskPromptHistoryTests {
    @Test func navigationPreservesDraftAndStopsAtBothEnds() {
        let browse = TaskPromptBrowseState()
        let draft = "  draft\n\t\n"
        #expect(browse.up(from: draft, history: []) == nil)
        #expect(browse.down() == nil)
        #expect(browse.up(from: draft, history: ["new", "old"]) == "new")
        #expect(browse.up(from: "new", history: ["changed"]) == "old")
        #expect(browse.up(from: "old", history: []) == "old")
        #expect(browse.down() == "new")
        #expect(browse.down() == draft)
        #expect(browse.down() == nil)
        #expect(!browse.isBrowsing)
    }

    @Test func projectSwitchRestoresDraftAndEditingPromotesRecall() {
        let browse = TaskPromptBrowseState()
        _ = browse.up(from: "original", history: ["project-a"])
        #expect(browse.end() == "original")
        #expect(browse.up(from: "original", history: ["project-b"]) == "project-b")
        browse.edited()
        #expect(browse.end() == nil)
        #expect(browse.down() == nil)
        _ = browse.up(from: "edited project-b", history: ["other"])
        #expect(browse.down() == "edited project-b")
    }

    @Test func historyIsBoundedFilteredAndCoalescesConfirmedReplays() {
        let history = TaskPromptHistory(deploymentID: "daemon", deviceID: "device")
        history.record(projectID: "a", source: "one", taskID: "1")
        history.record(projectID: "b", source: "other", taskID: "2")
        history.record(projectID: "a", source: "one", taskID: "3")
        history.record(projectID: "a", source: "two", taskID: "4")
        history.record(projectID: "a", source: "one", taskID: "1")
        history.record(projectID: "a", source: "one", taskID: "3")
        #expect(history.prompts(for: "a") == ["two", "one"])
        #expect(history.prompts(for: "b") == ["other"])
        #expect(history.prompts(for: "missing").isEmpty)
        for i in 5...60 {
            history.record(projectID: "a", source: "work \(i)", taskID: "\(i)")
        }
        #expect(history.entries.count == 50)
        #expect(history.prompts(for: "a").first == "work 60")
        #expect(history.prompts(for: "a").last == "work 11")
        #expect(history.prompts(for: "b").isEmpty)
    }

    @Test func diskRoundTripAndOwnership() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        func load(_ deployment: String = "daemon", _ device: String = "device") -> TaskPromptHistory {
            TaskPromptHistory(deploymentID: deployment, deviceID: device, directory: directory)
        }
        #expect(load().entries.isEmpty)
        let source = "  exact\nsource\n"
        load().record(projectID: "a", source: source, taskID: "1")
        #expect(load().prompts(for: "a") == [source])
        #expect(load("foreign").entries.isEmpty)
        #expect(load("daemon", "another-device").entries.isEmpty)
        #expect(load().prompts(for: "a") == [source])
    }

    @Test func malformedAndInvalidRecordsStartEmpty() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        func load() -> TaskPromptHistory {
            TaskPromptHistory(deploymentID: "daemon", deviceID: "device", directory: directory)
        }
        load().record(projectID: "a", source: "valid", taskID: "1")
        let file = try #require(
            FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil).first)
        try Data("not json".utf8).write(to: file)
        #expect(load().entries.isEmpty)
        let invalidEntries: [[TaskPromptHistory.Entry]] = [
            [.init(taskIDs: ["1"], projectID: "", source: "text")],
            [.init(taskIDs: ["1"], projectID: "a", source: " \n")],
            [.init(taskIDs: [], projectID: "a", source: "text")],
            [.init(taskIDs: [""], projectID: "a", source: "text")],
            Array(repeating: .init(taskIDs: ["1"], projectID: "a", source: "text"), count: 51),
        ]
        for entries in invalidEntries {
            try JSONEncoder().encode(
                TaskPromptHistory.Record(version: 1, deploymentID: "daemon", deviceID: "device", entries: entries)
            ).write(to: file)
            #expect(load().entries.isEmpty)
        }
        let history = load()
        history.record(projectID: "a", source: " \n", taskID: "1")
        history.record(projectID: "", source: "valid", taskID: "2")
        history.record(projectID: "a", source: "valid", taskID: "")
        #expect(history.entries.isEmpty)
    }

    @Test func saveFailureDoesNotRejectAcceptedSubmission() async throws {
        struct Refused: Error {}
        let history = TaskPromptHistory(
            deploymentID: "mock", deviceID: "device-mock", directory: URL(fileURLWithPath: "/unused"),
            write: { _, _ in throw Refused() })
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(), cache: InMemoryCacheStore(), promptHistory: history)
        let model = TaskSubmissionModel(coordinator: coordinator)
        let taskID = try #require(await model.submit(projectID: "project-1", source: "Accepted work"))
        #expect(model.state == .submitted(taskID: taskID))
        #expect(coordinator.pendingTaskSubmissions.isEmpty)
        #expect(history.prompts(for: "project-1") == ["Accepted work"])
        #expect(history.saveWarning?.contains("couldn't be saved") == true)
    }

    @Test func confirmedRecoveryRecordsSavedSourceAndFreshSubmitCreatesNewWork() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let coordinator = SyncCoordinator(client: client, cache: InMemoryCacheStore())
        let failures = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await failures.consume() }
        }
        let first = TaskSubmissionModel(coordinator: coordinator)
        #expect(await first.submit(projectID: "project-1", source: "  Submitted\nsource\n") == nil)
        #expect(coordinator.promptHistory.entries.isEmpty)
        let saved = try #require(coordinator.pendingTaskSubmissions.values.first)
        let recovery = TaskSubmissionModel(coordinator: coordinator)
        #expect(recovery.selectPending(saved.command_id) != nil)
        #expect(coordinator.promptHistory.entries.isEmpty)
        let recovered = try #require(await recovery.retry())
        #expect(coordinator.promptHistory.prompts(for: "project-1") == ["  Submitted\nsource\n"])
        let newWork = try #require(await first.submit(projectID: "project-1", source: "  Submitted\nsource\n"))
        #expect(newWork != recovered)
        #expect(coordinator.promptHistory.entries.count == 1)
        #expect(await recovery.retry() == recovered)
        #expect(coordinator.promptHistory.entries.count == 1)
    }

    @Test func rejectionAndCancellationDoNotRecordHistory() async {
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(400) }
        }
        let model = TaskSubmissionModel(coordinator: coordinator)
        #expect(await model.submit(projectID: "project-1", source: "Rejected") == nil)
        #expect(coordinator.promptHistory.entries.isEmpty)
        let cancelled = Task { @MainActor in
            withUnsafeCurrentTask { $0?.cancel() }
            return await model.submit(projectID: "project-1", source: "Cancelled")
        }
        #expect(await cancelled.value == nil)
        #expect(coordinator.promptHistory.entries.isEmpty)
    }

    @Test func sessionReconstructionPairingAndServerSwitchKeepHistoryScoped() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let serverA = try #require(URL(string: "http://history-a.example:7331"))
        let serverB = try #require(URL(string: "http://history-b.example:7331"))
        let credential = try #require(
            DeviceCredential(
                deviceID: "device-a", token: testDeviceToken(for: "device-a"), ntfySubscription: .mock))
        let credentials = InMemoryCredentialStore(credential: credential)
        func session(_ url: URL?, persistent: Bool = true) -> AppSession {
            AppSession(
                client: APIClientFactory.mock(), credentials: credentials, cache: InMemoryCacheStore(),
                deploymentURL: url, cacheRoot: persistent ? root : nil, credentialStore: { _ in credentials },
                persistServerURL: { _ in })
        }
        func coordinator(_ session: AppSession) throws -> SyncCoordinator {
            guard case .ready(let coordinator) = session.phase else {
                throw CocoaError(.coderInvalidValue)
            }
            return coordinator
        }
        let first = session(serverA)
        try coordinator(first).promptHistory.record(projectID: "a", source: "remember me", taskID: "1")
        #expect(try coordinator(session(serverA)).promptHistory.prompts(for: "a") == ["remember me"])
        #expect(try coordinator(session(serverB)).promptHistory.entries.isEmpty)
        first.connect(serverURL: serverB)
        #expect(try coordinator(first).promptHistory.entries.isEmpty)
        first.connect(serverURL: serverA)
        #expect(try coordinator(first).promptHistory.prompts(for: "a") == ["remember me"])
        first.completePairing(
            try #require(
                DeviceCredential(
                    deviceID: "device-b", token: testDeviceToken(for: "device-b"), ntfySubscription: .mock)))
        #expect(try coordinator(first).promptHistory.entries.isEmpty)
        first.completePairing(credential)
        #expect(try coordinator(first).promptHistory.prompts(for: "a") == ["remember me"])
        #expect(try coordinator(session(nil)).promptHistory.entries.isEmpty)
        #expect(try coordinator(session(serverA, persistent: false)).promptHistory.entries.isEmpty)
    }
}
