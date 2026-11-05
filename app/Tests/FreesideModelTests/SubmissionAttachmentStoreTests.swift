import Foundation
import Testing

@testable import FreesideModel

@Suite struct SubmissionAttachmentStoreTests {
    @Test func theDiskStoreKeepsOneFilePerDigestBesideTheCache() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let bytes = Data("attached".utf8)
        let digest = TaskAttachmentDraft.digest(of: bytes)
        let store = DiskSubmissionAttachmentStore(directory: root)
        #expect(store.storedDigests().isEmpty)
        try store.save(digest: digest, bytes: bytes)

        let relaunched = DiskSubmissionAttachmentStore(directory: root)
        #expect(relaunched.storedDigests() == [digest])
        #expect(relaunched.load(digest: digest) == bytes)
        let file = root.appendingPathComponent("submission-attachments")
            .appendingPathComponent(String(digest.dropFirst("sha256:".count)))
        #expect(FileManager.default.fileExists(atPath: file.path))

        relaunched.delete(digest: digest)
        #expect(relaunched.storedDigests().isEmpty)
        #expect(relaunched.load(digest: digest) == nil)
    }

    @Test func theDiskStoreRefusesADigestThatCouldNameAnotherPath() {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let store = DiskSubmissionAttachmentStore(directory: root)
        #expect(throws: (any Error).self) { try store.save(digest: "sha256:../../cache.json", bytes: Data()) }
        #expect(store.load(digest: "sha256:../../cache.json") == nil)
    }
}
