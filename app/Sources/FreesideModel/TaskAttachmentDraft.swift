import Crypto
import Foundation
import FreesideAPI

/// One file or image the operator has added to the New Task composer, held
/// in memory until Submit saves its bytes beside the command.
public struct DraftAttachment: Equatable, Sendable, Identifiable {
    /// The name the task records, unique within its draft.
    public let name: String
    /// The client's claim, from the file's type; the daemon records it but
    /// decides the media type from the bytes.
    public let claimedMediaType: String
    public let bytes: Data
    /// `sha256:<hex>`, the address the upload and the command name.
    public let digest: String
    /// The media type the daemon will decide from the bytes.
    public let mediaType: Components.Schemas.TaskAttachmentMediaType

    public var id: String { name }
    public var sizeBytes: Int { bytes.count }
    /// Whether the daemon will treat the bytes as an image, and so apply
    /// the image limit and show it on the task page.
    public var isImage: Bool { mediaType != .application_sol_octet_hyphen_stream }
}

/// One file of a folder the operator attached, named by its path inside
/// the folder (`sub/file.png`), not yet checked against any limit.
public struct FolderFile: Equatable, Sendable {
    public let path: String
    public let claimedMediaType: String?
    public let bytes: Data

    public init(path: String, claimedMediaType: String?, bytes: Data) {
        self.path = path
        self.claimedMediaType = claimedMediaType
        self.bytes = bytes
    }
}

/// Attachments as the operator sees them: a single file, or the files of
/// one folder, which a task names `<folder>/<path>` (#2591). Single files
/// never contain `/`, so a name with `/` belongs to the folder its first
/// segment names.
public enum AttachmentGroup<Item>: Identifiable {
    case file(name: String, item: Item)
    case folder(name: String, items: [Item])

    /// Groups `items` in the order their file or folder first appears.
    public static func grouped(_ items: [Item], name: (Item) -> String) -> [AttachmentGroup<Item>] {
        // Folders take their place on first sight and their files after.
        var groups: [AttachmentGroup<Item>] = []
        var folders: [String: [Item]] = [:]
        for item in items {
            let full = name(item)
            guard let folder = TaskAttachmentDraft.folderName(of: full) else {
                groups.append(.file(name: full, item: item))
                continue
            }
            if folders[folder] == nil { groups.append(.folder(name: folder, items: [])) }
            folders[folder, default: []].append(item)
        }
        return groups.map { group in
            guard case .folder(let folder, _) = group else { return group }
            return .folder(name: folder, items: folders[folder] ?? [])
        }
    }

    /// A single file's name is never a folder's, but the prefix keeps the
    /// ids apart regardless.
    public var id: String {
        switch self {
        case .file(let name, _): "file:\(name)"
        case .folder(let name, _): "folder:\(name)"
        }
    }
}

/// The composer's attachment list and the rules it checks before any upload
/// (#2590): the daemon's per-image, per-file, per-task, and count limits and
/// its name rules, all from `TaskAttachmentLimits`. "Image" means what the
/// daemon decides from the bytes, so a file named `.png` holding other bytes
/// takes the file limit. A repeated name is numbered, never refused. The
/// check only saves a wasted upload: the daemon still enforces every rule.
///
/// A folder (#2591) adds one item per file, named `<folder>/<path>`, and
/// attaches whole or not at all: a tree missing a file the operator chose
/// would mislead the agent. A file and a folder never share a name, since
/// the daemon refuses a name that is also a folder of another.
public struct TaskAttachmentDraft: Equatable, Sendable {
    public enum Addition: Equatable, Sendable {
        case added(DraftAttachment)
        /// One sentence that names the rule, ready to show the operator.
        case refused(String)
    }

    public enum FolderAddition: Equatable, Sendable {
        /// The folder's name in the draft, numbered if another item held it.
        case added(folder: String, items: [DraftAttachment])
        case refused(String)
    }

    public private(set) var items: [DraftAttachment] = []

    public init() {}

    public var isEmpty: Bool { items.isEmpty }
    public var totalBytes: Int { items.reduce(0) { $0 + $1.sizeBytes } }

    /// Adds `bytes` under `name`, numbering the name if the list already
    /// holds it. `claimedMediaType` falls back to `application/octet-stream`
    /// when it isn't a bare lowercase type/subtype the daemon accepts.
    public mutating func add(name: String, claimedMediaType: String?, bytes: Data) -> Addition {
        guard TaskAttachmentLimits.validName(name), !name.contains("/") else {
            return .refused(Self.invalidNameSentence(name))
        }
        guard items.count < TaskAttachmentLimits.maxCount else {
            return .refused(
                "\(name) wasn't added: a task can have at most "
                    + "\(TaskAttachmentLimits.maxCount.formatted()) attachments.")
        }
        let mediaType = TaskAttachmentLimits.sniff(bytes)
        let isImage = mediaType != .application_sol_octet_hyphen_stream
        let limit = isImage ? TaskAttachmentLimits.maxImageBytes : TaskAttachmentLimits.maxFileBytes
        guard bytes.count <= limit else {
            return .refused(Self.sizeRefusal(name: name, sizeBytes: bytes.count, isImage: isImage))
        }
        let total = totalBytes + bytes.count
        guard total <= TaskAttachmentLimits.maxTotalBytes else {
            return .refused(
                "\(name) would bring this task's attachments to \(Self.mebibytes(total)); "
                    + "they can total at most \(Self.mebibytes(TaskAttachmentLimits.maxTotalBytes)).")
        }
        let unique = uniqueName(name)
        guard TaskAttachmentLimits.validName(unique) else {
            return .refused(Self.invalidNameSentence(unique))
        }
        let item = Self.item(name: unique, claimedMediaType: claimedMediaType, bytes: bytes)
        items.append(item)
        return .added(item)
    }

    private static func item(name: String, claimedMediaType: String?, bytes: Data) -> DraftAttachment {
        let claim = claimedMediaType?.lowercased()
        return DraftAttachment(
            name: name,
            claimedMediaType: claim.flatMap { TaskAttachmentLimits.validClaim($0) ? $0 : nil }
                ?? "application/octet-stream",
            bytes: bytes,
            digest: digest(of: bytes),
            mediaType: TaskAttachmentLimits.sniff(bytes))
    }

    /// The refusal for a file over even the file limit, which its size
    /// alone decides, so the composer can refuse it before reading it; nil
    /// when it fits.
    public static func oversizedFileRefusal(name: String, sizeBytes: Int) -> String? {
        sizeBytes > TaskAttachmentLimits.maxFileBytes
            ? sizeRefusal(name: name, sizeBytes: sizeBytes, isImage: false) : nil
    }

    private static func sizeRefusal(name: String, sizeBytes: Int, isImage: Bool) -> String {
        let limit = isImage ? TaskAttachmentLimits.maxImageBytes : TaskAttachmentLimits.maxFileBytes
        return "\(name) is \(mebibytes(sizeBytes)); \(isImage ? "an image" : "a file") can be at most "
            + "\(mebibytes(limit))."
    }

    /// Adds every file of the folder `name`, each named `<folder>/<path>`,
    /// numbering the folder (`design 2`) if a file or folder already holds
    /// its name. Any limit or name rule refuses the whole folder, with one
    /// sentence naming the file and the rule.
    public mutating func addFolder(name: String, files: [FolderFile]) -> FolderAddition {
        guard TaskAttachmentLimits.validName(name), !name.contains("/") else {
            return .refused(Self.invalidNameSentence(name))
        }
        guard !files.isEmpty else { return .refused("\(name) has no files to attach.") }
        if let refusal = Self.folderRefusal(
            name: name, adding: Usage(count: files.count, bytes: files.reduce(0) { $0 + $1.bytes.count }),
            to: usage)
        {
            return .refused(refusal)
        }
        let folder = uniqueName(name, splitsExtension: false)
        var added: [DraftAttachment] = []
        added.reserveCapacity(files.count)
        for file in files {
            let shown = "\(name)/\(file.path)"
            let mediaType = TaskAttachmentLimits.sniff(file.bytes)
            let isImage = mediaType != .application_sol_octet_hyphen_stream
            let limit = isImage ? TaskAttachmentLimits.maxImageBytes : TaskAttachmentLimits.maxFileBytes
            guard file.bytes.count <= limit else {
                return .refused(
                    "\(name) wasn't added: "
                        + Self.sizeRefusal(name: shown, sizeBytes: file.bytes.count, isImage: isImage))
            }
            let full = "\(folder)/\(file.path)"
            guard TaskAttachmentLimits.validName(full) else {
                return .refused(
                    "\(name) wasn't added: \(shown) can't be attached: a path must be at most "
                        + "\(TaskAttachmentLimits.maxNameBytes.formatted()) bytes, each part at most "
                        + "\(TaskAttachmentLimits.maxSegmentBytes), with no control characters, "
                        + "and no part \".\" or \"..\".")
            }
            added.append(Self.item(name: full, claimedMediaType: file.claimedMediaType, bytes: file.bytes))
        }
        items += added
        return .added(folder: folder, items: added)
    }

    /// How much of a task's count and total limits attachments take: what a
    /// draft holds, or that plus what a batch being read has already taken.
    public struct Usage: Equatable, Sendable {
        public var count: Int
        public var bytes: Int

        public init(count: Int, bytes: Int) {
            self.count = count
            self.bytes = bytes
        }
    }

    public var usage: Usage { Usage(count: items.count, bytes: totalBytes) }

    /// Why a folder adding `adding` to `held` can't join a task, or nil when
    /// the count and total fit. Sizes alone decide it, so the composer can
    /// refuse a folder before reading its files, and stop walking one as
    /// soon as it passes a limit; the sentence says "more than" because a
    /// walk that stopped early doesn't know the folder's full size.
    public static func folderRefusal(name: String, adding: Usage, to held: Usage) -> String? {
        if held.count + adding.count > TaskAttachmentLimits.maxCount {
            return "\(name) wasn't added: it would bring this task to more than "
                + "\(TaskAttachmentLimits.maxCount.formatted()) attachments, the most a task can have."
        }
        if held.bytes + adding.bytes > TaskAttachmentLimits.maxTotalBytes {
            return "\(name) wasn't added: it would bring this task's attachments to more than "
                + "\(mebibytes(TaskAttachmentLimits.maxTotalBytes)), the most they can total."
        }
        return nil
    }

    public mutating func remove(name: String) {
        items.removeAll { $0.name == name }
    }

    /// Removes every file of the folder `name`.
    public mutating func removeFolder(name: String) {
        items.removeAll { Self.folderName(of: $0.name) == name }
    }

    /// The composer's chips: each single file, and each folder once, in the
    /// order first added.
    public var chips: [AttachmentGroup<DraftAttachment>] {
        AttachmentGroup.grouped(items, name: \.name)
    }

    /// The folder a name sits in: its first segment when it has `/`.
    public static func folderName(of name: String) -> String? {
        guard let slash = name.firstIndex(of: "/") else { return nil }
        return String(name[..<slash])
    }

    /// The command's attachment list in the order added, or nil for an empty
    /// draft so a text-only command carries no `attachments` field.
    public var inputs: [Components.Schemas.TaskAttachmentInput]? {
        items.isEmpty
            ? nil
            : items.map { .init(digest: $0.digest, name: $0.name, media_type: $0.claimedMediaType) }
    }

    /// The bytes to save beside the command, by digest.
    public var bytesByDigest: [String: Data] {
        items.reduce(into: [:]) { $0[$1.digest] = $1.bytes }
    }

    public static func digest(of bytes: Data) -> String {
        "sha256:" + SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
    }

    /// `name`, or the first of `name 2`, `name 3`, ... no file or folder in
    /// the list holds, with a file's number before its extension
    /// (`image 2.png`) and a folder's at the end (`design.v1 2`). A leading
    /// dot starts the name, not an extension (`.env 2`). Taking folder names
    /// keeps a single file from becoming a folder of another name, which the
    /// daemon refuses. Names compare by UTF-8 bytes, as the daemon compares
    /// them.
    func uniqueName(_ name: String, splitsExtension: Bool = true) -> String {
        let taken = Set(
            items.flatMap { item in
                [Array(item.name.utf8)] + (Self.folderName(of: item.name).map { [Array($0.utf8)] } ?? [])
            })
        guard taken.contains(Array(name.utf8)) else { return name }
        let stem: Substring
        let suffix: Substring
        if splitsExtension, let dot = name.lastIndex(of: "."), dot != name.startIndex {
            stem = name[..<dot]
            suffix = name[dot...]
        } else {
            stem = name[...]
            suffix = ""
        }
        var number = 2
        while taken.contains(Array("\(stem) \(number)\(suffix)".utf8)) { number += 1 }
        return "\(stem) \(number)\(suffix)"
    }

    private static func invalidNameSentence(_ name: String) -> String {
        "\(name.isEmpty ? "A file with no name" : name) can't be attached: a name must be at most "
            + "\(TaskAttachmentLimits.maxSegmentBytes) bytes, with no control characters, and not \".\" or \"..\"."
    }

    /// Sizes in the limits' own unit, rounded up so a file one byte over a
    /// limit never reads as equal to it.
    static func mebibytes(_ bytes: Int) -> String {
        let tenths = (bytes * 10 + (1 << 20) - 1) >> 20
        return tenths % 10 == 0 ? "\(tenths / 10) MiB" : "\(tenths / 10).\(tenths % 10) MiB"
    }
}
