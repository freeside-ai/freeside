import Foundation
import Testing

@testable import FreesideAPI
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

    // MARK: Folders (#2591)

    private func folderAdded(_ addition: TaskAttachmentDraft.FolderAddition) -> (String, [DraftAttachment])? {
        if case .added(let folder, let items) = addition { return (folder, items) }
        return nil
    }

    private func folderRefusal(_ addition: TaskAttachmentDraft.FolderAddition) -> String? {
        if case .refused(let sentence) = addition { return sentence }
        return nil
    }

    private static func folderFile(_ path: String, _ bytes: Data = Data("x".utf8)) -> FolderFile {
        FolderFile(path: path, claimedMediaType: "text/plain", bytes: bytes)
    }

    @Test func aFolderAddsEveryFileUnderItsName() throws {
        var draft = TaskAttachmentDraft()
        let (folder, items) = try #require(
            folderAdded(
                draft.addFolder(
                    name: "design",
                    files: [Self.folderFile("a.txt"), Self.folderFile("screens/home.png", Self.png(size: 16))])))
        #expect(folder == "design")
        #expect(items.map(\.name) == ["design/a.txt", "design/screens/home.png"])
        #expect(draft.inputs?.map(\.name) == ["design/a.txt", "design/screens/home.png"])
        #expect(items[1].isImage)
        #expect(!TaskAttachmentLimits.namesConflict(draft.items.map(\.name)))
    }

    @Test func aFolderOverTheCountIsRefusedWholeCountingTheDraft() {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "held.txt", claimedMediaType: nil, bytes: Data("h".utf8))
        let files = (0..<TaskAttachmentLimits.maxCount).map { Self.folderFile("f\($0)") }
        let sentence = folderRefusal(draft.addFolder(name: "logs", files: files))
        #expect(
            sentence
                == "logs wasn't added: it would bring this task to more than 1,000 attachments, the most a task can have."
        )
        #expect(draft.items.map(\.name) == ["held.txt"])
    }

    @Test func aFolderOverTheTotalIsRefusedWhole() {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "held.bin", claimedMediaType: nil, bytes: Self.file(size: 20 << 20))
        let files = (0..<3).map { Self.folderFile("part\($0)", Self.file(size: 30 << 20)) }
        let totalRefusal =
            "dump wasn't added: it would bring this task's attachments to more than 100 MiB, the most they can total."
        #expect(folderRefusal(draft.addFolder(name: "dump", files: files)) == totalRefusal)
        #expect(draft.items.count == 1)
        // The same check from sizes alone, before any file is read.
        #expect(
            TaskAttachmentDraft.folderRefusal(name: "dump", adding: .init(count: 3, bytes: 90 << 20), to: draft.usage)
                == totalRefusal)
        #expect(
            TaskAttachmentDraft.folderRefusal(name: "dump", adding: .init(count: 3, bytes: 80 << 20), to: draft.usage)
                == nil)
    }

    @Test func aFolderWithAnOversizedImageIsRefusedWholeNamingTheFile() {
        var draft = TaskAttachmentDraft()
        let sentence = folderRefusal(
            draft.addFolder(
                name: "design",
                files: [
                    Self.folderFile("ok.txt"),
                    Self.folderFile("screens/big.png", Self.png(size: TaskAttachmentLimits.maxImageBytes + 1)),
                ]))
        #expect(sentence == "design wasn't added: design/screens/big.png is 10.1 MiB; an image can be at most 10 MiB.")
        #expect(draft.isEmpty)
    }

    @Test func aFolderWithAPathTheDaemonRefusesIsRefusedWhole() throws {
        var draft = TaskAttachmentDraft()
        let sentence = try #require(
            folderRefusal(draft.addFolder(name: "design", files: [Self.folderFile("ok"), Self.folderFile("bad\u{7}")])))
        #expect(sentence.hasPrefix("design wasn't added: design/bad\u{7} can't be attached: a path must be at most"))
        let long = String(repeating: "a", count: 256)
        #expect(folderRefusal(draft.addFolder(name: "design", files: [Self.folderFile(long)])) != nil)
        #expect(draft.isEmpty)
    }

    @Test func aFolderWithNoFilesIsRefused() {
        var draft = TaskAttachmentDraft()
        #expect(folderRefusal(draft.addFolder(name: "empty", files: [])) == "empty has no files to attach.")
    }

    @Test func aFolderAndAFileNeverShareAName() throws {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "design", claimedMediaType: nil, bytes: Data("d".utf8))
        let first = try #require(folderAdded(draft.addFolder(name: "design", files: [Self.folderFile("a")])))
        #expect(first.0 == "design 2")
        let second = try #require(folderAdded(draft.addFolder(name: "design", files: [Self.folderFile("a")])))
        #expect(second.0 == "design 3")
        // A single file never takes a folder's name.
        let file = try #require(added(draft.add(name: "design", claimedMediaType: nil, bytes: Data("f".utf8))))
        #expect(file.name == "design 4")
        // A folder's number goes at the end, not before a dot.
        _ = draft.addFolder(name: "handoff.v1", files: [Self.folderFile("a")])
        let dotted = try #require(folderAdded(draft.addFolder(name: "handoff.v1", files: [Self.folderFile("a")])))
        #expect(dotted.0 == "handoff.v1 2")
        #expect(!TaskAttachmentLimits.namesConflict(draft.items.map(\.name)))
    }

    @Test func removingAFolderRemovesEveryFileInIt() {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "a.txt", claimedMediaType: nil, bytes: Data("a".utf8))
        _ = draft.addFolder(name: "design", files: [Self.folderFile("x"), Self.folderFile("sub/y")])
        _ = draft.addFolder(name: "design 2", files: [Self.folderFile("z")])
        draft.removeFolder(name: "design")
        #expect(draft.items.map(\.name) == ["a.txt", "design 2/z"])
    }

    @Test func chipsListFilesAndFoldersInTheOrderAdded() {
        var draft = TaskAttachmentDraft()
        _ = draft.add(name: "a.txt", claimedMediaType: nil, bytes: Data("a".utf8))
        _ = draft.addFolder(name: "design", files: [Self.folderFile("x"), Self.folderFile("sub/y", Data("yy".utf8))])
        _ = draft.add(name: "b.txt", claimedMediaType: nil, bytes: Data("b".utf8))
        let chips = draft.chips.map { chip -> String in
            switch chip {
            case .file(let name, _): name
            case .folder(let name, let items): "\(name)[\(items.map(\.name).joined(separator: ","))]"
            }
        }
        #expect(chips == ["a.txt", "design[design/x,design/sub/y]", "b.txt"])
    }

    @Test func groupingKeepsAFolderAtItsFirstFileEvenWhenItsFilesAreApart() {
        let groups = AttachmentGroup.grouped(["d/1", "a", "d/2", "e/1"], name: { $0 })
        #expect(groups.map(\.id) == ["folder:d", "file:a", "folder:e"])
        if case .folder(_, let items) = groups[0] { #expect(items == ["d/1", "d/2"]) }
    }
}
