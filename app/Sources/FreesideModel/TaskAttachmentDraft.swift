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

/// The composer's attachment list and the rules it checks before any upload
/// (#2590): the daemon's per-image, per-file, per-task, and count limits and
/// its name rules, all from `TaskAttachmentLimits`. "Image" means what the
/// daemon decides from the bytes, so a file named `.png` holding other bytes
/// takes the file limit. A repeated name is numbered, never refused. The
/// check only saves a wasted upload: the daemon still enforces every rule.
public struct TaskAttachmentDraft: Equatable, Sendable {
    public enum Addition: Equatable, Sendable {
        case added(DraftAttachment)
        /// One sentence that names the rule, ready to show the operator.
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
        let claim = claimedMediaType?.lowercased()
        let item = DraftAttachment(
            name: unique,
            claimedMediaType: claim.flatMap { TaskAttachmentLimits.validClaim($0) ? $0 : nil }
                ?? "application/octet-stream",
            bytes: bytes,
            digest: Self.digest(of: bytes),
            mediaType: mediaType)
        items.append(item)
        return .added(item)
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

    public mutating func remove(name: String) {
        items.removeAll { $0.name == name }
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

    /// `name`, or the first of `name 2`, `name 3`, ... the list doesn't hold,
    /// with the number before the extension (`image 2.png`). A leading dot
    /// starts the name, not an extension (`.env 2`). Names compare by UTF-8
    /// bytes, as the daemon compares them.
    func uniqueName(_ name: String) -> String {
        let taken = Set(items.map { Array($0.name.utf8) })
        guard taken.contains(Array(name.utf8)) else { return name }
        let stem: Substring
        let suffix: Substring
        if let dot = name.lastIndex(of: "."), dot != name.startIndex {
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
