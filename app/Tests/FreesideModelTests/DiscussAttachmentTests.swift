import Foundation
import FreesideTestSupport
import Testing

@testable import FreesideAPI
@testable import FreesideModel

/// A spec approval's Discuss with attachments (#2995): the bytes are saved
/// with the pending command, uploaded by digest in order before it is sent,
/// uploaded again on Retry, and deleted once no pending command or saved
/// task submission names them.
@Suite @MainActor struct DiscussAttachmentTests {
    private static let png = Data([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3])
    private static let notes = Data("Notes for the agent.".utf8)
    private static let specItemID = "item-spec_approval"

    private static func attachments() -> [DraftAttachment] {
        var draft = TaskAttachmentDraft(for: .message)
        _ = draft.add(name: "screen.png", claimedMediaType: "image/png", bytes: png)
        _ = draft.add(name: "notes.txt", claimedMediaType: "text/plain", bytes: notes)
        return draft.items
    }

    private static var digests: [String] { attachments().map(\.digest) }

    private struct Fixture {
        let server: MockServer
        let cache: InMemoryCacheStore
        let saved: InMemorySubmissionAttachmentStore
        let coordinator: SyncCoordinator
        let model: DecisionModel
    }

    private func fixture(itemID: String = specItemID) async -> Fixture {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let saved = InMemorySubmissionAttachmentStore()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: cache, submissionAttachments: saved)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: itemID, openURL: { _ in true })
        await model.validate()
        return Fixture(server: server, cache: cache, saved: saved, coordinator: coordinator, model: model)
    }

    @Test func uploadsInOrderThenSendsTheCommandListingTheDigests() async throws {
        let f = await fixture()
        let operations = OperationLog()
        await f.server.setBeforeRespond { await operations.append($0) }

        #expect(await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments()))

        let issued = await operations.ids
        #expect(issued.filter { $0 == "uploadAttachment" }.count == 2)
        #expect(issued.lastIndex(of: "uploadAttachment")! < issued.firstIndex(of: "submitCommand")!)
        #expect(f.model.appliedRecord?.action == .discuss)
        #expect(f.model.appliedRecord?.attachments == Self.digests)
        #expect(f.model.pendingCommand == nil)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func theFirstAttachmentUploadsBeforeTheSecond() async throws {
        let f = await fixture()
        let uploads = CallCounter()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", await uploads.next() == 2 { throw InjectedFailure() }
        }
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(await f.server.attachmentBytes(digest: Self.digests[0]) == Self.png)
        #expect(await f.server.attachmentBytes(digest: Self.digests[1]) == nil)
        #expect(f.model.submissionError == "notes.txt didn't upload: the daemon couldn't be reached")
    }

    @Test func aTextOnlyDiscussCarriesNoAttachmentsField() async throws {
        let f = await fixture()
        let lost = InjectedFailures(times: 2)
        await f.server.setAfterRespond { if $0 == "submitCommand" { try await lost.consume() } }
        _ = await f.model.submitDiscuss(message: "Text only.")
        let command = try #require(f.model.pendingCommand)
        #expect(command.decisionPayload?.attachments == nil)
    }

    @Test func aFailedUploadKeepsTheCommandAndRetrySendsTheSameOne() async throws {
        let f = await fixture()
        let operations = OperationLog()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { operation in
            await operations.append(operation)
            if operation == "uploadAttachment" { try await down.consume() }
        }

        #expect(await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments()))
        #expect(!(await operations.ids).contains("submitCommand"))
        let pending = try #require(f.model.pendingCommand)
        #expect(pending.decisionPayload?.attachments == Self.digests)
        #expect(f.model.canRetryLostResponse)
        #expect(f.model.unsentAfterFailedUpload)
        #expect(f.model.submissionError == "screen.png didn't upload: the daemon couldn't be reached")
        #expect(f.saved.storedDigests() == Set(Self.digests))

        await f.model.retryLostResponse()
        #expect(f.model.appliedRecord?.command_id == pending.command_id)
        #expect(f.model.appliedRecord?.attachments == Self.digests)
        #expect(!f.model.unsentAfterFailedUpload)
        #expect(f.model.pendingCommand == nil)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func bytesThatNoLongerHashToTheirDigestFinishAsNotSent() async throws {
        let f = await fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        try f.saved.save(digest: Self.digests[0], bytes: Data("tampered".utf8))

        await f.model.retryLostResponse()
        #expect(f.model.submissionError == "screen.png changed after it was saved, so the message wasn't sent")
        #expect(f.model.pendingCommand == nil)
        #expect(!f.model.canRetryLostResponse)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aRefusedUploadFinishesAsNotSentAndAnUnauthenticatedOneStaysRetryable() async throws {
        let f = await fixture()
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        await status.set(401)
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(f.model.canRetryLostResponse)
        #expect(f.model.submissionError == "screen.png didn't upload (status 401)")
        #expect(f.coordinator.store.freshness == .unauthenticated)

        await status.set(413)
        await f.model.retryLostResponse()
        #expect(f.model.submissionError == "the daemon refused screen.png (status 413), so the message wasn't sent")
        #expect(f.model.pendingCommand == nil)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aServerErrorOrForbiddenUploadKeepsTheCommandAndItsBytes() async throws {
        let f = await fixture()
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        await status.set(503)
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(f.model.canRetryLostResponse)
        #expect(f.coordinator.store.freshness != .unauthenticated)

        await status.set(403)
        await f.model.retryLostResponse()
        #expect(f.model.canRetryLostResponse)
        #expect(f.model.submissionError == "screen.png didn't upload (status 403)")
        #expect(f.coordinator.store.freshness == .unauthenticated)
        #expect(f.saved.storedDigests() == Set(Self.digests))
    }

    @Test func aRelaunchKeepsTheBytesAndRetrySendsThem() async throws {
        let f = await fixture()
        let down = InjectedFailures(times: 1)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        let pending = try #require(f.model.pendingCommand)
        let stray = TaskAttachmentDraft.digest(of: Data("stray".utf8))
        try f.saved.save(digest: stray, bytes: Data("stray".utf8))

        // The launch sweep deletes the stray and keeps what the restored
        // command names.
        let relaunched = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: f.cache, submissionAttachments: f.saved)
        #expect(f.saved.storedDigests() == Set(Self.digests))
        let model = DecisionModel(store: relaunched.store, itemID: Self.specItemID, openURL: { _ in true })
        #expect(model.pendingCommand == pending)
        #expect(model.canRetryLostResponse)
        #expect(!model.unsentAfterFailedUpload)

        await model.retryLostResponse()
        #expect(model.appliedRecord?.command_id == pending.command_id)
        #expect(model.appliedRecord?.attachments == Self.digests)
        #expect(model.pendingCommand == nil)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aRestoredCommandNamesAFailedAttachmentByPosition() async throws {
        let f = await fixture()
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        await status.set(503)
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        let relaunched = SyncCoordinator(
            client: APIClientFactory.mock(server: f.server), cache: f.cache, submissionAttachments: f.saved)
        let model = DecisionModel(store: relaunched.store, itemID: Self.specItemID, openURL: { _ in true })
        await model.retryLostResponse()
        #expect(model.submissionError == "attachment 1 didn't upload (status 503)")
    }

    @Test func bytesADiscussAndATaskSubmissionShareStayUntilBothFinish() async throws {
        let f = await fixture()
        let down = InjectedFailures(times: 2)
        await f.server.setBeforeRespond { if $0 == "uploadAttachment" { try await down.consume() } }
        let submission = TaskSubmissionModel(coordinator: f.coordinator)
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "screen.png", claimedMediaType: "image/png", bytes: Self.png)
        _ = await submission.submit(projectID: "project-1", source: "Fix it.", attachments: draft)
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(f.saved.storedDigests() == Set(Self.digests))

        await f.model.retryLostResponse()
        #expect(f.model.pendingCommand == nil)
        #expect(f.saved.storedDigests() == [Self.digests[0]])
        _ = try #require(await submission.retry())
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func aDiscussThatCantBeSavedSendsNothingAndKeepsNoBytes() async throws {
        let server = MockServer()
        let cache = RefusingCacheStore()
        let saved = InMemorySubmissionAttachmentStore()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: cache, submissionAttachments: saved)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: Self.specItemID, openURL: { _ in true })
        await model.validate()
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }
        cache.refuses = true

        #expect(!(await model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())))
        #expect(model.pendingCommand == nil)
        #expect(saved.storedDigests().isEmpty)
        #expect(!(await operations.ids).contains("uploadAttachment"))
    }

    @Test func aDiscussWhoseBytesCantBeSavedSendsNothing() async throws {
        let server = MockServer()
        let saved = RefusingAttachmentStore()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore(), submissionAttachments: saved)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: Self.specItemID, openURL: { _ in true })
        await model.validate()
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }

        #expect(!(await model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())))
        #expect(model.pendingCommand == nil)
        #expect(saved.storedDigests().isEmpty)
        #expect(await operations.ids.isEmpty)
    }

    @Test func aSentDiscussKeepsItsBytesWhileTheCacheStillNamesIt() async throws {
        let server = MockServer()
        let cache = RefusingCacheStore()
        let saved = InMemorySubmissionAttachmentStore()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: cache, submissionAttachments: saved)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: Self.specItemID, openURL: { _ in true })
        await model.validate()
        await server.setBeforeRespond { if $0 == "submitCommand" { cache.refuses = true } }

        _ = await model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(model.pendingCommand == nil)
        #expect(saved.storedDigests() == Set(Self.digests))
    }

    @Test func aRetryTheDaemonRejectsFinishesAndDeletesTheBytes() async throws {
        let f = await fixture()
        let uploadDown = InjectedFailures(times: 1)
        let status = ScriptedStatus()
        await f.server.setBeforeRespond { operation in
            if operation == "uploadAttachment" { try await uploadDown.consume() }
            if operation == "submitCommand", let code = await status.next() { throw MockServer.ForcedStatus(code) }
        }
        _ = await f.model.submitDiscuss(message: "Does this match?", attachments: Self.attachments())
        #expect(f.model.canRetryLostResponse)

        await status.set(400)
        await f.model.retryLostResponse()
        #expect(f.model.pendingCommand == nil)
        #expect(!f.model.unsentAfterFailedUpload)
        #expect(f.saved.storedDigests().isEmpty)
    }

    @Test func onlyASpecApprovalDiscussCarriesAttachments() async throws {
        let f = await fixture(itemID: "item-execution_failure")
        #expect(f.model.offeredActions.contains(.discuss))
        #expect(!f.model.acceptsDiscussAttachments)
        let operations = OperationLog()
        await f.server.setBeforeRespond { await operations.append($0) }

        #expect(!(await f.model.submitDiscuss(message: "What failed?", attachments: Self.attachments())))
        #expect(f.model.submissionError == "only a specification discussion can carry attachments")
        #expect(f.model.pendingCommand == nil)
        #expect(f.saved.storedDigests().isEmpty)
        #expect(await operations.ids.isEmpty)

        let spec = await fixture()
        #expect(spec.model.acceptsDiscussAttachments)
    }
}

/// Counts calls, returning each call's 1-based number.
private actor CallCounter {
    private var count = 0
    func next() -> Int {
        count += 1
        return count
    }
}

/// The next forced status for a scripted operation, or nil to answer it.
private actor ScriptedStatus {
    private var code: Int?
    func set(_ code: Int?) { self.code = code }
    func next() -> Int? { code }
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

/// An attachment store whose every save fails.
private final class RefusingAttachmentStore: SubmissionAttachmentStore, @unchecked Sendable {
    struct Refused: Error {}
    private let backing = InMemorySubmissionAttachmentStore()
    func save(digest: String, bytes: Data) throws { throw Refused() }
    func load(digest: String) -> Data? { backing.load(digest: digest) }
    func delete(digest: String) { backing.delete(digest: digest) }
    func storedDigests() -> Set<String> { backing.storedDigests() }
}
