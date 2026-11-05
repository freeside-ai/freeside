import Foundation

/// The mock's mirror of the daemon's task-attachment rules (domain
/// task_attachment.go, owner decision 2026-11-04), so a client developed
/// against the mock meets the refusals the daemon gives. The New Task
/// composer reuses the public members to refuse an attachment before
/// uploading it; that check only saves a wasted upload, and the daemon
/// stays the enforcer of every rule.
public enum TaskAttachmentLimits {
    public static let maxImageBytes = 10 << 20
    public static let maxFileBytes = 32 << 20
    public static let maxTotalBytes = 100 << 20
    public static let maxCount = 1000
    public static let maxSegmentBytes = 255
    public static let maxNameBytes = 1024
    static let maxClaimBytes = 255

    /// A sha256 digest in the daemon's strict form.
    public static func validDigest(_ digest: String) -> Bool {
        guard digest.hasPrefix("sha256:") else { return false }
        let hex = digest.utf8.dropFirst("sha256:".count)
        return hex.count == 64 && hex.allSatisfy { (0x30...0x39).contains($0) || (0x61...0x66).contains($0) }
    }

    /// A relative path of "/"-separated segments: at most 1,024 bytes, no
    /// empty, ".", or ".." segment, no leading "/", no control character (the
    /// daemon's unicode.IsControl is the Cc category), each segment at most 255
    /// bytes. It works on UTF-8 bytes, as the daemon does: a Character can
    /// join "/" to a combining mark, which would hide the separator.
    public static func validName(_ name: String) -> Bool {
        let bytes = Array(name.utf8)
        guard !bytes.isEmpty, bytes.count <= maxNameBytes, bytes.first != slash else { return false }
        guard !name.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) else {
            return false
        }
        return bytes.split(separator: slash, omittingEmptySubsequences: false).allSatisfy { segment in
            !segment.isEmpty && !segment.elementsEqual([dot]) && !segment.elementsEqual([dot, dot])
                && segment.count <= maxSegmentBytes
        }
    }

    /// Names that cannot coexist, compared by UTF-8 bytes as the daemon
    /// compares them (Swift String equality is canonical equivalence): a
    /// repeated name, or a name that is a directory of another.
    static func namesConflict(_ names: [String]) -> Bool {
        let byteNames = names.map { Array($0.utf8) }
        let set = Set(byteNames)
        guard set.count == byteNames.count else { return true }
        return byteNames.contains { name in
            name.indices.contains { end in name[end] == slash && set.contains(Array(name[..<end])) }
        }
    }

    private static let slash = UInt8(ascii: "/")
    private static let dot = UInt8(ascii: ".")

    /// A lowercase type/subtype pair of RFC 2045 token characters with no
    /// parameters, at most 255 bytes.
    public static func validClaim(_ claim: String) -> Bool {
        guard claim.utf8.count <= maxClaimBytes else { return false }
        let parts = claim.split(separator: "/", omittingEmptySubsequences: false)
        let specials = Set("()<>@,;:\\\"/[]?=".utf8)
        return parts.count == 2
            && parts.allSatisfy { part in
                !part.isEmpty
                    && part.utf8.allSatisfy { byte in
                        byte > 0x20 && byte < 0x7F && !(0x41...0x5A).contains(byte) && !specials.contains(byte)
                    }
            }
    }

    /// The daemon's media type from leading bytes: the four image
    /// signatures net/http.DetectContentType recognizes, else octet-stream.
    public static func sniff(_ bytes: Data) -> Components.Schemas.TaskAttachmentMediaType {
        let head = [UInt8](bytes.prefix(14))
        func starts(_ prefix: [UInt8]) -> Bool { head.starts(with: prefix) }
        if starts([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A]) { return .image_sol_png }
        if starts([0xFF, 0xD8, 0xFF]) { return .image_sol_jpeg }
        if starts(Array("GIF87a".utf8)) || starts(Array("GIF89a".utf8)) { return .image_sol_gif }
        if head.count == 14, starts(Array("RIFF".utf8)), head[8...].elementsEqual("WEBPVP".utf8) {
            return .image_sol_webp
        }
        return .application_sol_octet_hyphen_stream
    }
}
