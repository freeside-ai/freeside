import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// The composer's pre-upload rules (#2590): the daemon's limits, with
/// "image" decided from the bytes, and its name rules.
@Suite struct TaskAttachmentDraftTests {
    private static let pngSignature = Data([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A])

    private static func png(size: Int) -> Data {
        pngSignature + Data(count: size - pngSignature.count)
    }

    private static func file(size: Int) -> Data {
        Data(repeating: 0x61, count: size)
    }

    private func added(_ addition: TaskAttachmentDraft.Addition) -> DraftAttachment? {
        if case .added(let item) = addition { return item }
        return nil
    }

    private func refusal(_ addition: TaskAttachmentDraft.Addition) -> String? {
        if case .refused(let sentence) = addition { return sentence }
        return nil
    }

    @Test func anEmptyDraftBuildsNoAttachmentsField() {
        #expect(TaskAttachmentDraft().inputs == nil)
    }

    @Test func inputsKeepTheOrderAddedWithDigestNameAndClaim() throws {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "b.txt", claimedMediaType: "text/plain", bytes: Data("b".utf8))
        _ = draft.add(name: "a.png", claimedMediaType: "image/png", bytes: Self.png(size: 16))
        let inputs = try #require(draft.inputs)
        #expect(inputs.map(\.name) == ["b.txt", "a.png"])
        #expect(inputs.map(\.media_type) == ["text/plain", "image/png"])
        #expect(inputs[0].digest == TaskAttachmentDraft.digest(of: Data("b".utf8)))
        #expect(TaskAttachmentLimits.validDigest(inputs[0].digest))
    }

    @Test func aClaimTheDaemonWouldRefuseFallsBackToOctetStream() throws {
        var draft = TaskAttachmentDraft()
        let item = try #require(
            added(draft.add(name: "a", claimedMediaType: "text/plain; charset=utf-8", bytes: Data("a".utf8))))
        #expect(item.claimedMediaType == "application/octet-stream")
        let upper = try #require(added(draft.add(name: "b", claimedMediaType: "Image/PNG", bytes: Data("b".utf8))))
        #expect(upper.claimedMediaType == "image/png")
        let none = try #require(added(draft.add(name: "c", claimedMediaType: nil, bytes: Data("c".utf8))))
        #expect(none.claimedMediaType == "application/octet-stream")
    }

    @Test func anImageIsDecidedByItsSignatureNotItsName() throws {
        var draft = TaskAttachmentDraft()
        let image = try #require(added(draft.add(name: "x.bin", claimedMediaType: nil, bytes: Self.png(size: 16))))
        #expect(image.isImage)
        #expect(image.mediaType == .image_sol_png)
        let notImage = try #require(
            added(draft.add(name: "y.png", claimedMediaType: "image/png", bytes: Self.file(size: 16))))
        #expect(!notImage.isImage)
    }

    @Test func theImageLimitAppliesAtItsEdge() throws {
        var draft = TaskAttachmentDraft()
        #expect(added(draft.add(name: "at.png", claimedMediaType: nil, bytes: Self.png(size: 10 << 20))) != nil)
        let sentence = try #require(
            refusal(draft.add(name: "over.png", claimedMediaType: nil, bytes: Self.png(size: (10 << 20) + 1))))
        #expect(sentence == "over.png is 10.1 MiB; an image can be at most 10 MiB.")
    }

    @Test func aFileNamedLikeAnImageTakesTheFileLimit() {
        var draft = TaskAttachmentDraft()
        #expect(added(draft.add(name: "big.png", claimedMediaType: nil, bytes: Self.file(size: 11 << 20))) != nil)
    }

    @Test func theFileLimitAppliesAtItsEdge() throws {
        var draft = TaskAttachmentDraft()
        #expect(added(draft.add(name: "at.bin", claimedMediaType: nil, bytes: Self.file(size: 32 << 20))) != nil)
        let sentence = try #require(
            refusal(draft.add(name: "over.bin", claimedMediaType: nil, bytes: Self.file(size: (32 << 20) + 1))))
        #expect(sentence == "over.bin is 32.1 MiB; a file can be at most 32 MiB.")
    }

    @Test func theTaskTotalAppliesAtItsEdge() throws {
        var draft = TaskAttachmentDraft()
        for index in 0..<3 {
            _ = draft.add(name: "f\(index)", claimedMediaType: nil, bytes: Self.file(size: 32 << 20))
        }
        #expect(added(draft.add(name: "rest", claimedMediaType: nil, bytes: Self.file(size: 4 << 20))) != nil)
        #expect(draft.totalBytes == TaskAttachmentLimits.maxTotalBytes)
        let sentence = try #require(refusal(draft.add(name: "one", claimedMediaType: nil, bytes: Data("1".utf8))))
        #expect(sentence == "one would bring this task's attachments to 100.1 MiB; they can total at most 100 MiB.")
        #expect(draft.items.count == 4)
    }

    @Test func theCountLimitAppliesAtItsEdge() throws {
        var draft = TaskAttachmentDraft()
        for index in 0..<TaskAttachmentLimits.maxCount {
            _ = draft.add(name: "f\(index)", claimedMediaType: nil, bytes: Data("\(index)".utf8))
        }
        #expect(draft.items.count == TaskAttachmentLimits.maxCount)
        let sentence = try #require(refusal(draft.add(name: "last", claimedMediaType: nil, bytes: Data())))
        #expect(sentence.hasPrefix("last wasn't added: a task can have at most "))
    }

    @Test func aRepeatedNameIsNumberedBeforeItsExtension() {
        var draft = TaskAttachmentDraft()
        let names = ["image.png", "image.png", "image.png", "README", "README", ".env", ".env", "a.tar.gz", "a.tar.gz"]
        for (index, name) in names.enumerated() {
            _ = draft.add(name: name, claimedMediaType: nil, bytes: Data("\(index)".utf8))
        }
        #expect(
            draft.items.map(\.name) == [
                "image.png", "image 2.png", "image 3.png", "README", "README 2", ".env", ".env 2", "a.tar.gz",
                "a.tar 2.gz",
            ])
    }

    @Test func aNameTheDaemonRefusesIsRefused() throws {
        var draft = TaskAttachmentDraft()
        for name in ["", ".", "..", "a\u{7}b", "a/b", String(repeating: "x", count: 256)] {
            let sentence = try #require(refusal(draft.add(name: name, claimedMediaType: nil, bytes: Data("a".utf8))))
            #expect(sentence.contains("can't be attached"))
        }
        #expect(draft.isEmpty)
    }

    @Test func removeDropsOnlyTheNamedItem() {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "a", claimedMediaType: nil, bytes: Data("a".utf8))
        _ = draft.add(name: "b", claimedMediaType: nil, bytes: Data("b".utf8))
        draft.remove(name: "a")
        #expect(draft.items.map(\.name) == ["b"])
        draft.remove(name: "b")
        #expect(draft.inputs == nil)
    }

    @Test func mebibytesRoundUp() {
        #expect(TaskAttachmentDraft.mebibytes(10 << 20) == "10 MiB")
        #expect(TaskAttachmentDraft.mebibytes((10 << 20) + 1) == "10.1 MiB")
        #expect(TaskAttachmentDraft.mebibytes(1) == "0.1 MiB")
    }
}
