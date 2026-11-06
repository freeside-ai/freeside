import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// Submitting with attachments (#2590): the bytes are saved beside the
/// command, uploaded by digest before it is sent, re-uploaded on retry, and
/// deleted once no saved submission names them.
@Suite @MainActor struct TaskSubmissionAttachmentTests {
    private static let png = Data([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3])
    private static let notes = Data("Notes for the agent.".utf8)

    private static func draft() -> TaskAttachmentDraft {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "screen.png", claimedMediaType: "image/png", bytes: png)
        _ = draft.add(name: "notes.txt", claimedMediaType: "text/plain", bytes: notes)
        return draft
    }

    private struct Fixture {
        let server: MockServer
        let cache: InMemoryCacheStore
        let saved: InMemorySubmissionAttachmentStore
        let coordinator: SyncCoordinator
        let model: TaskSubmissionModel
    }

    private func fixture(server: MockServer = MockServer()) -> Fixture {
        let cache = InMemoryCacheStore()
        let saved = InMemorySubmissionAttachmentStore()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: cache, submissionAttachments: saved)
        return Fixture(
            server: server, cache: cache, saved: saved, coordinator: coordinator,
            model: TaskSubmissionModel(coordinator: coordinator))
    }

    private func expectAttachments(on taskID: String, in coordinator: SyncCoordinator) throws {
        let task = try #require(coordinator.tasks.first { $0.task.id == taskID }).task
        #expect(task.attachments.map(\.name) == ["screen.png", "notes.txt"])
        #expect(task.attachments.map(\.digest) == Self.draft().items.map(\.digest))
        #expect(task.attachments.map(\.media_type) == [.image_sol_png, .application_sol_octet_hyphen_stream])
        #expect(task.attachments.map(\.claimed_media_type) == ["image/png", "text/plain"])
    }

    @Test func submitUploadsThenLandsOnATaskListingTheAttachments() async throws {
        let f = fixture()
        let operations = OperationLog()
        await f.server.setBeforeRespond { await operations.append($0) }
        let taskID = try #require(
            await f.model.submit(projectID: "project-1", source: "Fix the layout.", attachments: Self.draft()))
        try expectAttachments(on: taskID, in: f.coordinator)
        let issued = await operations.ids
        #expect(issued.filter { $0 == "uploadAttachment" }.count == 2)
        #expect(issued.lastIndex(of: "uploadAttachment")! < issued.firstIndex(of: "submitCommand")!)
        #expect(f.saved.storedDigests().isEmpty)
        #expect(f.coordinator.pendingTaskSubmissions.isEmpty)
    }

    @Test func aTextOnlyCommandCarriesNoAttachmentsField() async throws {
        let f = fixture()
        let lost = InjectedFailures(times: 1)
        await f.server.setAfterRespond { if $0 == "submitCommand" { try await lost.consume() } }
        _ = await f.model.submit(projectID: "project-1", source: "Text only.")
        #expect(f.model.state == .lost)
        let command = try #require(f.coordinator.pendingTaskSubmissions.values.first)
        guard case .submit_task(let payload) = command.payload else {
            Issue.record("expected a submit_task command")
            return
        }
        #expect(payload.attachments == nil)
    }

    @Test func anUploadFailureKeepsTheCommandAndRetryUploadsAndSubmitsIt() async throws {
        let f = fixture()
        let operations = OperationLog()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { operation in
            await operations.append(operation)
            if operation == "uploadAttachment" { try await down.consume() }
        }
        #expect(await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft()) == nil)
        #expect(f.model.state == .lost)
        #expect(!(await operations.ids).contains("submitCommand"))
        let saved = f.coordinator.pendingTaskSubmissions
        #expect(saved.count == 1)
        #expect(f.saved.storedDigests() == Set(Self.draft().items.map(\.digest)))

        // A relaunch restores the command and its bytes; Retry re-uploads
        // and sends the same command.
        let restored = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: f.cache, submissionAttachments: f.saved)
        #expect(restored.pendingTaskSubmissions == saved)
        let recovery = TaskSubmissionModel(coordinator: restored)
        #expect(recovery.selectPending(try #require(saved.keys.first)) != nil)
        let taskID = try #require(await recovery.retry())
        try expectAttachments(on: taskID, in: restored)
        #expect(f.saved.storedDigests().isEmpty)
        #expect(restored.pendingTaskSubmissions.isEmpty)
    }

    @Test func aFolderUploadsEveryFileAndRetryResendsTheSavedCommand() async throws {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "design", claimedMediaType: "text/plain", bytes: Self.notes)
        _ = draft.addFolder(
            name: "design",
            files: [
                FolderFile(path: "brief.md", claimedMediaType: "text/markdown", bytes: Data("brief".utf8)),
                FolderFile(path: "screens/home.png", claimedMediaType: "image/png", bytes: Self.png),
            ])
        let names = ["design", "design 2/brief.md", "design 2/screens/home.png"]
        #expect(draft.items.map(\.name) == names)

        let f = fixture()
        let operations = OperationLog()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { operation in
            await operations.append(operation)
            if operation == "uploadAttachment" { try await down.consume() }
        }
        #expect(await f.model.submit(projectID: "project-1", source: "Match the handoff.", attachments: draft) == nil)
        let saved = try #require(f.coordinator.pendingTaskSubmissions.values.first)
        guard case .submit_task(let payload) = saved.payload else {
            Issue.record("expected a submit_task command")
            return
        }
        #expect(payload.attachments?.map(\.name) == names)

        let taskID = try #require(await f.model.retry())
        let issued = await operations.ids
        // One failed upload, then all three on Retry, then the command.
        #expect(issued.filter { $0 == "uploadAttachment" }.count == 4)
        #expect(issued.filter { $0 == "submitCommand" }.count == 1)
        let task = try #require(f.coordinator.tasks.first { $0.task.id == taskID }).task
        #expect(task.attachments.map(\.name) == names)
        #expect(task.attachments.map(\.digest) == draft.items.map(\.digest))
        #expect(f.coordinator.pendingTaskSubmissions.isEmpty)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aRetryAfterALostResponseReuploadsStoredDigestsAsANoOp() async throws {
        let f = fixture()
        let lost = InjectedFailures(times: 1)
        await f.server.setAfterRespond { if $0 == "submitCommand" { try await lost.consume() } }
        #expect(await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft()) == nil)
        #expect(f.model.state == .lost)
        let taskID = try #require(await f.model.retry())
        try expectAttachments(on: taskID, in: f.coordinator)
        #expect(f.coordinator.tasks.filter { $0.task.id == taskID }.count == 1)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func bytesThatNoLongerHashToTheirDigestFinishAsRejected() async throws {
        let f = fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        let digest = Self.draft().items[0].digest
        try f.saved.save(digest: digest, bytes: Data("tampered".utf8))
        #expect(await f.model.retry() == nil)
        #expect(f.model.state == .rejected("screen.png changed after it was saved, so nothing was submitted."))
        #expect(f.coordinator.pendingTaskSubmissions.isEmpty)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aRefusedUploadFinishesAsRejectedAndAnUnauthenticatedOneStaysLost() async throws {
        let f = fixture()
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        await status.set(401)
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        #expect(f.model.state == .lost)
        #expect(f.coordinator.store.freshness == .unauthenticated)
        #expect(f.coordinator.pendingTaskSubmissions.count == 1)

        await status.set(413)
        #expect(await f.model.retry() == nil)
        #expect(
            f.model.state == .rejected("the daemon refused screen.png (status 413), so nothing was submitted"))
        #expect(f.coordinator.pendingTaskSubmissions.isEmpty)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aServerErrorOrForbiddenUploadStaysLostWithItsBytes() async throws {
        let f = fixture()
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        await status.set(503)
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        #expect(f.model.state == .lost)
        #expect(f.coordinator.store.freshness != .unauthenticated)

        await status.set(403)
        #expect(await f.model.retry() == nil)
        #expect(f.model.state == .lost)
        #expect(f.coordinator.store.freshness == .unauthenticated)
        #expect(f.coordinator.pendingTaskSubmissions.count == 1)
        #expect(f.saved.storedDigests() == Set(Self.draft().items.map(\.digest)))
    }

    @Test func bytesTwoSavedSubmissionsShareStayUntilBothFinish() async throws {
        let f = fixture()
        let down = InjectedFailures(times: 2)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        let first = f.model
        let second = TaskSubmissionModel(coordinator: f.coordinator)
        _ = await first.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        _ = await second.submit(projectID: "project-1", source: "Fix it again.", attachments: Self.draft())
        #expect(f.coordinator.pendingTaskSubmissions.count == 2)

        _ = try #require(await first.retry())
        #expect(f.saved.storedDigests() == Set(Self.draft().items.map(\.digest)))
        _ = try #require(await second.retry())
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aDaemonRejectionDeletesTheSavedBytes() async throws {
        let f = fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        #expect(f.model.state == .lost)
        // With one saved copy gone, the retry skips its upload and sends a
        // digest the daemon doesn't hold, which it answers with a 400.
        f.saved.delete(digest: Self.draft().items[0].digest)
        #expect(await f.model.retry() == nil)
        guard case .rejected = f.model.state else {
            Issue.record("expected a rejection, got \(f.model.state)")
            return
        }
        #expect(f.coordinator.pendingTaskSubmissions.isEmpty)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func launchDeletesBytesNoSavedSubmissionNames() async throws {
        let f = fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())
        let stray = TaskAttachmentDraft.digest(of: Data("stray".utf8))
        try f.saved.save(digest: stray, bytes: Data("stray".utf8))

        _ = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: f.cache, submissionAttachments: f.saved)
        #expect(f.saved.storedDigests() == Set(Self.draft().items.map(\.digest)))

        // Another daemon's coordinator restores none of these commands, so
        // it keeps none of their bytes.
        _ = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: f.cache, submissionDaemonID: "daemon-b",
            submissionAttachments: f.saved)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aLaunchThatCantReadTheCacheKeepsEveryByte() async throws {
        let f = fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft())

        // Before first unlock an iPhone can list the saved bytes but can't
        // read the cache that names them.
        _ = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: UnreadableCacheStore(),
            submissionAttachments: f.saved)
        #expect(f.saved.storedDigests() == Set(Self.draft().items.map(\.digest)))
    }

    @Test func aFailedSaveKeepsNoBytesAndSendsNothing() async throws {
        let cache = RefusingCacheStore()
        let saved = InMemorySubmissionAttachmentStore()
        let server = MockServer()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: cache, submissionAttachments: saved)
        let model = TaskSubmissionModel(coordinator: coordinator)
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }
        cache.refuses = true
        #expect(await model.submit(projectID: "project-1", source: "Fix it.", attachments: Self.draft()) == nil)
        guard case .rejected = model.state else {
            Issue.record("expected a rejection, got \(model.state)")
            return
        }
        #expect(saved.storedDigests().isEmpty)
        #expect(!(await operations.ids).contains("uploadAttachment"))
    }
}

/// The next forced status for a scripted operation, or nil to answer it.
private actor ScriptedStatus {
    private var code: Int?
    func set(_ code: Int?) { self.code = code }
    func next() -> Int? { code }
}

/// A cache that can't be read, as a protected file before first unlock.
private struct UnreadableCacheStore: CacheStore {
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws {}
    func discard() {}
}

/// A cache whose saves fail while `refuses` is set.
private final class RefusingCacheStore: CacheStore, @unchecked Sendable {
    struct Refused: Error {}
    private let backing = InMemoryCacheStore()
    private let lock = NSLock()
    private var _refuses = false
    var refuses: Bool {
        get { lock.withLock { _refuses } }
        set { lock.withLock { _refuses = newValue } }
    }
    func load() -> CachedState? { backing.load() }
    func save(_ state: CachedState) throws {
        if refuses { throw Refused() }
        try backing.save(state)
    }
    func discard() { backing.discard() }
}
