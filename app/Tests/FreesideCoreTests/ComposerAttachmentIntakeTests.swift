import CoreGraphics
import Foundation
import FreesideAPI
import FreesideModel
import ImageIO
import Testing
import UniformTypeIdentifiers

@testable import FreesideCore

#if os(macOS)
    import AppKit
#endif

@Suite struct ComposerAttachmentIntakeTests {
    private func temporaryDirectory() throws -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    /// A 4×4 opaque image encoded as `type`, with `properties` as its
    /// metadata.
    private func image(_ type: UTType, properties: [CFString: Any] = [:]) throws -> Data {
        let context = try #require(
            CGContext(
                data: nil, width: 4, height: 4, bitsPerComponent: 8, bytesPerRow: 0,
                space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue))
        context.setFillColor(CGColor(red: 0.2, green: 0.4, blue: 0.6, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: 4, height: 4))
        let output = NSMutableData()
        let destination = try #require(
            CGImageDestinationCreateWithData(output as CFMutableData, type.identifier as CFString, 1, nil))
        CGImageDestinationAddImage(destination, try #require(context.makeImage()), properties as CFDictionary)
        try #require(CGImageDestinationFinalize(destination))
        return output as Data
    }

    private func file(at url: URL) -> ComposerAttachmentIntake.Item {
        ComposerAttachmentIntake.file(at: url, repository: nil, held: TaskAttachmentDraft().usage)
    }

    @Test func aPackageIsRefusedByName() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let package = root.appendingPathComponent("Tool.app", isDirectory: true)
        try FileManager.default.createDirectory(at: package, withIntermediateDirectories: true)
        #expect(
            file(at: package)
                == .refused("Tool.app is a package, such as an app or a document bundle, and can't be attached."))
    }

    /// A spec approval's Discuss takes no folders (#2995), on either
    /// platform.
    @Test func aComposerThatTakesNoFoldersRefusesOne() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let folder = root.appendingPathComponent("screens", isDirectory: true)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        try Data("a".utf8).write(to: folder.appendingPathComponent("a.txt"))
        let items = ComposerAttachmentIntake.files(
            at: [folder, folder.appendingPathComponent("a.txt")], repository: nil,
            held: TaskAttachmentDraft(for: .message).usage, takesFolders: false)
        #expect(
            items == [
                .refused("screens is a folder. A message attaches files, not folders."),
                .attachment(name: "a.txt", claimedMediaType: "text/plain", bytes: Data("a".utf8)),
            ])
    }

    #if os(macOS)
        @Test func aFolderIsWalkedOnTheMac() throws {
            let root = try temporaryDirectory()
            defer { try? FileManager.default.removeItem(at: root) }
            let folder = root.appendingPathComponent("screens", isDirectory: true)
            try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
            try Data("a".utf8).write(to: folder.appendingPathComponent("a.txt"))
            #expect(
                file(at: folder)
                    == .folder(
                        name: "screens",
                        files: [FolderFile(path: "a.txt", claimedMediaType: "text/plain", bytes: Data("a".utf8))],
                        skippedLinks: []))
        }
    #endif

    @Test func aFileOverTheFileLimitIsRefusedFromItsSize() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let url = root.appendingPathComponent("dump.bin")
        // A sparse file: its size is past the limit without writing 32 MiB.
        #expect(FileManager.default.createFile(atPath: url.path, contents: nil))
        let handle = try FileHandle(forWritingTo: url)
        try handle.truncate(atOffset: UInt64(TaskAttachmentLimits.maxFileBytes + 1))
        try handle.close()
        #expect(
            file(at: url) == .refused("dump.bin is 32.1 MiB; a file can be at most 32 MiB."))
    }

    @Test func aFileKeepsItsNameAndClaimsItsType() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let url = root.appendingPathComponent("crash.log")
        let bytes = Data("panic".utf8)
        try bytes.write(to: url)
        guard case .attachment(let name, let claim, let read) = file(at: url) else {
            Issue.record("crash.log wasn't read")
            return
        }
        #expect(name == "crash.log")
        #expect(claim == "text/plain")
        #expect(read == bytes)
    }

    @Test func aPastedTIFFBecomesAPNG() throws {
        let item = ComposerAttachmentIntake.image(try image(.tiff), origin: .pasted)
        guard case .attachment(let name, let claim, let bytes) = item else {
            Issue.record("the TIFF wasn't re-encoded")
            return
        }
        #expect(name == "Pasted image.png")
        #expect(claim == "image/png")
        #expect(TaskAttachmentLimits.sniff(bytes) == .image_sol_png)
    }

    @Test func aHEICPhotoBecomesAJPEG() throws {
        let heic = try image(.heic)
        #expect(TaskAttachmentLimits.sniff(heic) == .application_sol_octet_hyphen_stream)
        guard case .attachment(let name, _, let bytes) = ComposerAttachmentIntake.image(heic, origin: .photo) else {
            Issue.record("the HEIC wasn't re-encoded")
            return
        }
        #expect(name == "Photo.jpg")
        #expect(TaskAttachmentLimits.sniff(bytes) == .image_sol_jpeg)
    }

    @Test func aReencodedPhotoKeepsItsOrientationAndDropsItsLocation() throws {
        let heic = try image(
            .heic,
            properties: [
                kCGImagePropertyOrientation: 6,
                kCGImagePropertyGPSDictionary: [
                    kCGImagePropertyGPSLatitude: 37.0, kCGImagePropertyGPSLatitudeRef: "N",
                ],
            ])
        guard case .attachment(_, _, let bytes) = ComposerAttachmentIntake.image(heic, origin: .photo) else {
            Issue.record("the HEIC wasn't re-encoded")
            return
        }
        let source = try #require(CGImageSourceCreateWithData(bytes as CFData, nil))
        let properties = try #require(CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any])
        #expect(properties[kCGImagePropertyOrientation] as? Int == 6)
        #expect(properties[kCGImagePropertyGPSDictionary] == nil)
    }

    @Test func anImageTheDaemonSniffsKeepsItsBytes() throws {
        let png = try image(.png)
        #expect(
            ComposerAttachmentIntake.image(png, origin: .photo)
                == .attachment(name: "Photo.png", claimedMediaType: "image/png", bytes: png))
    }

    @Test func bytesThatArentAnImageAreRefused() {
        #expect(
            ComposerAttachmentIntake.image(Data("not an image".utf8), origin: .pasted)
                == .refused("The pasted image couldn't be read as an image."))
    }

    #if os(macOS)
        /// A pasteboard in memory, so the paste policy is checked without a
        /// window-server session (the real-pasteboard tests below need one).
        private struct MemoryPasteboard: PasteboardContents {
            var urls: [URL] = []
            var text: String?
            var data: [NSPasteboard.PasteboardType: Data] = [:]

            /// Honors `.urlReadingFileURLsOnly` as `NSPasteboard` does, so a
            /// caller that drops it reads a web address as a file.
            func readObjects(forClasses: [AnyClass], options: [NSPasteboard.ReadingOptionKey: Any]?) -> [Any]? {
                options?[.urlReadingFileURLsOnly] as? Bool == true ? urls.filter(\.isFileURL) : urls
            }
            func string(forType type: NSPasteboard.PasteboardType) -> String? { type == .string ? text : nil }
            func data(forType type: NSPasteboard.PasteboardType) -> Data? { data[type] }
        }

        @Test @MainActor func thePastePolicyHoldsWithoutAWindowServer() throws {
            let png = try image(.png)
            #expect(PromptTextView.attachment(from: MemoryPasteboard(text: "plain words")) == nil)

            // Text beside an image is text on a paste and the image on a drag.
            let textAndImage = MemoryPasteboard(text: "plain words", data: [.png: png])
            #expect(PromptTextView.attachment(from: textAndImage) == nil)
            #expect(PromptTextView.attachment(from: textAndImage, textWins: false) == .image(png))

            #expect(PromptTextView.attachment(from: MemoryPasteboard(data: [.png: png])) == .image(png))

            let file = URL(fileURLWithPath: "/tmp/crash.log")
            #expect(PromptTextView.attachment(from: MemoryPasteboard(urls: [file])) == .files([file]))

            // A web address is no file to attach.
            let page = try #require(URL(string: "https://example.com/crash.log"))
            #expect(PromptTextView.attachment(from: MemoryPasteboard(urls: [page])) == nil)
        }

        @Test @MainActor func textOnThePasteboardStillPastesAsText() throws {
            let pasteboard = NSPasteboard(name: .init(UUID().uuidString))
            defer { pasteboard.releaseGlobally() }
            pasteboard.clearContents()
            pasteboard.setString("plain words", forType: .string)
            #expect(PromptTextView.attachment(from: pasteboard) == nil)

            // Text beside an image, as a rich-text copy carries, is text.
            let png = try image(.png)
            pasteboard.setData(png, forType: .png)
            #expect(PromptTextView.attachment(from: pasteboard) == nil)

            // A drag of the same is the image: a browser's image drag
            // carries its address as text.
            #expect(PromptTextView.attachment(from: pasteboard, textWins: false) == .image(png))
        }

        @Test @MainActor func anImageOrFileOnThePasteboardAttaches() throws {
            let pasteboard = NSPasteboard(name: .init(UUID().uuidString))
            defer { pasteboard.releaseGlobally() }
            pasteboard.clearContents()
            let png = try image(.png)
            pasteboard.setData(png, forType: .png)
            #expect(PromptTextView.attachment(from: pasteboard) == .image(png))

            pasteboard.clearContents()
            let file = URL(fileURLWithPath: "/tmp/crash.log")
            pasteboard.writeObjects([file as NSURL])
            #expect(PromptTextView.attachment(from: pasteboard) == .files([file]))
        }
    #endif
}
