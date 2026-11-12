import CoreGraphics
import Foundation
import FreesideAPI
import FreesideModel
import ImageIO
import UniformTypeIdentifiers

/// Turns what the operator drops, pastes, or picks into the New Task
/// composer into named bytes the draft can check (#2590). A file keeps its
/// base name; an image with no name gets one from its kind and format
/// (`Pasted image.png`, `Photo.jpg`). Image bytes the daemon wouldn't call
/// an image (HEIC, TIFF) are re-encoded first, so the daemon sees an image
/// and the task page can show it. On the Mac a folder goes to
/// `ComposerFolderIntake` (#2591); on iPhone it is refused. A package (an
/// app, a Keynote document) is refused on both. A composer that takes no
/// folders (a spec approval's Discuss, #2995) refuses one on both, before
/// reading it.
enum ComposerAttachmentIntake {
    /// Named bytes ready for `TaskAttachmentDraft.add`, a folder's files
    /// ready for `addFolder`, a repository path to insert into the task
    /// text, or a sentence saying why the operator's item can't be attached.
    enum Item: Equatable, Sendable {
        case attachment(name: String, claimedMediaType: String?, bytes: Data)
        /// A folder's regular files, with the paths of the symlinks the walk
        /// left out.
        case folder(name: String, files: [FolderFile], skippedLinks: [String])
        /// A folder inside the project's repository, by its path there with
        /// a trailing `/`: the agent already has it, so nothing uploads.
        case repositoryPath(String)
        case refused(String)
    }

    /// What the Mac text view hands over from a paste or a drop instead of
    /// inserting it as text.
    enum Pasted: Equatable {
        case files([URL])
        case image(Data)
    }

    /// Where an unnamed image came from, which names it.
    enum ImageOrigin: Sendable {
        case pasted
        case photo

        var baseName: String {
            switch self {
            case .pasted: "Pasted image"
            case .photo: "Photo"
            }
        }
    }

    /// Reads a file or folder the operator dropped, pasted, or picked. A
    /// file over the larger (file) limit is refused before its bytes are
    /// read. `repository` (the project's `owner/repo`) and `held` (what the
    /// composer and the batch so far take) decide a folder; a file ignores
    /// them.
    static func file(
        at url: URL, repository: String?, held: TaskAttachmentDraft.Usage, takesFolders: Bool = true
    ) -> Item {
        let accessing = url.startAccessingSecurityScopedResource()
        defer { if accessing { url.stopAccessingSecurityScopedResource() } }
        let name = url.lastPathComponent
        let values = try? url.resourceValues(forKeys: [
            .isDirectoryKey, .isPackageKey, .fileSizeKey, .contentTypeKey,
        ])
        if values?.isPackage == true {
            return .refused("\(name) is a package, such as an app or a document bundle, and can't be attached.")
        }
        if values?.isDirectory == true {
            guard takesFolders else { return .refused(noFoldersSentence(name)) }
            #if os(macOS)
                return ComposerFolderIntake.item(at: url, repository: repository, held: held)
            #else
                return .refused("\(name) is a folder. Folders attach on the Mac.")
            #endif
        }
        if let size = values?.fileSize,
            let refusal = TaskAttachmentDraft.oversizedFileRefusal(name: name, sizeBytes: size)
        {
            return .refused(refusal)
        }
        guard let bytes = try? Data(contentsOf: url) else {
            return .refused("\(name) couldn't be read.")
        }
        return .attachment(name: name, claimedMediaType: claim(for: url, values?.contentType), bytes: bytes)
    }

    /// Reads `urls` in order, counting each item read against the limits
    /// of the next, so a batch of folders can't together read past the
    /// task's count or total before the draft refuses them.
    static func files(
        at urls: [URL], repository: String?, held: TaskAttachmentDraft.Usage, takesFolders: Bool = true
    ) -> [Item] {
        var held = held
        return urls.map { url in
            let item = file(at: url, repository: repository, held: held, takesFolders: takesFolders)
            held = taking(item, held)
            return item
        }
    }

    /// `held` plus what `item` would add to the draft.
    static func taking(_ item: Item, _ held: TaskAttachmentDraft.Usage) -> TaskAttachmentDraft.Usage {
        switch item {
        case .attachment(_, _, let bytes):
            TaskAttachmentDraft.Usage(count: held.count + 1, bytes: held.bytes + bytes.count)
        case .folder(_, let files, _):
            TaskAttachmentDraft.Usage(
                count: held.count + files.count, bytes: held.bytes + files.reduce(0) { $0 + $1.bytes.count })
        case .repositoryPath, .refused:
            held
        }
    }

    /// The refusal for a folder offered to a composer that takes none.
    static func noFoldersSentence(_ name: String) -> String {
        "\(name) is a folder. A message attaches files, not folders."
    }

    /// The sentence under the composer for a folder attached without its
    /// symlinks; nil when it had none.
    static func skippedLinksSentence(folder: String, links: [String]) -> String? {
        guard let first = links.first else { return nil }
        let count = links.count == 1 ? "1 symbolic link wasn't" : "\(links.count) symbolic links weren't"
        return "\(folder): \(count) attached, such as \(first)."
    }

    /// The file type's MIME type; a text type with none of its own (a
    /// `.log`) claims `text/plain`.
    static func claim(for url: URL, _ contentType: UTType?) -> String? {
        guard let type = contentType ?? UTType(filenameExtension: url.pathExtension) else { return nil }
        return type.preferredMIMEType ?? (type.conforms(to: .text) ? "text/plain" : nil)
    }

    /// Names image bytes with no name of their own, re-encoding a format the
    /// daemon doesn't sniff as an image: pasted images as PNG, which keeps
    /// a screenshot's pixels exact, photos as JPEG.
    static func image(_ bytes: Data, origin: ImageOrigin) -> Item {
        var bytes = bytes
        var mediaType = TaskAttachmentLimits.sniff(bytes)
        if mediaType == .application_sol_octet_hyphen_stream {
            let target: UTType = origin == .photo ? .jpeg : .png
            guard let encoded = reencode(bytes, as: target) else {
                return .refused("The \(origin == .photo ? "photo" : "pasted image") couldn't be read as an image.")
            }
            bytes = encoded
            mediaType = TaskAttachmentLimits.sniff(bytes)
        }
        let ext: String
        switch mediaType {
        case .image_sol_png: ext = "png"
        case .image_sol_jpeg: ext = "jpg"
        case .image_sol_gif: ext = "gif"
        case .image_sol_webp: ext = "webp"
        case .application_sol_octet_hyphen_stream: ext = "bin"
        case .unknown: ext = "bin"
        }
        return .attachment(name: "\(origin.baseName).\(ext)", claimedMediaType: mediaType.rawValue, bytes: bytes)
    }

    /// The first image in `bytes` encoded as `type`, or nil when ImageIO
    /// can't decode it. The source's metadata carries over, so a portrait
    /// photo keeps its orientation, except its location. A JPEG keeps 0.9
    /// quality.
    static func reencode(_ bytes: Data, as type: UTType) -> Data? {
        guard let source = CGImageSourceCreateWithData(bytes as CFData, nil),
            CGImageSourceGetCount(source) > 0
        else { return nil }
        let output = NSMutableData()
        guard
            let destination = CGImageDestinationCreateWithData(
                output as CFMutableData, type.identifier as CFString, 1, nil)
        else { return nil }
        var options: [CFString: Any] = [kCGImageMetadataShouldExcludeGPS: true]
        if type == .jpeg { options[kCGImageDestinationLossyCompressionQuality] = 0.9 }
        CGImageDestinationAddImageFromSource(destination, source, 0, options as CFDictionary)
        guard CGImageDestinationFinalize(destination) else { return nil }
        return output as Data
    }

    /// Reads dropped or pasted item providers in order: a file URL as a
    /// file, otherwise image data as an unnamed image. A file URL this app
    /// can't read (another app's file pasted on iPhone) falls back to the
    /// provider's image when it offers one. `repository` and `held` decide
    /// a folder, as `files(at:repository:held:)` says.
    static func items(
        from providers: [NSItemProvider], origin: ImageOrigin, repository: String?, held: TaskAttachmentDraft.Usage,
        takesFolders: Bool = true
    ) async -> [Item] {
        var items: [Item] = []
        var held = held
        for provider in providers {
            let hasImage = provider.hasItemConformingToTypeIdentifier(UTType.image.identifier)
            if provider.hasItemConformingToTypeIdentifier(UTType.fileURL.identifier),
                let url = await fileURL(from: provider), !hasImage || isReadable(url)
            {
                let item = file(at: url, repository: repository, held: held, takesFolders: takesFolders)
                held = taking(item, held)
                items.append(item)
            } else if hasImage {
                if let data = await data(from: provider, type: .image) {
                    items.append(image(data, origin: origin))
                } else {
                    items.append(.refused("The \(origin == .photo ? "photo" : "image") couldn't be read."))
                }
            }
        }
        return items
    }

    private static func isReadable(_ url: URL) -> Bool {
        let accessing = url.startAccessingSecurityScopedResource()
        defer { if accessing { url.stopAccessingSecurityScopedResource() } }
        return FileManager.default.isReadableFile(atPath: url.path)
    }

    private static func fileURL(from provider: NSItemProvider) async -> URL? {
        await withCheckedContinuation { continuation in
            _ = provider.loadObject(ofClass: URL.self) { url, _ in
                continuation.resume(returning: url?.isFileURL == true ? url : nil)
            }
        }
    }

    private static func data(from provider: NSItemProvider, type: UTType) async -> Data? {
        await withCheckedContinuation { continuation in
            _ = provider.loadDataRepresentation(forTypeIdentifier: type.identifier) { data, _ in
                continuation.resume(returning: data)
            }
        }
    }

    /// A small decoded copy of an image for its chip, at most `maxPixels`
    /// on the long side; nil for bytes ImageIO can't decode.
    static func thumbnail(_ bytes: Data, maxPixels: Int = 96) -> CGImage? {
        guard let source = CGImageSourceCreateWithData(bytes as CFData, nil) else { return nil }
        let options: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixels,
        ]
        return CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary)
    }
}
